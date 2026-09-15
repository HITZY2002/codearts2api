package upstream

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAggregateCodeArtsSSE(t *testing.T) {
	stream := "data: {\"delta\":{\"content\":\"你\",\"reasoning_content\":\"思考\"}}\n" +
		"data: {\"delta\":{\"content\":\"好\"}}\n" +
		"event: done\ndata: {\"error_code\":\"0\",\"error_msg\":\"\"}\n\n"
	rc, err := AggregateRaw(bytes.NewBufferString(stream))
	if err != nil {
		t.Fatal(err)
	}
	if rc.Content != "你好" || rc.Reasoning != "思考" {
		t.Fatalf("rc=%+v", rc)
	}
}

func TestAggregateCodeArtsSnapshot(t *testing.T) {
	// text 为全文快照 → 替换语义
	stream := "data: {\"text\":\"你\"}\n" +
		"data: {\"text\":\"你好世界\"}\n" +
		"data: {\"text\":\"[DONE]\",\"error_code\":\"0\"}\n"
	rc, err := AggregateRaw(bytes.NewBufferString(stream))
	if err != nil {
		t.Fatal(err)
	}
	if rc.Content != "你好世界" {
		t.Fatalf("content=%q", rc.Content)
	}
}

func TestAggregateCodeArtsError(t *testing.T) {
	stream := "event: done\ndata: {\"error_code\":\"1001\",\"error_msg\":\"quota exceeded\"}\n\n"
	_, err := AggregateRaw(bytes.NewBufferString(stream))
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("err=%v", err)
	}
}

func TestStreamCodeArts(t *testing.T) {
	stream := "event: onAnswer\ndata: {\"text\":\"hi\"}\n\n" +
		"event: done\ndata: {\"error_code\":\"0\"}\n\n"
	rec := httptest.NewRecorder()
	var captured *RawCompletion
	err := StreamCapture(rec, bytes.NewBufferString(stream), "m", func(rc *RawCompletion) {
		captured = rc
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured == nil || captured.Content != "hi" {
		t.Fatalf("captured=%+v", captured)
	}
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestAggregateStructuredQA(t *testing.T) {
	// 纯 QA 对象帧（无 text）：正文为空时提取 answer
	stream := "data: {\"question\":\"What is the capital of France?\",\"options\":[\"Paris\",\"London\",\"Berlin\",\"Madrid\"],\"answer\":\"Paris\"}\n" +
		"event: done\ndata: {\"error_code\":\"0\"}\n\n"
	rc, err := AggregateRaw(bytes.NewBufferString(stream))
	if err != nil {
		t.Fatal(err)
	}
	if rc.Content != "Paris" {
		t.Fatalf("expected content=Paris, got %q", rc.Content)
	}
}

func TestAggregateRelatedQuestionAnswerIgnored(t *testing.T) {
	// related_question_answer 是追问建议，不应覆盖/充当正文
	stream := "data: {\"text\":\"你好，我是助手\"}\n" +
		"event: done\ndata: {\"error_code\":\"0\",\"related_question_answer\":[{\"question\":\"Q?\",\"answer\":\"A1\"}]}\n\n"
	rc, err := AggregateRaw(bytes.NewBufferString(stream))
	if err != nil {
		t.Fatal(err)
	}
	if rc.Content != "你好，我是助手" {
		t.Fatalf("expected real text, got %q", rc.Content)
	}
}

func TestUnwrapQAContentString(t *testing.T) {
	// 模型把整段 QA JSON 写进 text 时，聚合后 unwrap 成 answer
	stream := "data: {\"text\":\"{\\\"question\\\":\\\"What is the capital of France?\\\",\\\"options\\\":[\\\"Paris\\\",\\\"London\\\"],\\\"answer\\\":\\\"Paris\\\"}\"}\n" +
		"event: done\ndata: {\"error_code\":\"0\"}\n\n"
	rc, err := AggregateRaw(bytes.NewBufferString(stream))
	if err != nil {
		t.Fatal(err)
	}
	if rc.Content != "Paris" {
		t.Fatalf("expected Paris after unwrap, got %q", rc.Content)
	}
}

func TestPKCE(t *testing.T) {
	v, c, err := PKCE()
	if err != nil {
		t.Fatal(err)
	}
	if v == "" || c == "" || v == c {
		t.Fatalf("v=%q c=%q", v, c)
	}
}

func TestChatHeaders(t *testing.T) {
	h := ChatHeadersV2("tok", "trace-1", "zh-cn")
	if h["x-auth-token"] != "tok" || h["Accept"] != "text/event-stream" || h["app-id"] != "CodeAgent3.0" {
		t.Fatalf("headers=%v", h)
	}
	if _, has := h["Agent-Type"]; has {
		t.Fatalf("v2 headers must not contain Agent-Type: %v", h)
	}
}

func TestExchangeCodeEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/oauth2/tokens" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("content-type=%s", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("DPoP") == "" {
			t.Fatal("missing DPoP header")
		}
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != "code" {
			t.Fatalf("form=%v", r.PostForm)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"u1","user_name":"n1","domain_id":"d1","refresh_token":"rt","credentials":{"access_key_id":"ak","secret_access_key":"sk","security_token":"st","expiration":"2026-08-04T00:00:00Z"}}`))
	}))
	defer srv.Close()
	c := New(5 * time.Second)
	cfg := DefaultLoginConfig()
	cfg.STSHost = srv.URL
	resp, err := c.ExchangeCode(context.Background(), cfg, "code", "verifier", 9999, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.UserID != "u1" || resp.Credentials.SecurityToken != "st" || resp.RefreshToken != "rt" {
		t.Fatalf("resp=%+v", resp)
	}
}

func TestPollTicketLegacyCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"u9","user_name":"n9","domain_id":"d9","credential":{"access":"AK","secret":"SK","securitytoken":"ST","expires_at":"2026-08-04T00:00:00Z"}}`))
	}))
	defer srv.Close()
	c := New(5 * time.Second)
	cfg := DefaultLoginConfig()
	cfg.SnapManager = srv.URL
	tok, err := c.PollTicket(context.Background(), cfg, "ticket", "portal-secret")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Credentials.SecurityToken != "ST" || tok.Credentials.AccessKeyID != "AK" || tok.Credentials.SecretAccessKey != "SK" {
		t.Fatalf("credential=%+v", tok.Credentials)
	}
}

func TestSigner(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://snap-access.cn-north-4.myhuaweicloud.com/v1/chat/chat", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	signRequest(req, []byte(`{}`), SignCredential{
		AccessKeyID:     "AK",
		SecretAccessKey: "SK",
		SecurityToken:   "ST",
	})
	if req.Header.Get("Authorization") == "" {
		t.Fatal("missing Authorization")
	}
	if !strings.HasPrefix(req.Header.Get("Authorization"), "SDK-HMAC-SHA256 Access=AK, SignedHeaders=") {
		t.Fatalf("authz=%s", req.Header.Get("Authorization"))
	}
	if req.Header.Get("X-Sdk-Date") == "" || req.Header.Get("X-Security-Token") != "ST" {
		t.Fatalf("headers=%v", req.Header)
	}
}
