// CodeArts /v1/chat/chat SSE → OpenAI chat.completion 转换。
//
// 标准 SSE（event: <name> + data: <json>）。常见事件（逆向自 vscode-codebot）：
//   - onAnswer:  {"text":"增量","custom_extra_info":...}
//   - reasoning / thinking: 思考链增量
//   - done:      {"error_code":"0","error_msg":"","related_question_answer":[...],"response_message_id":"..."}
//     related_question_answer 是「相关追问」建议，不是正文，解析时忽略。
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

// APIError applies the same status classification to errors found after SSE
// preflight, so streaming and aggregated responses treat them consistently.
func (e *UpstreamError) APIError() *ApiError {
	return embeddedSSEError(map[string]any{"error_code": e.Code, "error_msg": e.Msg})
}

// RawCompletion 聚合结果。
type RawCompletion struct {
	Content     string
	Reasoning   string
	Finish      string
	ToolCalls   []ChatToolCall
	Usage       map[string]any
	CreditUsage *CreditUsage
}

// CreditUsage captures billing metadata from a CodeArts Web done frame.
type CreditUsage struct {
	CreditCost          float64 `json:"credit_cost"`
	CacheHitRatio       float64 `json:"cache_hit_ratio"`
	MsgID               string  `json:"msg_id"`
	PromptTokenUsed     int64   `json:"prompt_token_used"`
	CompletionTokenUsed int64   `json:"completion_token_used"`
	CachedTokenUsed     int64   `json:"cached_token_used"`
	TotalTokenUsed      int64   `json:"total_token_used"`
	RequestCount        int64   `json:"request_count"`
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

// textBuffer 是可按字节尾部取增量的文本累积器。strings.Builder 每个事件都要
// String() 全量拷贝来做前缀校验/增量切片，长回复是 O(n²)；这里增量路径 O(delta)。
type textBuffer struct{ buf []byte }

func (b *textBuffer) WriteString(s string) { b.buf = append(b.buf, s...) }
func (b *textBuffer) Reset()               { b.buf = b.buf[:0] }
func (b *textBuffer) Len() int             { return len(b.buf) }
func (b *textBuffer) String() string       { return string(b.buf) }
func (b *textBuffer) tail(from int) string { return string(b.buf[from:]) }

// hasBytePrefix 报告 s 是否以 b 为前缀，零分配。
func hasBytePrefix(s string, b []byte) bool {
	if len(s) < len(b) {
		return false
	}
	for i := range b {
		if s[i] != b[i] {
			return false
		}
	}
	return true
}

// replaceSnapshot 应用全文快照替换。check 为 true（流式转发）时校验已转发
// 内容仍是新快照的前缀，否则报 invalid_snapshot；聚合路径不校验。
func replaceSnapshot(content *textBuffer, t string, upErr *error, check bool) {
	if check && *upErr == nil && !hasBytePrefix(t, content.buf) {
		*upErr = &UpstreamError{Code: "invalid_snapshot", Msg: "upstream replaced already streamed content"}
	}
	content.Reset()
	content.WriteString(t)
}

// textFrom 从 JSON 对象按序取第一个出现的字符串字段。
func textFrom(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			return v
		}
	}
	return ""
}

// chunkError 识别错误帧。扁平形态：error_code/code 为非 "0" 字符串即错误；
// 嵌套形态：OpenAI 风格 error 对象。官方内核对 v2 流内错误做的是 in-band
// 检测（agent-kernel.exe crates\llm\adapters\src\lib.rs，"In-band stream
// error"），扁平与嵌套两种形态都要识别。返回 nil 表示这不是错误帧，
// 调用方继续按正常帧处理（error_code=0 的帧是正常帧）。
func chunkError(payload map[string]any) *UpstreamError {
	if payload == nil {
		return nil
	}
	if code := textFrom(payload, "error_code", "code"); code != "" {
		if code == "0" {
			return nil
		}
		return &UpstreamError{Code: code, Msg: firstNonEmpty(textFrom(payload, "error_msg", "message", "msg"), "request failed")}
	}
	eobj, ok := payload["error"].(map[string]any)
	if !ok {
		// error 为字符串（非标准信封）：整串即错误消息。
		if es, isStr := payload["error"].(string); isStr && es != "" {
			return &UpstreamError{Code: "error", Msg: es}
		}
		return nil
	}
	msg := textFrom(eobj, "message", "error_msg", "msg")
	code := textFrom(eobj, "error_code", "code")
	if code == "0" {
		return nil
	}
	if code == "" {
		// 无错误码：error 对象本身就是失败信号（对齐官方 "provider reported a stream error"）
		return &UpstreamError{Code: "error", Msg: firstNonEmpty(msg, "provider reported a stream error")}
	}
	return &UpstreamError{Code: code, Msg: firstNonEmpty(msg, "request failed")}
}

