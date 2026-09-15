package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

func TestOAuthImportedCodeStoresRefreshableCredential(t *testing.T) {
	dpopSeen := make(chan string, 1)
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dpopSeen <- r.Header.Get("DPoP")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"user_id":"oauth-user","user_name":"tester","domain_id":"domain","refresh_token":"refresh-token","credentials":{"access_key_id":"ak","secret_access_key":"sk","security_token":"st","expiration":"2026-09-03T00:00:00Z"}}`)
	}))
	defer sts.Close()

	loginCfg := upstream.DefaultLoginConfig()
	loginCfg.STSHost = sts.URL
	p, err := pool.New(nil, pool.Config{LoginConfig: loginCfg}, "")
	if err != nil {
		t.Fatal(err)
	}
	authDir := t.TempDir()
	h := NewHandler(Config{
		APIKey: "panel-key", AuthDir: authDir, Listen: ":7866", Pool: p,
		OAuthClient: upstream.New(5 * time.Second), LoginConfig: loginCfg,
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

	callbackURL := "http://127.0.0.1:7866/oauth/callback?code=AUTH_CODE"
	body, _ := json.Marshal(map[string]string{"session_id": start.SessionID, "callback_url": callbackURL})
	importReq := httptest.NewRequest(http.MethodPost, "/admin/api/oauth/import-callback", bytes.NewReader(body))
	importReq.Header.Set("Authorization", "Bearer panel-key")
	importRec := httptest.NewRecorder()
	h.ServeHTTP(importRec, importReq)
	if importRec.Code != http.StatusOK {
		t.Fatalf("oauth code import status=%d body=%s", importRec.Code, importRec.Body.String())
	}
	var imported struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(importRec.Body.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.Status != "done" {
		t.Fatalf("oauth code import status=%q body=%s", imported.Status, importRec.Body.String())
	}
	if proof := <-dpopSeen; proof == "" {
		t.Fatal("authorization-code exchange omitted DPoP proof")
	}

	accounts, err := auth.LoadDir(authDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].Refresh() != "refresh-token" {
		t.Fatalf("stored credential is not refreshable: accounts=%d", len(accounts))
	}
	if jwk := accounts[0].DPoPPrivateJWK(); jwk["d"] == "" {
		t.Fatal("stored credential omitted the DPoP private key required for refresh")
	}
}

func TestOAuthDirectCallbackCompletesSessionForPanelPoll(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"user_id":"oauth-user","user_name":"tester","domain_id":"domain","refresh_token":"refresh-token","credentials":{"access_key_id":"ak","secret_access_key":"sk","security_token":"st","expiration":"2026-09-03T00:00:00Z"}}`)
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
	var start struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(startRec.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}

	callbackReq := httptest.NewRequest(http.MethodGet, "/oauth/callback?code=AUTH_CODE", nil)
	callbackRec := httptest.NewRecorder()
	h.ServeHTTP(callbackRec, callbackReq)
	if callbackRec.Code != http.StatusOK {
		t.Fatalf("oauth callback status=%d body=%s", callbackRec.Code, callbackRec.Body.String())
	}

	pollBody, _ := json.Marshal(map[string]string{"session_id": start.SessionID})
	pollReq := httptest.NewRequest(http.MethodPost, "/admin/api/oauth/poll", bytes.NewReader(pollBody))
	pollReq.Header.Set("Authorization", "Bearer panel-key")
	pollRec := httptest.NewRecorder()
	h.ServeHTTP(pollRec, pollReq)
	var polled struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(pollRec.Body.Bytes(), &polled); err != nil {
		t.Fatal(err)
	}
	if polled.Status != "done" {
		t.Fatalf("panel poll did not observe direct callback completion: status=%q body=%s", polled.Status, pollRec.Body.String())
	}
}

