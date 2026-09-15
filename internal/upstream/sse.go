// CodeArts /v1/chat/chat SSE → OpenAI chat.completion 转换。
//
// 标准 SSE（event: <name> + data: <json>）。常见事件（逆向自 vscode-codebot）：
//   - onAnswer:  {"text":"增量","custom_extra_info":...}
//   - reasoning / thinking: 思考链增量
//   - done:      {"error_code":"0","error_msg":"","related_question_answer":[...],"response_message_id":"..."}
//                 related_question_answer 是「相关追问」建议，不是正文，解析时忽略。
//   - error / heartbeat 等
//
// 另：部分帧可能直接给出 structured QA 对象 {question,options,answer}（无 text 字段）。
// 仅在正文仍为空时提取 answer，避免把整段 JSON 当回复；已有 text 时绝不覆盖。
package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// UpstreamError 流内业务错误。
type UpstreamError struct {
	Code string
	Msg  string
}

func (e *UpstreamError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("codearts error code=%s msg=%s", e.Code, e.Msg)
	}
	return fmt.Sprintf("codearts error: %s", e.Msg)
}

// RawCompletion 聚合结果。
type RawCompletion struct {
	Content   string
	Reasoning string
	Finish    string
	ToolCalls []ChatToolCall
}

// scanLine 处理一行 SSE：CodeArts 是逐行 data:（无空行分隔），
// 兼容标准 event:/data: 空行分隔两种形态。
// 返回 (event, data, 是否触发事件)。event 为空表示无 event 前缀。
func scanLine(line string, pendingEvent *string) (event, data string, ok bool) {
	line = strings.TrimRight(line, "\r")
	switch {
	case strings.HasPrefix(line, "event:"):
		*pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		return "", "", false
	case strings.HasPrefix(line, "data:"):
		d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if d == "" {
			return "", "", false
		}
		ev := *pendingEvent
		*pendingEvent = ""
		return ev, d, true
	case strings.HasPrefix(line, ":"):
		// 注释
		return "", "", false
	case line == "":
		*pendingEvent = ""
		return "", "", false
	}
	return "", "", false
}

// isValidStructuredQA 检查是否为有效的结构化问答对象。
// 避免将代码片段或 other structured content误认为是问答结构。
func isValidStructuredQA(obj map[string]any) bool {
	if obj == nil {
		return false
	}
	// 检查是否包含必要的问答字段
	question, hasQ := obj["question"]
	answer, hasA := obj["answer"]
	_, hasOptions := obj["options"]

	// 必须同时有 question 和 answer 字段
	if !hasQ || !hasA {
		return false
	}

	// 确保字段是字符串类型
	qStr, qOk := question.(string)
	aStr, aOk := answer.(string)
	if !qOk || !aOk {
		return false
	}

	// 检查是否是典型的问答格式（避免代码块）
	// Question should be a natural language question
	if len(qStr) == 0 || len(qStr) > 500 { // 问题不应该太长
		return false
	}

	// Answer should be relatively short and not contain code-like structures
	if len(aStr) == 0 || len(aStr) > 100 { // 答案不应该太长
		return false
	}

	// Check if it looks like code (contains common programming keywords)
	lowerAns := strings.ToLower(aStr)
	if strings.Contains(lowerAns, "import ") || strings.Contains(lowerAns, "def ") ||
		strings.Contains(lowerAns, "class ") || strings.Contains(lowerAns, "func ") ||
		strings.Contains(lowerAns, "package ") || strings.Contains(lowerAns, "module ") ||
		strings.Contains(lowerAns, "struct ") || strings.Contains(lowerAns, "interface ") {
		return false
	}

	// If options are present, they should be a slice
	if hasOptions {
		opts, optsOk := obj["options"].([]any)
		if !optsOk {
			return false
		}
		// Options should be reasonable (not too many, not too long)
		if len(opts) > 10 || len(opts) < 2 {
			return false
		}
	}

	return true
}

// deltaFromChunk 从 OpenAI 兼容 chunk 中提取 delta 与 finish_reason。
// 标准格式：{"choices":[{"delta":{...},"finish_reason":"stop"}]}。
func deltaFromChunk(payload map[string]any) (delta map[string]any, finishReason string) {
	if d, ok := payload["delta"].(map[string]any); ok {
		return d, ""
	}
	choices, ok := payload["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, ""
	}
	if c0, ok := choices[0].(map[string]any); ok {
		if fr, ok := c0["finish_reason"].(string); ok {
			finishReason = fr
		}
		if d, ok := c0["delta"].(map[string]any); ok {
			delta = d
		}
	}
	return delta, finishReason
}

