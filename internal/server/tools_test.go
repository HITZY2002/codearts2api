package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractToolCallsFenced(t *testing.T) {
	text := "先分析。\n```tool_call\n{\"name\":\"f1\",\"arguments\":{\"a\":1}}\n```\n然后结束。"
	calls, rest, found := extractToolCalls(text)
	if !found || len(calls) != 1 {
		t.Fatalf("found=%v calls=%+v", found, calls)
	}
	if calls[0].Name != "f1" || calls[0].Arguments != `{"a":1}` {
		t.Fatalf("call=%+v", calls[0])
	}
	if strings.Contains(rest, "tool_call") || !strings.Contains(rest, "先分析") || !strings.Contains(rest, "然后结束") {
		t.Fatalf("rest=%q", rest)
	}
}

func TestExtractToolCallsStripsGLMXMLWrapper(t *testing.T) {
	text := "准备检查。\n<tool_call>\n```tool_call\n{\"name\":\"bash\",\"arguments\":{\"command\":\"pwd\"}}\n```\n</tool_call>\n"
	calls, rest, found := extractToolCalls(text)
	if !found || len(calls) != 1 || calls[0].Name != "bash" {
		t.Fatalf("found=%v calls=%+v rest=%q", found, calls, rest)
	}
	if strings.Contains(rest, "tool_call") || strings.Contains(rest, "</tool_call>") {
		t.Fatalf("GLM tool protocol leaked into content: %q", rest)
	}
}

func TestExtractToolCallsSkipsMalformedBlockAndKeepsLaterValidCall(t *testing.T) {
	text := "<tool_call>\n```tool_call\nnot-json\n```\n</tool_call>\n" +
		"<tool_call>\n```tool_call\n{\"name\":\"bash\",\"arguments\":{\"command\":\"pwd\"}}\n```\n</tool_call>"
	calls, rest, found := extractToolCalls(text)
	if !found || len(calls) != 1 || calls[0].Name != "bash" {
		t.Fatalf("found=%v calls=%+v rest=%q", found, calls, rest)
	}
	if strings.Contains(rest, "tool_call") || strings.Contains(rest, "not-json") {
		t.Fatalf("malformed protocol block leaked into content: %q", rest)
	}
}

func TestToolResponseHidesProtocolTranscriptWhenCallIsPresent(t *testing.T) {
	text := "分析完成\n<tool_call>\n```tool_call\n{\"name\":\"bash\",\"arguments\":{\"command\":\"pwd\"}}\n```\n</tool_call>" +
		"\n[工具 call_1 返回结果]\n/a/very/large/internal/transcript"
	calls, content, found := toolResponse(text)
	if !found || len(calls) != 1 || calls[0].Name != "bash" {
		t.Fatalf("found=%v calls=%+v content=%q", found, calls, content)
	}
	if content != "" {
		t.Fatalf("tool protocol/history leaked into OpenAI content: %q", content)
	}
}

func TestExtractToolCallsWrappedJSON(t *testing.T) {
	text := `{"tool_calls":[{"name":"f1","arguments":{"a":1}},{"name":"f2","arguments":{"b":2}}]}`
	calls, _, found := extractToolCalls(text)
	if !found || len(calls) != 2 {
		t.Fatalf("found=%v calls=%+v", found, calls)
	}
	if calls[1].Name != "f2" {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestExtractToolCallsSingleObject(t *testing.T) {
	calls, _, found := extractToolCalls(`{"name":"f1","arguments":{"a":1}}`)
	if !found || len(calls) != 1 || calls[0].Name != "f1" {
		t.Fatalf("found=%v calls=%+v", found, calls)
	}
}

func TestExtractToolCallsNone(t *testing.T) {
	if _, _, found := extractToolCalls("普通回复，无工具调用。"); found {
		t.Fatal("should not find tool calls")
	}
}

func TestNormalizeTools(t *testing.T) {
	raw := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "f1", "description": "d1"}},
		{"type": "function"}, // 缺 function → 跳过
	}
	specs := normalizeTools(raw)
	if len(specs) != 1 || specs[0].Name != "f1" || specs[0].Description != "d1" {
		t.Fatalf("specs=%+v", specs)
	}
	if len(specs[0].Parameters) == 0 {
		t.Fatal("parameters should have default")
	}
}

func TestToolsActive(t *testing.T) {
	req := &chatRequest{
		Tools:      []map[string]any{{"type": "function", "function": map[string]any{"name": "f"}}},
		ToolChoice: toolChoiceOpenAI{Mode: "auto"},
	}
	if !toolsActive(req) {
		t.Fatal("should be active")
	}
	req.ToolChoice = toolChoiceOpenAI{Mode: "none"}
	if toolsActive(req) {
		t.Fatal("should be inactive when none")
	}
}

func TestBuildToolsPrompt(t *testing.T) {
	specs := []toolSpec{{Name: "f1", Description: "desc", Parameters: json.RawMessage(`{"type":"object"}`)}}
	p := buildToolsPrompt(specs, toolChoiceOpenAI{Mode: "required"})
	if !contains(p, "f1") || !contains(p, "必须") || !contains(p, "tool_call") {
		t.Fatalf("prompt missing parts: %s", p)
	}
}
