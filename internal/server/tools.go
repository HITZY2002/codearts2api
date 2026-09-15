// 工具调用（Function Calling）模拟层。
//
// 上游 /v1/chat/chat 没有用户自定义 function-calling 通道，因此在网关侧
// 用「提示词注入 + 输出解析」模拟 OpenAI tools 语义：
//   - 请求带 tools 时，把工具 schema 注入提示词，要求模型用 ```tool_call 代码块回答；
//   - 响应文本里解析 tool_call 块，转换回 OpenAI tool_calls（流式按 index 增量、
//     非流式挂 message.tool_calls，finish_reason=tool_calls）。
//
// 与 workbuddy2api/traework2api 的原生透传相比，这是对无原生支持上游的标准做法。
package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolSpec 归一化后的工具定义。
type toolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// normalizeTools 从 OpenAI tools 数组提取 function 定义。
func normalizeTools(raw []map[string]any) []toolSpec {
	var out []toolSpec
	for _, t := range raw {
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		spec := toolSpec{Name: name}
		spec.Description, _ = fn["description"].(string)
		if p, ok := fn["parameters"]; ok {
			if raw, err := json.Marshal(p); err == nil {
				spec.Parameters = raw
			}
		}
		if len(spec.Parameters) == 0 {
			spec.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, spec)
	}
	return out
}

// toolsActive 判断本次请求是否启用工具模拟。
func toolsActive(req *chatRequest) bool {
	return len(normalizeTools(req.Tools)) > 0 && req.ToolChoice.Mode != "none"
}