type toolCallAccumulator map[int]*ChatToolCall

// applyToolCallDeltas 聚合 OpenAI 流式 tool_calls 的 id/name/arguments 分片。
func applyToolCallDeltas(delta map[string]any, calls toolCallAccumulator) {
	rawCalls, ok := delta["tool_calls"].([]any)
	if !ok {
		return
	}
	for position, raw := range rawCalls {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		index := position
		switch v := m["index"].(type) {
		case float64:
			index = int(v)
		case int:
			index = v
		}
		call := calls[index]
		if call == nil {
			call = &ChatToolCall{Index: index, Type: "function"}
			calls[index] = call
		}
		if id, ok := m["id"].(string); ok && id != "" {
			call.ID = id
		}
		if typ, ok := m["type"].(string); ok && typ != "" {
			call.Type = typ
		}
		if fn, ok := m["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				call.Function.Name += name
			}
			if arguments, ok := fn["arguments"].(string); ok {
				call.Function.Arguments += arguments
			}
		}
	}
}

func sortedToolCalls(calls toolCallAccumulator) []ChatToolCall {
	indexes := make([]int, 0, len(calls))
	for index := range calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	out := make([]ChatToolCall, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, *calls[index])
	}
	return out
}

// applyEvent 把单事件应用到聚合状态。
func applyEvent(content, reason *strings.Builder, finish *string, upErr *error, event, data string) {
	event = strings.ToLower(strings.TrimSpace(event))
	var payload map[string]any
	if strings.TrimSpace(data) != "" {
		_ = json.Unmarshal([]byte(data), &payload)
	}
	text := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := payload[k].(string); ok {
				return v
			}
		}
		return ""
	}
	switch event {
	case "", "data":
		// OpenAI 标准 SSE 终止标记
		if strings.TrimSpace(data) == "[DONE]" {
			if *finish == "" {
				*finish = "stop"
			}
			return
		}
		// 纯 data 行（CodeArts 部分响应无 event 前缀）
		if code := text("error_code", "code"); code != "" && code != "0" {
			*upErr = &UpstreamError{Code: code, Msg: firstNonEmpty(text("error_msg", "message", "msg"), "request failed")}
			return
		}
		// OpenAI 风格 delta：{choices:[{delta:{content,reasoning_content}}]} → 增量追加
		delta, finishReason := deltaFromChunk(payload)
		if finishReason != "" {
			*finish = finishReason
		}
		if d := delta; d != nil {
			if c, ok := d["content"].(string); ok && c != "" {
				content.WriteString(c)
			}
			if r, ok := d["reasoning_content"].(string); ok && r != "" {
				reason.WriteString(r)
			}
			return
		}
		if _, hasUsage := payload["usage"].(map[string]any); hasUsage {
			// 纯 usage 终帧：{choices:[],usage:{...}} → 结束
			if *finish == "" {
				*finish = "stop"
			}
			return
		}
		if t := text("text", "content", "delta"); t != "" {
			if t == "[DONE]" || t == "[DONE] " || strings.EqualFold(t, "done") {
				*finish = "stop"
				return
			}
			// 实测：text 为全文快照（非增量）→ 替换
			content.Reset()
			content.WriteString(t)
			return
		}
		// 终态 output 数组：[{type:"output_text",text}]
		if out, ok := payload["output"].([]any); ok && len(out) > 0 {
			var sb strings.Builder
			for _, item := range out {
				if m, ok := item.(map[string]any); ok && m["type"] == "output_text" {
					if s, ok := m["text"].(string); ok {
						sb.WriteString(s)
					}
				}
			}
			if sb.Len() > 0 {
				content.Reset()
				content.WriteString(sb.String())
			}
			return
		}
		// 纯 structured QA 帧（无 text）：仅正文仍空时取 answer，避免整段 JSON 当回复
		tryFillStructuredAnswer(content, payload)
	case "onanswer", "answer", "delta", "message", "content":
		if code := text("error_code", "code"); code != "" && code != "0" {
			*upErr = &UpstreamError{Code: code, Msg: firstNonEmpty(text("error_msg", "message", "msg"), "request failed")}
			return
		}
		delta, finishReason := deltaFromChunk(payload)
		if finishReason != "" {
			*finish = finishReason
		}
		if d := delta; d != nil {
			if c, ok := d["content"].(string); ok && c != "" {
				content.WriteString(c)
			}
			if r, ok := d["reasoning_content"].(string); ok && r != "" {
				reason.WriteString(r)
			}
			return
		}
		if t := text("text", "content", "delta", "message"); t != "" {
			if t == "[DONE]" || t == "[DONE] " || strings.EqualFold(t, "done") {
				*finish = "stop"
				return
			}
			content.Reset()
			content.WriteString(t)
			return
		}
		tryFillStructuredAnswer(content, payload)
	case "reasoning", "thinking", "onreasoning", "onthinking":
		if t := text("text", "reasoning", "content", "thinking"); t != "" {
			reason.WriteString(t)
		}
	case "done", "end", "finish":
		if code := text("error_code", "code"); code != "" && code != "0" {
			*upErr = &UpstreamError{Code: code, Msg: firstNonEmpty(text("error_msg", "message", "msg"), "request failed")}
			return
		}
		if f := text("finish_reason", "finish", "reason"); f != "" {
			*finish = f
		} else {
			*finish = "stop"
		}
	}
}

