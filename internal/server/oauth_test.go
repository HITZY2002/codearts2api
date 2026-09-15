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

// portal 第二次回调不带 ticket_id，只带 code + secret。若匹配不到就回退到
// 「唯一活跃会话」，服务重启过或开过两次登录时会把 code 配给错误的 PKCE 密钥，
// 表现为 400 STS5.1805 invalid authorization code。
func TestCodeSessionMatchesBySecret(t *testing.T) {
	now := time.Now()
	st := newOAuthStore()
	st.put(&oauthSession{ID: "s-old", TicketID: "t-old", Secret: "secret-old", Verifier: "v-old", CreatedAt: now})
	st.put(&oauthSession{ID: "s-new", TicketID: "t-new", Secret: "secret-new", Verifier: "v-new", CreatedAt: now})

	// 1) secret 命中（portal 实际行为：回调不带 ticket_id）
	got := st.codeSession("", "secret-new")
	if got == nil || got.ID != "s-new" {
		t.Fatalf("应按 secret 命中 s-new，got=%v", got)
	}

	// 2) 带了 secret 但匹配不上 → 不得回退到唯一/其它会话
	if got := st.codeSession("", "secret-from-restarted-server"); got != nil {
		t.Fatalf("secret 匹配不上时必须放弃，got=%v(verifier=%s)", got, got.Verifier)
	}

	// 3) 无 secret 时用 ticket_id
	if got := st.codeSession("t-old", ""); got == nil || got.ID != "s-old" {
		t.Fatalf("应按 ticket_id 命中 s-old，got=%v", got)
	}

	// 4) 两者都没有且只有一个活跃会话 → 兜底
	st2 := newOAuthStore()
	st2.put(&oauthSession{ID: "only", Secret: "s1", CreatedAt: now})
	if got := st2.codeSession("", ""); got == nil || got.ID != "only" {
		t.Fatalf("唯一会话应兜底命中，got=%v", got)
	}

	// 5) 多个会话且无任何匹配键：portal 只回传 code（实测 has_secret=false），
	//    所以返回全部候选、由调用方逐个尝试，而不是猜一个（旧行为会错配 verifier）。
	st2.put(&oauthSession{ID: "second", Secret: "s2", CreatedAt: now.Add(time.Second)})
	cands := st2.codeSessionCandidates("", "")
	if len(cands) != 2 {
		t.Fatalf("应返回 2 个候选，got=%d", len(cands))
	}
	if cands[0].ID != "second" {
		t.Errorf("候选应按创建时间倒序（最新的优先），got[0]=%s", cands[0].ID)
	}
	// 已完成的会话不得再作为候选
	st2.complete("second")
	if cands := st2.codeSessionCandidates("", ""); len(cands) != 1 || cands[0].ID != "only" {
		t.Fatalf("已完成会话应被排除，got=%v", cands)
	}
}

// 复现线上故障（2026-09-16）：portal 回调只带 code（has_secret=false），
// 而生成链接的那个会话在服务重启后已丢失，只剩另一个无关会话。
// 旧实现会拿这个会话的 verifier 去换 → 400 STS5.1805 invalid authorization code。
// 新实现改为按创建时间倒序逐个候选尝试，直到某个 verifier 被 STS 接受。
func TestOAuthCallbackTriesAllCandidateSessions(t *testing.T) {
	const goodVerifier = "verifier-of-the-real-session"
	var attempts []string
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		v := r.Form.Get("code_verifier")
		attempts = append(attempts, v)
		w.Header().Set("Content-Type", "application/json")
		if v != goodVerifier {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error_code":"STS5.1805","error_msg":"invalid authorization code"}`)
			return
		}
		_, _ = io.WriteString(w, `{"user_id":"u-recover","user_name":"tester","domain_id":"d","refresh_token":"rt","credentials":{"access_key_id":"ak","secret_access_key":"sk","security_token":"st","expiration":"2026-09-16T00:00:00Z"}}`)
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
		APIKey: "k", AuthDir: authDir, Pool: p,
		OAuthClient: upstream.New(5 * time.Second), LoginConfig: loginCfg,
	})

	// 最新会话的 verifier 是错的（旧实现会直接挑它 → STS5.1805），
	// 真正能换到凭证的那个会话更早。新实现逐个尝试后应能成功。
	now := time.Now()
	h.oauth.put(&oauthSession{ID: "sb", TicketID: "tb", Secret: "sb-secret", Verifier: goodVerifier, CreatedAt: now.Add(-2 * time.Minute)})
	h.oauth.put(&oauthSession{ID: "sc", TicketID: "tc", Secret: "sc-secret", Verifier: "wrong-newest", CreatedAt: now.Add(-time.Minute)})

	// portal 实测只回传 code，没有 secret / ticket_id
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback?code=THE_CODE", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("回调应通过候选重试成功，status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(attempts) != 2 || attempts[0] != "wrong-newest" || attempts[1] != goodVerifier {
		t.Errorf("应先试最新会话、失败后回退到旧会话，got=%v", attempts)
	}
	if _, err := auth.LoadDir(authDir); err != nil {
		t.Fatalf("凭证应已落盘: %v", err)
	}
}
