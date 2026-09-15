package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type terminalErrorReader struct{ err error }

func (r terminalErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestStreamStopsReadingAfterDone(t *testing.T) {
	stream := io.MultiReader(
		strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"),
		terminalErrorReader{err: context.Canceled},
	)
	rec := httptest.NewRecorder()
	if err := Stream(rec, stream, "glm-5.2"); err != nil {
		t.Fatalf("stream read past [DONE]: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"content":"ok"`) || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("unexpected stream: %s", rec.Body.String())
	}
}

func TestStreamReturnsEmbeddedErrorAfterWritingTerminalEvent(t *testing.T) {
	stream := strings.NewReader("event: done\ndata: {\"error_code\":\"quota.429\",\"error_msg\":\"quota exceeded\"}\n\n")
	rec := httptest.NewRecorder()
	err := Stream(rec, stream, "glm-5.2")
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("embedded upstream error was reported as success: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "event: error") || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("client did not receive terminal error stream: %s", rec.Body.String())
	}
}

func TestSendChatV2SurfacesEmbeddedQueueError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"text\":\"[DONE]\",\"error_code\":\"InferHub.ModelArts.81111.429\",\"error_msg\":\"TPM limit reached\"}\n\n"))
	}))
	defer srv.Close()

	c := NewWithChatEndpoint(5*time.Second, srv.URL)
	_, err := c.SendChatV2(context.Background(), map[string]any{
		"model": "GLM-5.2", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, "trace", SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "token"}, "token", false)
	var apiErr *ApiError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests || !strings.Contains(apiErr.Message, "81111.429") {
		t.Fatalf("embedded queue error was not surfaced as retryable: %T %v", err, err)
	}
}

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

func TestChatBodyV2CarriesBuiltInAgentChatID(t *testing.T) {
	body := chatBodyV2(
		"0123456789abcdef0123456789abcdef",
		[]ChatMessage{{Type: "text", Text: "continue after tool result"}},
		"glm-5.2",
		ChatOptions{},
	)
	if body["chat_id"] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("chat_id missing from upstream body: %#v", body)
	}
	if body["model"] != "GLM-5.2" {
		t.Fatalf("model not canonicalized: %#v", body)
	}
}

func TestBuildAuthorizeURLUsesRefreshableOAuthParameters(t *testing.T) {
	cfg := DefaultLoginConfig()
	got, err := url.Parse(New(time.Second).BuildAuthorizeURL(cfg, "ticket", "challenge", "S256", 43123))
	if err != nil {
		t.Fatal(err)
	}
	query := got.Query()
	want := map[string]string{
		"theme": "2", "locale": "zh-cn", "uri_scheme": "codearts-agent",
		"client_id": "codearts-agent", "port": "43123", "ticket_id": "ticket",
		"code_challenge": "challenge", "code_challenge_method": "SHA-256",
		"plugin-name": "snap_AIIDE", "plugin-version": "5.2.0",
	}
	for key, expected := range want {
		if actual := query.Get(key); actual != expected {
			t.Fatalf("authorize query %s=%q, want %q; url=%s", key, actual, expected, got.String())
		}
	}
}

func TestOAuthRefreshReusesPersistedDPoPKey(t *testing.T) {
	proofs := make([]string, 0, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proofs = append(proofs, r.Header.Get("DPoP"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"user_id":"u1","user_name":"n1","refresh_token":"rt2","credentials":{"access_key_id":"ak","secret_access_key":"sk","security_token":"st","expiration":"2026-09-03T00:00:00Z"}}`)
	}))
	defer srv.Close()

	privateJWK, err := NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	c := New(5 * time.Second)
	cfg := DefaultLoginConfig()
	cfg.STSHost = srv.URL
	if _, err := c.ExchangeCode(context.Background(), cfg, "code", "verifier", 9999, privateJWK); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RefreshToken(context.Background(), cfg, "rt", "verifier", privateJWK); err != nil {
		t.Fatal(err)
	}
	if len(proofs) != 2 {
		t.Fatalf("DPoP proofs=%d, want 2", len(proofs))
	}

	firstHeader, firstPayload := decodeDPoP(t, proofs[0])
	secondHeader, secondPayload := decodeDPoP(t, proofs[1])
	firstJWK, _ := firstHeader["jwk"].(map[string]any)
	secondJWK, _ := secondHeader["jwk"].(map[string]any)
	for _, field := range []string{"kty", "crv", "x", "y"} {
		if firstJWK[field] != privateJWK[field] || secondJWK[field] != privateJWK[field] {
			t.Fatalf("DPoP public JWK field %s was not restored from persisted key", field)
		}
	}
	if firstPayload["jti"] == secondPayload["jti"] {
		t.Fatal("each DPoP proof must use a fresh jti")
	}
	if len(firstPayload["jti"].(string)) != 64 || len(secondPayload["jti"].(string)) != 64 {
		t.Fatalf("DPoP jti must be 32 random bytes: first=%q second=%q", firstPayload["jti"], secondPayload["jti"])
	}
}

func TestOAuthRefreshClassifiesExpiredRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_code":"IAM.ExpiredRefreshToken","error_msg":"refresh token expired"}`)
	}))
	defer srv.Close()
	privateJWK, err := NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultLoginConfig()
	cfg.STSHost = srv.URL
	_, err = New(5*time.Second).RefreshToken(context.Background(), cfg, "expired", "verifier", privateJWK)
	var expired *RefreshTokenExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("refresh error=%T %v, want RefreshTokenExpiredError", err, err)
	}
}

func decodeDPoP(t *testing.T, proof string) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("invalid DPoP proof: %q", proof)
	}
	decode := func(part string) map[string]any {
		raw, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	return decode(parts[0]), decode(parts[1])
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
	privateJWK, err := NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.ExchangeCode(context.Background(), cfg, "code", "verifier", 9999, privateJWK)
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