// tryFillStructuredAnswer 尝试填充 structured QA（{question,options,answer} 对象）。
// 仅 content 为空时生效（避免把整段 JSON 当回复）；答案优先，直接取 answer。
func tryFillStructuredAnswer(content *strings.Builder, obj map[string]any) {
	if content.Len() > 0 || !isValidStructuredQA(obj) {
		return
	}
	if a, ok := obj["answer"].(string); ok {
		content.WriteString(a)
	}
}

// unwrapQAContent 移除可能的 ```markdown 或 ```json 等包装，
// 若内容本身是结构化问答 JSON，则直接提取 answer。
func unwrapQAContent(s string) string {
	trimmed := strings.TrimSpace(s)
	// 裸 JSON 字符串：模型把整段 QA JSON 写进 text
	if obj, ok := parseQAJSON(trimmed); ok {
		if a, ok := obj["answer"].(string); ok {
			return a
		}
		return s
	}
	if strings.HasPrefix(s, "```") {
		lines := strings.Split(trimmed, "\n")
		if len(lines) >= 3 {
			first := strings.ToLower(strings.TrimSpace(lines[0]))
			last := strings.ToLower(strings.TrimSpace(lines[len(lines)-1]))
			if (strings.HasPrefix(first, "```markdown") || strings.HasPrefix(first, "```json") || strings.HasPrefix(first, "```")) && last == "```" {
				content := strings.Join(lines[1:len(lines)-1], "\n")
				if obj, ok := parseQAJSON(content); ok {
					if ans, ok := obj["answer"].(string); ok {
						return ans
					}
				}
				return content
			}
		}
		return s
	}
	return s
}

// parseQAJSON 尝试把字符串解析为结构化问答 JSON 对象。
func parseQAJSON(s string) (map[string]any, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, false
	}
	if !isValidStructuredQA(obj) {
		return nil, false
	}
	return obj, true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// AggregateRaw 读取完整 SSE 并聚合。
func AggregateRaw(r io.Reader) (*RawCompletion, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		content strings.Builder
		reason  strings.Builder
		finish  = "stop"
		upErr   error
		calls   = make(toolCallAccumulator)
	)
	var pendingEvent string
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if ev, data, ok := scanLine(strings.TrimRight(line, "\r\n"), &pendingEvent); ok {
			var payload map[string]any
			if json.Unmarshal([]byte(data), &payload) == nil {
				delta, _ := deltaFromChunk(payload)
				applyToolCallDeltas(delta, calls)
			}
			applyEvent(&content, &reason, &finish, &upErr, ev, data)
		}
		if err == io.EOF {
			break
		}
	}
	if upErr != nil {
		return nil, upErr
	}
	return &RawCompletion{
		Content:   unwrapQAContent(content.String()),
		Reasoning: reason.String(),
		Finish:    finish,
		ToolCalls: sortedToolCalls(calls),
	}, nil
}