// buildToolsPrompt 生成注入提示词的工具说明块。
func buildToolsPrompt(specs []toolSpec, choice toolChoiceOpenAI) string {
	var sb strings.Builder
	sb.WriteString("\n\n# 工具调用（重要）\n")
	sb.WriteString("你可以调用下列工具。需要调用时，在回复中输出一个 ```tool_call 代码块，格式严格如下：\n")
	sb.WriteString("```tool_call\n{\"name\":\"<工具名>\",\"arguments\":{<参数JSON>}}\n```\n")
	sb.WriteString("可输出多个 tool_call 块调用多个工具；代码块之外不要复述工具调用内容。")
	sb.WriteString("不需要调用工具时正常回复文本，绝对不要输出 tool_call 代码块。\n")
	switch choice.Mode {
	case "required":
		sb.WriteString("本轮【必须】至少调用一个工具，禁止直接文本回答。\n")
	case "function":
		sb.WriteString(fmt.Sprintf("本轮【必须】调用工具 %q。\n", choice.Function))
	}
	sb.WriteString("工具列表：\n")
	for i, s := range specs {
		sb.WriteString(fmt.Sprintf("%d. %s", i+1, s.Name))
		if s.Description != "" {
			sb.WriteString(" — " + s.Description)
		}
		sb.WriteString("\n   parameters: " + string(s.Parameters) + "\n")
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// 输出解析
// ---------------------------------------------------------------------------

// extractToolCalls 从模型输出中提取工具调用。
// 返回解析到的调用、剥离调用块后的剩余文本、是否找到。
func extractToolCalls(text string) (calls []openAIToolCall, rest string, found bool) {
	rest = text
	// 1. 优先找 ```tool_call ... ``` 代码块（可能多个）。
	for {
		start := strings.Index(rest, "```tool_call")
		if start < 0 {
			break
		}
		bodyStart := start + len("```tool_call")
		end := strings.Index(rest[bodyStart:], "```")
		if end < 0 {
			break
		}
		block := rest[bodyStart : bodyStart+end]
		if c, ok := parseCallBlock(block); ok {
			calls = append(calls, c...)
		}
		// 显式标记为 tool_call 的围栏属于内部协议。即使单个块损坏也丢弃该块
		// 并继续查找后续有效调用，不得将协议原文作为 assistant content 泄漏。
		rest = strings.TrimSpace(rest[:start] + " " + rest[bodyStart+end+3:])
	}
	if len(calls) > 0 {
		return calls, stripToolWrappers(rest), true
	}
	// 2. 兜底：找含 "tool_calls" 键的 JSON 对象（裸 JSON 回复）。
	if idx := strings.Index(rest, "\"tool_calls\""); idx >= 0 {
		if objStart := strings.LastIndex(rest[:idx], "{"); objStart >= 0 {
			if obj, end := scanJSONObject(rest, objStart); obj != "" {
				if c, ok := parseToolCallsJSON(obj); ok {
					return c, stripToolWrappers(rest[:objStart] + " " + rest[end:]), true
				}
			}
		}
	}
	// 3. 兜底：单个 {"name":...,"arguments":...} 对象。
	if idx := strings.Index(rest, "\"arguments\""); idx >= 0 {
		if objStart := strings.LastIndex(rest[:idx], "{"); objStart >= 0 {
			if obj, end := scanJSONObject(rest, objStart); obj != "" {
				if c, ok := parseCallBlock(obj); ok {
					return c, stripToolWrappers(rest[:objStart] + " " + rest[end:]), true
				}
			}
		}
	}
	return nil, text, false
}

func stripToolWrappers(text string) string {
	replacer := strings.NewReplacer(
		"<tool_call>", "",
		"</tool_call>", "",
		"<|tool_call|>", "",
		"<|/tool_call|>", "",
	)
	return strings.TrimSpace(replacer.Replace(text))
}

// toolResponse 把提示词模拟的工具调用转为 OpenAI 结构。一旦存在调用，
// 上游剩余文本不可信（可能是协议/工具历史复述），因此不作为 content 暴露。
func toolResponse(text string) (calls []openAIToolCall, content string, found bool) {
	calls, _, found = extractToolCalls(text)
	if found {
		return calls, "", true
	}
	return nil, text, false
}

// parseCallBlock 解析单个或多个 {"name","arguments"} 对象（也兼容 {"tool_calls":[...]} 包装）。
func parseCallBlock(block string) ([]openAIToolCall, bool) {
	block = strings.TrimSpace(block)
	if block == "" {
		return nil, false
	}
	if calls, ok := parseToolCallsJSON(block); ok {
		return calls, true
	}
	var single struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(block), &single); err != nil || single.Name == "" {
		return nil, false
	}
	return []openAIToolCall{{Name: single.Name, Arguments: rawJSONString(single.Arguments)}}, true
}

// parseToolCallsJSON 解析 {"tool_calls":[{"name","arguments"},...]} 包装形态。
func parseToolCallsJSON(raw string) ([]openAIToolCall, bool) {
	var wrap struct {
		ToolCalls []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil || len(wrap.ToolCalls) == 0 {
		return nil, false
	}
	var out []openAIToolCall
	for _, c := range wrap.ToolCalls {
		if c.Name == "" {
			return nil, false
		}
		out = append(out, openAIToolCall{Name: c.Name, Arguments: rawJSONString(c.Arguments)})
	}
	return out, true
}

// scanJSONObject 从 s[start]（须为 '{'）开始扫描配对的 JSON 对象，
// 返回对象文本与结束下标（exclusive）。失败返回 "", start。
func scanJSONObject(s string, start int) (string, int) {
	if start < 0 || start >= len(s) || s[start] != '{' {
		return "", start
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// 字符串内的花括号不计
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], i + 1
			}
		}
	}
	return "", start
}

// assignCallIDs 给解析出的调用分配 OpenAI 风格 id（call_xxx）。
func assignCallIDs(calls []openAIToolCall) {
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = "call_" + randHex(24)
		}
	}
}

// toOpenAIToolCalls 转为 OpenAI message.tool_calls 线格式。
func toOpenAIToolCalls(calls []openAIToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for i, c := range calls {
		out = append(out, map[string]any{
			"index": i,
			"id":    c.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      c.Name,
				"arguments": c.Arguments,
			},
		})
	}
	return out
}
