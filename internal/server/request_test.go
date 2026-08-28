package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildUpstreamMessagesNewChat(t *testing.T) {
	req := &chatRequest{
		Messages: []openAIMessage{
			{Role: "system", Text: "be nice"},
			{Role: "user", Text: "hi"},
			{Role: "assistant", Text: "hello"},
			{Role: "user", Text: "again"},
		},
	}
	msgs := buildUpstreamMessages(req, false, false)
	if len(msgs) != 1 {
		t.Fatalf("msgs=%d", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "be nice") || !strings.Contains(msgs[0].Text, "again") {
		t.Fatalf("full prompt missing pieces: %q", msgs[0].Text)
	}
	if !strings.Contains(msgs[0].Text, "[对话历史]") {
		t.Fatalf("expected history fold for multi-turn: %q", msgs[0].Text)
	}
}

func TestBuildUpstreamMessagesContinueChat(t *testing.T) {
	req := &chatRequest{
		Messages: []openAIMessage{
			{Role: "system", Text: "be nice"},
			{Role: "user", Text: "hi"},
			{Role: "user", Text: "again"},
		},
	}
	msgs := buildUpstreamMessages(req, false, true)
	if len(msgs) != 1 {
		t.Fatalf("msgs=%d", len(msgs))
	}
	// 续聊只发尾部，不该再塞完整 system+历史
	if strings.Contains(msgs[0].Text, "be nice") || strings.Contains(msgs[0].Text, "[对话历史]") {
		t.Fatalf("continue should be tail-only: %q", msgs[0].Text)
	}
	if msgs[0].Text != "again" {
		t.Fatalf("tail=%q", msgs[0].Text)
	}
}

func TestParseChatRequest(t *testing.T) {
	body := `{
		"model":"glm-5.2",
		"stream":true,
		"reasoning_effort":"high",
		"max_completion_tokens":8192,
		"temperature":0.3,
		"top_p":0.8,
		"conversation_id":"conv-1",
		"messages":[
			{"role":"system","content":"be nice"},
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"hello"},
			{"role":"user","content":[{"type":"text","text":"again"}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		],
		"tools":[{"type":"function","function":{"name":"f1","parameters":{"type":"object","properties":{}}}}],
		"tool_choice":"required"
	}`
	req, err := parseChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "glm-5.2" || !req.Stream || req.ConversationID != "conv-1" {
		t.Fatalf("basic fields: %+v", req)
	}
	if req.ReasoningEffort != "high" || req.MaxTokens == nil || *req.MaxTokens != 8192 {
		t.Fatalf("generation options: %+v", req)
	}
	if req.Temperature == nil || *req.Temperature != 0.3 || req.TopP == nil || *req.TopP != 0.8 {
		t.Fatalf("sampling options: %+v", req)
	}
	if len(req.Messages) != 5 {
		t.Fatalf("messages=%d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Text != "be nice" {
		t.Fatalf("sys=%+v", req.Messages[0])
	}
	if req.Messages[3].Text != "again" {
		t.Fatalf("fragmented content=%q", req.Messages[3].Text)
	}
	if req.Messages[4].Role != "tool" || req.Messages[4].ToolCallID != "call_1" {
		t.Fatalf("tool msg=%+v", req.Messages[4])
	}
	if len(req.Tools) != 1 || req.ToolChoice.Mode != "required" {
		t.Fatalf("tools=%v choice=%+v", req.Tools, req.ToolChoice)
	}
}

func TestParseChatRequestErrors(t *testing.T) {
	if _, err := parseChatRequest([]byte(`{}`)); err == nil {
		t.Fatal("expected error for empty messages")
	}
	if _, err := parseChatRequest([]byte(`{"messages":[{"role":"assistant","content":"x"}]}`)); err == nil {
		t.Fatal("expected error: no user/tool message")
	}
	if _, err := parseChatRequest([]byte(`{"max_tokens":0,"messages":[{"role":"user","content":"x"}]}`)); err == nil {
		t.Fatal("expected error: invalid max_tokens")
	}
	if _, err := parseChatRequest([]byte(`{"top_p":2,"messages":[{"role":"user","content":"x"}]}`)); err == nil {
		t.Fatal("expected error: invalid top_p")
	}
}

func TestParseAssistantToolCalls(t *testing.T) {
	body := `{"messages":[{"role":"assistant","content":"","tool_calls":[
		{"id":"call_1","type":"function","function":{"name":"f1","arguments":"{\"a\":1}"}}
	]},{"role":"user","content":"done"}]}`
	req, err := parseChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages[0].ToolCalls) != 1 {
		t.Fatalf("tool calls=%d", len(req.Messages[0].ToolCalls))
	}
	c := req.Messages[0].ToolCalls[0]
	if c.ID != "call_1" || c.Name != "f1" || c.Arguments != `{"a":1}` {
		t.Fatalf("call=%+v", c)
	}
}

func TestRawJSONString(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"{\"a\":1}"`, `{"a":1}`},
		{`{"a":1,"b":2}`, `{"a":1,"b":2}`},
		{``, `{}`},
		{`null`, `{}`},
	}
	for _, c := range cases {
		if got := rawJSONString(json.RawMessage(c.in)); got != c.want {
			t.Fatalf("rawJSONString(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestFingerprintStableAndReplay(t *testing.T) {
	msgs := []openAIMessage{
		{Role: "user", Text: "hi"},
		{Role: "assistant", Text: "hello"},
		{Role: "user", Text: "again"},
	}
	fp1 := fingerprintOf(msgs)
	fp2 := fingerprintOf(msgs)
	if fp1 != fp2 {
		t.Fatalf("fingerprint not stable: %s vs %s", fp1, fp2)
	}
	// 网关生成的 assistant tool_calls 被客户端原样回发时应能复算。
	base := []openAIMessage{{Role: "user", Text: "q"}}
	assistant := openAIMessage{Role: "assistant", Text: "", ToolCalls: []openAIToolCall{
		{ID: "call_x", Name: "f", Arguments: `{"p":1}`},
	}}
	after := chainFingerprint(fingerprintOf(base), assistant)
	replay := append([]openAIMessage{}, base...)
	replay = append(replay, assistant)
	if fingerprintOf(replay) != after {
		t.Fatalf("replay fingerprint mismatch: %s vs %s", fingerprintOf(replay), after)
	}
}

func TestFlattenContent(t *testing.T) {
	if flattenContent(json.RawMessage(`"plain"`)) != "plain" {
		t.Fatal("string content")
	}
	if flattenContent(json.RawMessage(`[{"type":"text","text":"a"},{"type":"image_url","imageUrl":{"url":"x"}}]`)) != "a" {
		t.Fatal("parts content")
	}
	if flattenContent(json.RawMessage(`null`)) != "" {
		t.Fatal("null content")
	}
}

var _ = strings.TrimSpace
