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
	msgs := buildUpstreamMessages(req, false)
	if len(msgs) != 4 {
		t.Fatalf("msgs=%d", len(msgs))
	}
	if msgs[0].Role != "system" || msgs[0].Content != "be nice" || msgs[3].Role != "user" || msgs[3].Content != "again" {
		t.Fatalf("native messages lost roles/content: %#v", msgs)
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
	msgs := buildUpstreamMessages(req, false)
	if len(msgs) != 3 {
		t.Fatalf("msgs=%d", len(msgs))
	}
	// V2 不依赖服务端持续 Agent 语义状态：完整历史以原生 role 重放。
	if msgs[0].Role != "system" || msgs[1].Content != "hi" || msgs[2].Content != "again" {
		t.Fatalf("continue omitted native history: %#v", msgs)
	}
}

func TestBuildUpstreamMessagesContinueToolTurnReplaysStatelessHistory(t *testing.T) {
	req := &chatRequest{
		Messages: []openAIMessage{
			{Role: "system", Text: "be nice"},
			{Role: "user", Text: "inspect the repository"},
			{Role: "assistant", ToolCalls: []openAIToolCall{{ID: "call_1", Name: "bash", Arguments: `{"command":"pwd"}`}}},
			{Role: "tool", ToolCallID: "call_1", Name: "bash", Text: "/workspace"},
		},
	}
	msgs := buildUpstreamMessages(req, true)
	if len(msgs) != 4 {
		t.Fatalf("msgs=%d", len(msgs))
	}
	if msgs[1].Role != "user" || msgs[1].Content != "inspect the repository" {
		t.Fatalf("tool continuation omitted original task: %#v", msgs)
	}
	if msgs[2].Role != "assistant" || len(msgs[2].ToolCalls) != 1 || msgs[2].ToolCalls[0].Function.Name != "bash" {
		t.Fatalf("assistant tool call was not native: %#v", msgs[2])
	}
	if msgs[3].Role != "tool" || msgs[3].ToolCallID != "call_1" || msgs[3].Content != "/workspace" {
		t.Fatalf("tool result was not native: %#v", msgs[3])
	}
}

func TestBuildUpstreamMessagesDropsIncompleteToolBatch(t *testing.T) {
	req := &chatRequest{Messages: []openAIMessage{
		{Role: "user", Text: "inspect"},
		{Role: "assistant", Text: "still useful", ToolCalls: []openAIToolCall{
			{ID: "call_1", Name: "read", Arguments: `{}`},
			{ID: "call_2", Name: "bash", Arguments: `{}`},
		}},
		{Role: "tool", ToolCallID: "call_1", Text: "partial"},
		{Role: "user", Text: "continue"},
	}}
	msgs := buildUpstreamMessages(req, true)
	if len(msgs) != 3 {
		t.Fatalf("orphan tool result should be dropped: %#v", msgs)
	}
	if msgs[1].Role != "assistant" || len(msgs[1].ToolCalls) != 0 || msgs[1].Content != "still useful" {
		t.Fatalf("incomplete tool call batch should be stripped: %#v", msgs[1])
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
			{"role":"assistant","content":"hello","reasoning_content":"thought"},
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
	if req.Messages[2].ReasoningContent != "thought" {
		t.Fatalf("reasoning content=%q", req.Messages[2].ReasoningContent)
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

func TestConversationIDStaysStableAsOpenAIHistoryGrows(t *testing.T) {
	first := openAIMessage{Role: "user", Text: "开始一个长时间代码审计"}
	round1 := &chatRequest{Messages: []openAIMessage{
		{Role: "system", Text: "time=1"},
		first,
	}}
	round2 := &chatRequest{Messages: []openAIMessage{
		{Role: "system", Text: "time=2"},
		first,
		{Role: "assistant", ToolCalls: []openAIToolCall{{ID: "call_1", Name: "bash", Arguments: `{"command":"pwd"}`}}},
		{Role: "tool", ToolCallID: "call_1", Text: "/workspace"},
	}}

	id1 := conversationIDFor(round1, "")
	id2 := conversationIDFor(round2, "")
	if id1 == "" || id1 != id2 || !validChatID(id1) {
		t.Fatalf("conversation IDs are not stable valid chat IDs: %q vs %q", id1, id2)
	}
}

func TestConversationIDSeparatesTitleChatFromToolAgent(t *testing.T) {
	first := openAIMessage{Role: "user", Text: "inspect the repository"}
	title := &chatRequest{Messages: []openAIMessage{first}}
	agent := &chatRequest{
		Messages: []openAIMessage{first},
		Tools:    []map[string]any{{"type": "function", "function": map[string]any{"name": "bash"}}},
	}
	if conversationIDFor(title, "") == conversationIDFor(agent, "") {
		t.Fatal("title generation and tool agent must not share CodeArts built-in chat state")
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