func TestOAuthPollDoesNotRaceAuthorizationCodeWithLegacyTicketFallback(t *testing.T) {
	var ticketPolls int
	snapManager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ticketPolls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"user_id":"legacy-user","user_name":"legacy","credential":{"access":"ak","secret":"sk","securitytoken":"st","expires_at":"2026-09-03T00:00:00Z"}}`)
	}))
	defer snapManager.Close()

	loginCfg := upstream.DefaultLoginConfig()
	loginCfg.SnapManager = snapManager.URL
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
	var start struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(startRec.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"session_id": start.SessionID})
	pollReq := httptest.NewRequest(http.MethodPost, "/admin/api/oauth/poll", bytes.NewReader(body))
	pollReq.Header.Set("Authorization", "Bearer panel-key")
	pollRec := httptest.NewRecorder()
	h.ServeHTTP(pollRec, pollReq)
	var polled struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(pollRec.Body.Bytes(), &polled); err != nil {
		t.Fatal(err)
	}
	if polled.Status != "pending" || ticketPolls != 0 {
		t.Fatalf("OAuth poll raced legacy ticket fallback: status=%q ticket_polls=%d body=%s", polled.Status, ticketPolls, pollRec.Body.String())
	}
}

func TestOAuthImportContinuesRemoteLoopbackCallback(t *testing.T) {
	h := NewHandler(Config{APIKey: "panel-key", AuthDir: t.TempDir(), Listen: ":7866"})

	startReq := httptest.NewRequest(http.MethodPost, "/admin/api/oauth/start", bytes.NewReader([]byte("{}")))
	startReq.Header.Set("Authorization", "Bearer panel-key")
	startRec := httptest.NewRecorder()
	h.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("oauth start status = %d, want 200: %s", startRec.Code, startRec.Body.String())
	}
	var start struct {
		SessionID string `json:"session_id"`
		AuthURL   string `json:"auth_url"`
	}
	if err := json.Unmarshal(startRec.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	authURL, err := url.Parse(start.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	ticketID := authURL.Query().Get("ticket_id")
	if start.SessionID == "" || ticketID == "" {
		t.Fatalf("oauth start omitted session/ticket: %+v", start)
	}

	nextURL := "https://codearts.huaweicloud.com/portal/callback?ticket_id=" + url.QueryEscape(ticketID)
	loopback := "http://127.0.0.1:7866/oauth/callback?secret=portal-secret&redirect=" + url.QueryEscape(nextURL)
	body, _ := json.Marshal(map[string]string{"session_id": start.SessionID, "callback_url": loopback})
	importReq := httptest.NewRequest(http.MethodPost, "/admin/api/oauth/import-callback", bytes.NewReader(body))
	importReq.Header.Set("Authorization", "Bearer panel-key")
	importRec := httptest.NewRecorder()
	h.ServeHTTP(importRec, importReq)

	if importRec.Code != http.StatusOK {
		t.Fatalf("oauth callback import status = %d, want 200: %s", importRec.Code, importRec.Body.String())
	}
	var imported struct {
		Status  string `json:"status"`
		NextURL string `json:"next_url"`
	}
	if err := json.Unmarshal(importRec.Body.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.Status != "continue" || imported.NextURL != nextURL {
		t.Errorf("oauth import = %+v, want status=continue and the portal continuation URL", imported)
	}
	if sess := h.oauth.get(start.SessionID); sess == nil || sess.Secret != "portal-secret" {
		t.Fatalf("oauth session portal secret was not updated")
	}
}

func TestOAuthImportRejectsNonHuaweiContinuation(t *testing.T) {
	h := NewHandler(Config{APIKey: "panel-key", AuthDir: t.TempDir(), Listen: ":7866"})
	sess := &oauthSession{ID: "session", TicketID: "ticket", Port: 7866, CreatedAt: time.Now()}
	h.oauth.put(sess)

	nextURL := "https://example.com/steal?ticket_id=ticket"
	loopback := "http://127.0.0.1:7866/oauth/callback?secret=portal-secret&redirect=" + url.QueryEscape(nextURL)
	body, _ := json.Marshal(map[string]string{"session_id": sess.ID, "callback_url": loopback})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/oauth/import-callback", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer panel-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oauth callback import status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}