// applyEvent 把单事件应用到聚合状态。
func applyEvent(content, reason *textBuffer, finish *string, upErr *error, event, data string, checkSnapshot bool) {
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
		// 纯 data 行（CodeArts 部分响应无 event 前缀）：扁平 error_code 与
		// 嵌套 error 对象（官方内核同款 in-band 检测）都要识别。
		if ue := chunkError(payload); ue != nil {
			*upErr = ue
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
			replaceSnapshot(content, t, upErr, checkSnapshot)
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
				replaceSnapshot(content, sb.String(), upErr, checkSnapshot)
			}
			return
		}
		// 纯 structured QA 帧（无 text）：仅正文仍空时取 answer，避免整段 JSON 当回复
		tryFillStructuredAnswer(content, payload)
	case "onanswer", "answer", "delta", "message", "content":
		if ue := chunkError(payload); ue != nil {
			*upErr = ue
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
			replaceSnapshot(content, t, upErr, checkSnapshot)
			return
		}
		tryFillStructuredAnswer(content, payload)
	case "reasoning", "thinking", "onreasoning", "onthinking":
		if t := text("text", "reasoning", "content", "thinking"); t != "" {
			reason.WriteString(t)
		}
	case "error":
		// 上游显式错误事件（见文件头注释）：此前无分支，会被聚合成
		// 「空内容成功」。结构化载荷交给 chunkError（error_code=0 视为正常帧）。
		if ue := chunkError(payload); ue != nil {
			*upErr = ue
			return
		}
		if payload != nil {
			// error_code=0 是正常帧；除此之外，error 事件里任何无可识别
			// 错误码的结构化载荷都不能当成功流（独立验证发现的吞错误缺口）。
			if code := textFrom(payload, "error_code", "code"); code == "" {
				if _, hasErr := payload["error"]; !hasErr {
					*upErr = &UpstreamError{Code: "error", Msg: firstNonEmpty(
						textFrom(payload, "message", "error_msg", "msg"),
						strings.TrimSpace(data), "request failed")}
				}
			}
			return
		}
		if msg := strings.TrimSpace(data); msg != "" {
			// 纯文本/非 JSON 载荷：整段 data 即错误消息。
			*upErr = &UpstreamError{Code: "error", Msg: msg}
			return
		}
		// 空载荷的 error 事件同样不能当作成功流。
		*upErr = &UpstreamError{Code: "error", Msg: "request failed"}
	case "done", "end", "finish":
		if ue := chunkError(payload); ue != nil {
			*upErr = ue
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
func tryFillStructuredAnswer(content *textBuffer, obj map[string]any) {
	if content.Len() > 0 || !isValidStructuredQA(obj) {
		return
	}
	if a, ok := obj["answer"].(string); ok {
		content.WriteString(a)
	}
}

// unwrapQAContent 移除 v1 通道已知的 ```markdown / ```json 包装，
// 若内容本身是结构化问答 JSON，则直接提取 answer。
// 其他语言的围栏与裸围栏是模型正文，原样保留（对齐官方内核行为）。
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
			// 仅剥 v1 通道已知的 ```markdown / ```json 包装（逆向 docstring）。
			// 其他围栏（```python、裸 ``` 等）是模型自己的代码块，必须保留：
			// 官方内核对正文不做任何围栏处理（agent-kernel.exe llm adapters
			// 无围栏逻辑，```markdown 字符串全部来自系统提示词模板）。
			if (strings.HasPrefix(first, "```markdown") || strings.HasPrefix(first, "```json")) && last == "```" {
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
		content     textBuffer
		reason      textBuffer
		finish      = "stop"
		upErr       error
		calls       = make(toolCallAccumulator)
		usage       map[string]any
		creditUsage *CreditUsage
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
				if u, ok := payload["usage"].(map[string]any); ok {
					usage = u
				}
				if credit := creditUsageFromPayload(payload); credit != nil {
					creditUsage = credit
				}
				delta, _ := deltaFromChunk(payload)
				applyToolCallDeltas(delta, calls)
			}
			applyEvent(&content, &reason, &finish, &upErr, ev, data, false)
		}
		if err == io.EOF {
			break
		}
	}
	if upErr != nil {
		return nil, upErr
	}
	return &RawCompletion{
		Content:     unwrapQAContent(content.String()),
		Reasoning:   reason.String(),
		Finish:      finish,
		ToolCalls:   sortedToolCalls(calls),
		Usage:       usage,
		CreditUsage: creditUsage,
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
	return StreamCaptureWithUsage(w, r, model, nil, nil)
}

