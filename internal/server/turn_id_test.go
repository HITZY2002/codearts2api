package server

import (
	"strings"
	"testing"
)

func TestUpstreamTurnKeyStableAcrossToolContinuation(t *testing.T) {
	base := []openAIMessage{
		{Role: "system", Text: "sys"},
		{Role: "user", Text: "question"},
	}
	k1 := upstreamTurnKey(&chatRequest{Messages: base})
	if k1 == "" {
		t.Fatal("empty turn key")
	}

	continued := append(append([]openAIMessage{}, base...),
		openAIMessage{Role: "assistant", Text: "thinking", ToolCalls: []openAIToolCall{{ID: "call_1", Name: "f"}}},
		openAIMessage{Role: "tool", ToolCallID: "call_1", Text: "result"},
	)
	if got := upstreamTurnKey(&chatRequest{Messages: continued}); got != k1 {
		t.Fatalf("tool continuation changed turn key: %q != %q", got, k1)
	}

	nextTurn := append(append([]openAIMessage{}, continued...), openAIMessage{Role: "user", Text: "follow-up"})
	if got := upstreamTurnKey(&chatRequest{Messages: nextTurn}); got == k1 {
		t.Fatalf("new user turn reused key %q", got)
	}
}

func TestUpstreamTurnKeySeparatesConversations(t *testing.T) {
	msgs := []openAIMessage{{Role: "user", Text: "same first message"}}
	a := upstreamTurnKey(&chatRequest{Messages: msgs, ConversationID: "conv-a"})
	b := upstreamTurnKey(&chatRequest{Messages: msgs, ConversationID: "conv-b"})
	if a == b {
		t.Fatalf("conversations share turn key %q", a)
	}
	if !strings.HasPrefix(a, "conv-a:") {
		t.Fatalf("conversation id missing from key: %q", a)
	}
}

func TestUpstreamMsgIDForPerAccountTurn(t *testing.T) {
	h := NewHandler(Config{})

	a1 := h.upstreamMsgIDFor("acct-a", "turn-1")
	a2 := h.upstreamMsgIDFor("acct-a", "turn-1")
	if a1 != a2 {
		t.Fatalf("same account+turn must reuse id: %q != %q", a1, a2)
	}
	if len(a1) != 30 || !strings.HasPrefix(a1, "msg_") {
		t.Fatalf("unexpected id shape: %q", a1)
	}

	b1 := h.upstreamMsgIDFor("acct-b", "turn-1")
	if b1 == a1 {
		t.Fatalf("different accounts must not share id %q", b1)
	}

	a3 := h.upstreamMsgIDFor("acct-a", "turn-2")
	if a3 == a1 {
		t.Fatalf("different turns must not share id %q", a3)
	}
}

func TestUpstreamSessionIDPerAccountConversation(t *testing.T) {
	s1 := upstreamSessionID("acct-a", "chat-1")
	if len(s1) != 32 {
		t.Fatalf("session id length: %q", s1)
	}
	if s1 != upstreamSessionID("acct-a", "chat-1") {
		t.Fatal("same account+conversation must be stable")
	}
	if s1 == upstreamSessionID("acct-b", "chat-1") {
		t.Fatal("different accounts must not share upstream session id")
	}
	if s1 == upstreamSessionID("acct-a", "chat-2") {
		t.Fatal("different conversations must not share upstream session id")
	}
}
