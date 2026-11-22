package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/telemetry"
	"codearts2api/internal/upstream"
)

type keepaliveClientFunc func(context.Context, upstream.SignCredential) ([]upstream.CreditCampaign, error)

func (f keepaliveClientFunc) FetchCreditCampaigns(ctx context.Context, cred upstream.SignCredential) ([]upstream.CreditCampaign, error) {
	return f(ctx, cred)
}

func TestHasStreamOutputIgnoresEmptyBootstrapChunk(t *testing.T) {
	bootstrap := []byte(`data: {"choices":[{"delta":{"content":"","role":"assistant"},"index":0}]}` + "\n\n")
	if hasStreamOutput(bootstrap) {
		t.Fatal("empty bootstrap chunk must not trigger TTFT")
	}
	nullTools := []byte(`data: {"choices":[{"delta":{"tool_calls":null},"index":0}]}` + "\n\n")
	if hasStreamOutput(nullTools) {
		t.Fatal("null tool_calls must not trigger TTFT")
	}
	realToken := []byte(`data: {"choices":[{"delta":{"content":"hi"},"index":0}]}` + "\n\n")
	if !hasStreamOutput(realToken) {
		t.Fatal("non-empty content delta must trigger TTFT")
	}
	reasoning := []byte(`data: {"choices":[{"delta":{"reasoning_content":"think"},"index":0}]}` + "\n\n")
	if !hasStreamOutput(reasoning) {
		t.Fatal("non-empty reasoning_content delta must trigger TTFT")
	}
}

func TestClientIPTrustsOnlyLocalOrPrivateProxy(t *testing.T) {
	pubReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	pubReq.RemoteAddr = "203.0.113.9:12345"
	pubReq.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := clientIP(pubReq); got != "203.0.113.9" {
		t.Fatalf("untrusted public peer spoofed clientIP: got %q", got)
	}

	proxyReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxyReq.RemoteAddr = "127.0.0.1:54321"
	proxyReq.Header.Set("X-Forwarded-For", "198.51.100.7, 127.0.0.1")
	if got := clientIP(proxyReq); got != "198.51.100.7" {
		t.Fatalf("trusted loopback proxy failed to forward clientIP: got %q", got)
	}
}

func TestBuildUpstreamMessagesPreservesDeveloperRole(t *testing.T) {
	req, err := parseChatRequest([]byte(`{"model":"glm-5.2","messages":[{"role":"developer","content":"follow rules"},{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	msgs := buildUpstreamMessages(req, false)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != "system" || msgs[0].Content != "follow rules" {
		t.Fatalf("developer message was not mapped to system: %#v", msgs[0])
	}
}

func TestAdminUnknownUIDReturns404AndClampedPagination(t *testing.T) {
	store, err := telemetry.Open(filepath.Join(t.TempDir(), "obs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first := int64(250)
	_ = store.RecordRequest(telemetry.RequestLog{
		RequestID:     "r1",
		CreatedAt:     time.Now().UnixMilli(),
		Endpoint:      "/v1/chat/completions",
		Method:        "POST",
		StatusCode:    200,
		Model:         "glm-5.2",
		FirstOutputMs: &first,
		TotalMs:       600,
	})
	_ = store.Flush()

	// MetricsOverview with to=0 must still compute non-zero percentiles.
	ov, err := store.MetricsOverview(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ov.TTFTP50Ms != 250 || ov.TotalP50Ms != 600 {
		t.Fatalf("MetricsOverview(0,0) percentiles broken: %#v", ov)
	}

	a := auth.New("uid-known", "tester", "dom", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, APIKey: testHandlerAPIKey, Telemetry: store})

	// Unknown UID should return 404 across admin endpoints.
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/admin/api/credits/balance?uid=missing", ""},
		{http.MethodGet, "/admin/api/checkin/status?uid=missing", ""},
		{http.MethodPost, "/admin/api/credits", `{"uid":"missing"}`},
		{http.MethodPost, "/admin/api/keepalive", `{"uid":"missing"}`},
	} {
		rec := httptest.NewRecorder()
		req := authenticatedTestRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s status=%d want 404 body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	// Unclamped limit/offset must be echoed as clamped values.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authenticatedTestRequest(http.MethodGet, "/admin/api/logs?limit=9999&offset=-5", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("logs status=%d", rec.Code)
	}
	var resp struct {
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Limit != 500 || resp.Offset != 0 {
		t.Fatalf("got limit=%d offset=%d want limit=500 offset=0", resp.Limit, resp.Offset)
	}
}

func TestAdminKeepaliveCallsUpstreamPing(t *testing.T) {
	pinged := false
	client := keepaliveClientFunc(func(ctx context.Context, cred upstream.SignCredential) ([]upstream.CreditCampaign, error) {
		pinged = true
		return nil, nil
	})

	a := auth.New("uid-ka", "tester", "dom", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{
		MaxConcurrent:   1,
		KeepaliveClient: client,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, APIKey: testHandlerAPIKey})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authenticatedTestRequest(http.MethodPost, "/admin/api/keepalive", bytes.NewBufferString(`{"uid":"uid-ka"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("keepalive status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !pinged {
		t.Fatal("POST /admin/api/keepalive did not perform upstream keepalive ping")
	}
}