// StreamCapture 同 Stream，正常结束时回调聚合结果。
func StreamCapture(w http.ResponseWriter, r io.Reader, model string, onDone func(*RawCompletion)) error {
	return StreamCaptureWithUsage(w, r, model, onDone, nil)
}

// StreamCaptureWithUsage emits a final usage chunk when usageFor is non-nil.
// usageFor can supply an estimate when the upstream omits usage.
func StreamCaptureWithUsage(w http.ResponseWriter, r io.Reader, model string, onDone func(*RawCompletion), usageFor func(*RawCompletion) map[string]any) error {
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
		content     textBuffer
		reason      textBuffer
		finish      = "stop"
		streamErr   error
		calls       = make(toolCallAccumulator)
		usage       map[string]any
		creditUsage *CreditUsage
	)
	var pendingEvent string

	completion := func() *RawCompletion {
		return &RawCompletion{
			Content: unwrapQAContent(content.String()), Reasoning: reason.String(),
			Finish: finish, ToolCalls: sortedToolCalls(calls), Usage: usage,
			CreditUsage: creditUsage,
		}
	}
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
		if usageFor != nil {
			chunk["usage"] = nil
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
	writeDONE := func(success bool) error {
		if success && usageFor != nil {
			chunk := map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
				"model": model, "choices": []any{}, "usage": usageFor(completion()),
			}
			raw, _ := json.Marshal(chunk)
			if _, err := io.WriteString(w, "data: "+string(raw)+"\n\n"); err != nil {
				return err
			}
		}
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

	// OpenAI 规范：流式首个 delta 带 role。注入引导 chunk（对齐官方 SDK 行为），
	// 后续上游若自带 role 也不再重复透传。
	if werr := writeChunk(map[string]any{"role": "assistant", "content": ""}, ""); werr != nil {
		return werr
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
					if werr := writeDONE(true); werr != nil {
						streamErr = werr
						break
					}
					sawDone = true
				}
				break
			}
			var payload map[string]any
			_ = json.Unmarshal([]byte(data), &payload)
			if u, ok := payload["usage"].(map[string]any); ok {
				usage = u
			}
			if credit := creditUsageFromPayload(payload); credit != nil {
				creditUsage = credit
			}
			nativeDelta, nativeFinish := deltaFromChunk(payload)
			applyToolCallDeltas(nativeDelta, calls)
			var upErr error
			beforeLen, beforeReasonLen := content.Len(), reason.Len()
			applyEvent(&content, &reason, &finish, &upErr, ev, data, true)
			if upErr != nil {
				if werr := writeErr(upErr.Error()); werr != nil {
					streamErr = werr
					break
				}
				if werr := writeDONE(false); werr != nil {
					streamErr = werr
					break
				}
				sawDone = true
				streamErr = upErr
				break
			}
			delta := map[string]any{}
			// role 已由引导 chunk 注入，上游自带 role 不重复透传。
			if content.Len() > beforeLen {
				delta["content"] = content.tail(beforeLen)
			}
			if reason.Len() > beforeReasonLen {
				delta["reasoning_content"] = reason.tail(beforeReasonLen)
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
				if werr := writeDONE(true); werr != nil {
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
		streamErr = writeDONE(true)
	}
	if streamErr == nil && onDone != nil {
		onDone(completion())
	}
	return streamErr
}

func creditUsageFromPayload(payload map[string]any) *CreditUsage {
	raw, ok := payload["credit_cost_for_web"].(map[string]any)
	if !ok {
		return nil
	}
	var out CreditUsage
	if value, ok := raw["id"].(string); ok {
		out.MsgID = value
	}
	if value, ok := raw["credit_cost"].(float64); ok {
		out.CreditCost = value
	}
	if value, ok := raw["cache_hit_ratio"].(float64); ok {
		out.CacheHitRatio = value
	}
	if value, ok := raw["prompt_token_used"].(float64); ok {
		out.PromptTokenUsed = int64(value)
	}
	if value, ok := raw["completion_token_used"].(float64); ok {
		out.CompletionTokenUsed = int64(value)
	}
	if value, ok := raw["cached_token_used"].(float64); ok {
		out.CachedTokenUsed = int64(value)
	}
	if value, ok := raw["total_token_used"].(float64); ok {
		out.TotalTokenUsed = int64(value)
	}
	if value, ok := raw["request_count"].(float64); ok {
		out.RequestCount = int64(value)
	}
	return &out
}