// Aggregate 非流式聚合为 OpenAI chat.completion。
func Aggregate(r io.Reader, model string) (map[string]any, error) {
	rc, err := AggregateRaw(r)
	if err != nil {
		return nil, err
	}
	message := map[string]any{"role": "assistant", "content": rc.Content}
	if rc.Reasoning != "" {
		message["reasoning_content"] = rc.Reasoning
	}
	if len(rc.ToolCalls) > 0 {
		message["tool_calls"] = rc.ToolCalls
		if rc.Content == "" {
			message["content"] = nil
		}
	}
	return map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": rc.Finish}},
	}, nil
}

// Stream 实时转换 SSE，保证至少一个 [DONE]。
func Stream(w http.ResponseWriter, r io.Reader, model string) error {
	return streamWithCapture(w, r, model, nil)
}

// StreamCapture 同 Stream，正常结束时回调聚合结果。
func StreamCapture(w http.ResponseWriter, r io.Reader, model string, onDone func(*RawCompletion)) error {
	return streamWithCapture(w, r, model, onDone)
}

func streamWithCapture(w http.ResponseWriter, r io.Reader, model string, onDone func(*RawCompletion)) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	br := bufio.NewReaderSize(r, 64*1024)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	sawDone := false
	var (
		content   strings.Builder
		reason    strings.Builder
		finish    = "stop"
		streamErr error
		calls     = make(toolCallAccumulator)
	)
	var pendingEvent string

	writeChunk := func(delta map[string]any, fin string) error {
		choice := map[string]any{"index": 0, "delta": delta}
		if fin != "" {
			choice["finish_reason"] = fin
		}
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{choice},
		}
		raw, _ := json.Marshal(chunk)
		if _, err := io.WriteString(w, "data: "+string(raw)+"\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	writeDONE := func() error {
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	writeErr := func(msg string) error {
		payload := map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error", "code": "CODEARTS_STREAM_ERROR"}}
		raw, _ := json.Marshal(payload)
		if _, err := io.WriteString(w, "event: error\n"+"data: "+string(raw)+"\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			streamErr = err
			break
		}
		if ev, data, ok := scanLine(strings.TrimRight(line, "\r\n"), &pendingEvent); ok {
			if strings.TrimSpace(data) == "[DONE]" {
				if !sawDone {
					if werr := writeDONE(); werr != nil {
						streamErr = werr
						break
					}
					sawDone = true
				}
				break
			}
			var payload map[string]any
			_ = json.Unmarshal([]byte(data), &payload)
			nativeDelta, nativeFinish := deltaFromChunk(payload)
			applyToolCallDeltas(nativeDelta, calls)
			var upErr error
			beforeContent, beforeReason := content.Len(), reason.Len()
			applyEvent(&content, &reason, &finish, &upErr, ev, data)
			if upErr != nil {
				if werr := writeErr(upErr.Error()); werr != nil {
					streamErr = werr
					break
				}
				if werr := writeDONE(); werr != nil {
					streamErr = werr
					break
				}
				sawDone = true
				streamErr = upErr
				break
			}
			delta := map[string]any{}
			if role, ok := nativeDelta["role"].(string); ok && role != "" {
				delta["role"] = role
			}
			if content.Len() > beforeContent {
				delta["content"] = content.String()[beforeContent:]
			}
			if reason.Len() > beforeReason {
				delta["reasoning_content"] = reason.String()[beforeReason:]
			}
			if toolCalls, ok := nativeDelta["tool_calls"]; ok {
				delta["tool_calls"] = toolCalls
			}
			if len(delta) > 0 || nativeFinish != "" {
				if werr := writeChunk(delta, nativeFinish); werr != nil {
					streamErr = werr
					break
				}
			}
			if strings.EqualFold(ev, "done") || strings.EqualFold(ev, "end") || strings.EqualFold(ev, "finish") {
				if nativeFinish == "" {
					if werr := writeChunk(map[string]any{}, finish); werr != nil {
						streamErr = werr
						break
					}
				}
				if werr := writeDONE(); werr != nil {
					streamErr = werr
					break
				}
				sawDone = true
			}
		}
		if sawDone {
			break
		}
		if err == io.EOF {
			break
		}
	}
	if streamErr == nil && !sawDone {
		streamErr = writeDONE()
	}
	if streamErr == nil && onDone != nil {
		onDone(&RawCompletion{
			Content:   unwrapQAContent(content.String()),
			Reasoning: reason.String(),
			Finish:    finish,
			ToolCalls: sortedToolCalls(calls),
		})
	}
	return streamErr
}
