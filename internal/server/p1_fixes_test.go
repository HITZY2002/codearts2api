package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/telemetry"
	"codearts2api/internal/upstream"
)

func TestOAuthCallbackEscapesHTMLAndSetsCSP(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `<script>alert("xss")</script>`)
	}))
	defer sts.Close()

	loginCfg := upstream.DefaultLoginConfig()
	loginCfg.STSHost = sts.URL
	p, err := pool.New(nil, pool.Config{LoginConfig: loginCfg}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{
		APIKey: "panel-key", AuthDir: t.TempDir(), Listen: ":7866", Pool: p,
		OAuthClient: upstream.New(5 * time.Second), LoginConfig: loginCfg,
	})

	startReq := httptest.NewRequest(http.MethodPost, "/admin/api/oauth/start", bytes.NewReader([]byte("{}")))
	startReq.Header.Set("Authorization", "Bearer panel-key")
	startRec := httptest.NewRecorder()
	h.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("oauth start status=%d", startRec.Code)
	}

	cbReq := httptest.NewRequest(http.MethodGet, "/oauth/callback?code=BAD", nil)
	cbRec := httptest.NewRecorder()
	h.ServeHTTP(cbRec, cbReq)
	if cbRec.Code != http.StatusBadGateway {
		t.Fatalf("callback status=%d want 502", cbRec.Code)
	}
	if csp := cbRec.Header().Get("Content-Security-Policy"); csp != "default-src 'none'" {
		t.Fatalf("CSP=%q want default-src 'none'", csp)
	}
	body := cbRec.Body.String()
	if strings.Contains(body, "<script>") {
		t.Fatalf("unescaped script tag in callback HTML: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("expected escaped script tag in callback HTML: %s", body)
	}
}

func TestOAuthCallbackHostUpdatesSessionPort(t *testing.T) {
	loginCfg := upstream.DefaultLoginConfig()
	p, err := pool.New(nil, pool.Config{LoginConfig: loginCfg}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{
		APIKey: "panel-key", AuthDir: t.TempDir(), Listen: ":7866", Pool: p,
		OAuthClient:       upstream.New(5 * time.Second),
		LoginConfig:       loginCfg,
		OAuthCallbackHost: "https://proxy.example.com:8443/codearts",
	})

	startReq := httptest.NewRequest(http.MethodPost, "/admin/api/oauth/start", bytes.NewReader([]byte("{}")))
	startReq.Header.Set("Authorization", "Bearer panel-key")
	startRec := httptest.NewRecorder()
	h.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("oauth start status=%d body=%s", startRec.Code, startRec.Body.String())
	}
	var start struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(startRec.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	sess := h.oauth.get(start.SessionID)
	if sess == nil {
		t.Fatal("session missing")
	}
	if sess.Port != 8443 {
		t.Fatalf("sess.Port=%d want 8443", sess.Port)
	}
}

func TestErrorTypeNotOverwrittenAndClearedOnRetrySuccess(t *testing.T) {
	store, err := telemetry.Open(filepath.Join(t.TempDir(), "obs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Case 1: 500 error must keep error_type="upstream_error" (not "model_access").
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	defer srv500.Close()

	a1 := auth.New("err-a1", "u1", "dom", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p1, err := pool.New([]*auth.Auth{a1}, pool.Config{MaxConcurrent: 1, ErrThreshold: 100}, "")
	if err != nil {
		t.Fatal(err)
	}
	useFreshTestCatalogs(p1)
	p1.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, srv500.URL)
	h1 := NewHandler(Config{Pool: p1, APIKey: testHandlerAPIKey, DefaultModel: "glm-5.2", MaxRotate: 1, Telemetry: store})

	rec1 := httptest.NewRecorder()
	h1.ServeHTTP(rec1, authenticatedTestRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec1.Code != http.StatusServiceUnavailable {
		t.Fatalf("case1 status=%d want 503", rec1.Code)
	}

	// Case 2: model access denied on account A, then rotation succeeds on account B -> error_type must be "".
	srvDeny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: error\ndata: {\"code\":\"TM.000010051\",\"message\":\"无权调用该模型\"}\n\n")
	}))
	defer srvDeny.Close()
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srvOK.Close()

	a2 := auth.New("rot-a", "ua", "dom", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	b2 := auth.New("rot-b", "ub", "dom", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p2, err := pool.New([]*auth.Auth{a2, b2}, pool.Config{MaxConcurrent: 1, ErrThreshold: 100}, "")
	if err != nil {
		t.Fatal(err)
	}
	useFreshTestCatalogs(p2)
	p2.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, srvDeny.URL)
	p2.Accounts()[1].Client = upstream.NewWithChatEndpoint(5*time.Second, srvOK.URL)
	h2 := NewHandler(Config{Pool: p2, APIKey: testHandlerAPIKey, DefaultModel: "glm-5.2", MaxRotate: 3, Telemetry: store})

	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, authenticatedTestRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("case2 status=%d want 200 body=%s", rec2.Code, rec2.Body.String())
	}

	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	items, total, err := store.ListRequests(telemetry.RequestFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("got %d logs, want 2", total)
	}
	// Ordered DESC by created_at: items[0] is case2 (200), items[1] is case1 (503).
	var got200, got503 *telemetry.RequestLog
	for i := range items {
		if items[i].StatusCode == 200 {
			got200 = &items[i]
		}
		if items[i].StatusCode == 503 {
			got503 = &items[i]
		}
	}
	if got503 == nil || got503.ErrorType != "upstream_error" {
		t.Fatalf("case1 503 log error_type=%q want upstream_error", got503.ErrorType)
	}
	if got200 == nil || got200.ErrorType != "" {
		t.Fatalf("case2 200 log error_type=%q want empty", got200.ErrorType)
	}
}
