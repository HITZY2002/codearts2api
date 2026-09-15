package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"codearts2api/internal/upstream"
)

// CodeArts OAuth（与 cmd/login / 旧 uiLogin 一致）：ticket + PKCE，服务端轮询 snap-manager。
const oauthSessionTTL = 15 * time.Minute

type oauthSession struct {
	ID             string
	TicketID       string
	Secret         string
	Verifier       string
	DPoPPrivateKey upstream.DPoPPrivateJWK
	Port           int
	AuthURL        string
	CreatedAt      time.Time
	Done           bool
	Err            string
	TicketFallback bool
}

type oauthStore struct {
	mu   sync.Mutex
	byID map[string]*oauthSession
}

func newOAuthStore() *oauthStore {
	return &oauthStore{byID: map[string]*oauthSession{}}
}

func (s *oauthStore) put(sess *oauthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.byID[sess.ID] = sess
}

func (s *oauthStore) get(id string) *oauthSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	return s.byID[id]
}

func (s *oauthStore) getByTicket(tid string) *oauthSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	for _, sess := range s.byID {
		if sess.TicketID == tid {
			return sess
		}
	}
	return nil
}

func (s *oauthStore) enableTicketFallbackByTicket(ticketID, secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.byID {
		if sess.TicketID == ticketID {
			sess.Secret = secret
			sess.TicketFallback = true
			return
		}
	}
}

func (s *oauthStore) enableTicketFallbackByID(id, secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.byID[id]; sess != nil {
		sess.Secret = secret
		sess.TicketFallback = true
	}
}

func (s *oauthStore) complete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.byID[id]; sess != nil {
		sess.Done = true
		sess.Err = ""
	}
}

// codeSession 在回调不带 ticket_id 时只接受唯一的活跃 OAuth 会话，
// 避免共享回调端口把 authorization code 配给错误的 PKCE/DPoP 密钥。
func (s *oauthStore) codeSession(ticketID string) *oauthSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	if ticketID != "" {
		for _, sess := range s.byID {
			if sess.TicketID == ticketID && !sess.Done {
				return sess
			}
		}
		return nil
	}
	var match *oauthSession
	for _, sess := range s.byID {
		if sess.Done {
			continue
		}
		if match != nil {
			return nil
		}
		match = sess
	}
	return match
}

func (s *oauthStore) del(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

func (s *oauthStore) gcLocked() {
	now := time.Now()
	for id, sess := range s.byID {
		if now.Sub(sess.CreatedAt) > oauthSessionTTL {
			delete(s.byID, id)
		}
	}
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *Handler) listenPort() int {
	s := h.cfg.Listen
	if i := strings.LastIndex(s, ":"); i >= 0 && i+1 < len(s) {
		if p, err := strconv.Atoi(s[i+1:]); err == nil && p > 0 {
			return p
		}
	}
	return 7866
}

// adminOAuthStart 发起 CodeArts 授权。
func (h *Handler) adminOAuthStart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AuthDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "auth_dir 未配置"})
		return
	}
	ticketID, err := upstream.RandomHex(16)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	secret, err := upstream.RandomHex(16)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	verifier, challenge, err := upstream.PKCE()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	dpopPrivateJWK, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	port := h.listenPort()
	cfg := h.cfg.LoginConfig
	authURL := h.cfg.OAuthClient.BuildAuthorizeURL(cfg, ticketID, challenge, "SHA-256", port)
	// 可选：把回调端口改写为公网反代地址的端口，让浏览器回调尽量命中 hub；
	// 远端若仍无法回调则回退到 ticket 轮询通道，不影响登录完成。
	if h.cfg.OAuthCallbackHost != "" {
		if p := portOfCallbackHost(h.cfg.OAuthCallbackHost); p != 0 {
			authURL = rewriteAuthURLPort(authURL, p)
		}
	}

	id := newSessionID()
	h.oauth.put(&oauthSession{
		ID: id, TicketID: ticketID, Secret: secret, Verifier: verifier, Port: port,
		DPoPPrivateKey: dpopPrivateJWK, AuthURL: authURL, CreatedAt: time.Now(),
	})
	// 兼容旧 loginMu 回调路径（/oauth/callback 会更新 portal secret）
	h.loginMu.Lock()
	h.logins[ticketID] = &pendingLogin{TicketID: ticketID, Secret: secret, Verifier: verifier, Port: port}
	h.loginMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"session_id": id,
		"auth_url":   authURL,
		"expires_in": int(oauthSessionTTL.Seconds()),
		"message":    "请在浏览器打开授权链接，完成后点「我已授权」或等待自动检测",
	})
}

// adminOAuthPoll 轮询 ticket。
func (h *Handler) adminOAuthPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	if req.SessionID == "" {
		req.SessionID = r.URL.Query().Get("session_id")
	}
	if req.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "status": "error", "message": "session_id required"})
		return
	}
	sess := h.oauth.get(req.SessionID)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "status": "error", "message": "会话不存在或已过期，请重新发起授权"})
		return
	}
	if sess.Done {
		if sess.Err != "" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": "error", "message": sess.Err})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "done", "message": "登录成功"})
		return
	}
	// 新式 OAuth 必须等待 authorization code；只有 Portal 明确回调
	// secret 时才进入旧 ticket 回退。否则并行轮询会抢先保存一份
	// 不含 refresh_token 的临时凭据。
	if !sess.TicketFallback {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "status": "pending", "message": "等待浏览器返回 OAuth 授权码…",
		})
		return
	}

	// 同步 loginMu 里可能被 callback 更新的 portal secret
	h.loginMu.Lock()
	if p, ok := h.logins[sess.TicketID]; ok && p.Secret != "" {
		sess.Secret = p.Secret
	}
	h.loginMu.Unlock()

	tok, err := h.cfg.OAuthClient.PollTicket(context.Background(), h.cfg.LoginConfig, sess.TicketID, sess.Secret)
	if err != nil || tok == nil || tok.UserName == "" || tok.Credentials.SecurityToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "status": "pending", "message": "等待浏览器完成登录…",
		})
		return
	}
	if err := h.saveLoginResult(tok, sess.Verifier); err != nil {
		sess.Done = true
		sess.Err = err.Error()
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": "error", "message": err.Error()})
		return
	}
	sess.Done = true
	h.oauth.del(req.SessionID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"status":  "done",
		"message": fmt.Sprintf("登录成功：%s (%s)", nonempty(tok.UserName, "未命名"), shortID(tok.UserID)),
		"account": map[string]any{"uid": tok.UserID, "nickname": tok.UserName},
	})
}

// adminOAuthImportCallback bridges the loopback callback used by the native
// CodeArts OAuth flow when the browser is not running on the proxy host. The
// operator pastes the failed 127.0.0.1 callback URL; the server validates it
// against the active session, records the portal secret, and returns the next
// Huawei URL for the browser to continue.
func (h *Handler) adminOAuthImportCallback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"session_id"`
		CallbackURL string `json:"callback_url"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
	if err := json.Unmarshal(raw, &req); err != nil || strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.CallbackURL) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "session_id 和 callback_url 必填"})
		return
	}
	sess := h.oauth.get(req.SessionID)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "会话不存在或已过期，请重新发起授权"})
		return
	}
	callback, err := url.Parse(strings.TrimSpace(req.CallbackURL))
	if err != nil || callback.Scheme != "http" || callback.User != nil || callback.Path != "/oauth/callback" || !loopbackCallbackHost(callback.Hostname()) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "只接受华为返回的 127.0.0.1 OAuth 回调地址"})
		return
	}
	if callback.Port() != strconv.Itoa(sess.Port) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "回调端口与当前授权会话不匹配"})
		return
	}
	secret := callback.Query().Get("secret")
	if code := callback.Query().Get("code"); code != "" {
		tok, exchangeErr := h.cfg.OAuthClient.ExchangeCode(r.Context(), h.cfg.LoginConfig, code, sess.Verifier, sess.Port, sess.DPoPPrivateKey)
		if exchangeErr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "status": "error", "message": exchangeErr.Error()})
			return
		}
		if err := h.saveLoginResult(tok, sess.Verifier, sess.DPoPPrivateKey); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "status": "error", "message": err.Error()})
			return
		}
		sess.Done = true
		h.oauth.del(req.SessionID)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "status": "done", "message": "OAuth 登录成功，已保存可自动续期凭据",
			"account": map[string]any{"uid": tok.UserID, "nickname": tok.UserName},
		})
		return
	}
	nextURL := callback.Query().Get("redirect")
	next, err := url.Parse(nextURL)
	portal, portalErr := url.Parse(h.cfg.LoginConfig.PortalHost)
	if secret == "" || err != nil || portalErr != nil || next.Scheme != "https" || next.User != nil ||
		!strings.EqualFold(next.Host, portal.Host) || next.Query().Get("ticket_id") != sess.TicketID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "回调地址缺少当前会话的授权信息"})
		return
	}

	h.oauth.enableTicketFallbackByID(req.SessionID, secret)
	h.loginMu.Lock()
	if pending, ok := h.logins[sess.TicketID]; ok {
		pending.Secret = secret
	}
	h.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "status": "continue", "next_url": nextURL,
		"message": "回调已接收，请继续打开华为授权页并等待自动检测",
	})
}

func loopbackCallbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func nonempty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func shortID(u string) string {
	if len(u) <= 12 {
		return u
	}
	return u[:8] + "…"
}

// portOfCallbackHost 从 host[:port] 提取端口，支持带 scheme 的地址。
// portOfCallbackHost 取出 oauth_callback_host 里显式写出的端口，没有则返回 0。
//
// 只认显式端口、不按 scheme 推断 443/80：portal 的回调目标是 127.0.0.1:{port}，
// 若在没写端口时擅自改成 443，本机浏览器的回调用例反而会失败（这是本仓库
// 之前的实际行为：带路径的写法一律解析失败，配置静默失效）。
// 需要走反代请在配置里写全端口，例如 https://example.com:443/codearts。
func portOfCallbackHost(host string) int {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return 0
	}
	if u, err := url.Parse(trimmed); err == nil && u.Host != "" {
		if p := u.Port(); p != "" {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				return n
			}
		}
		return 0
	}
	// 兼容 "example.com:8443" 这种没有 scheme 的写法。
	if i := strings.LastIndex(trimmed, ":"); i >= 0 {
		seg := strings.TrimRight(trimmed[i+1:], "/")
		if n, err := strconv.Atoi(seg); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// rewriteAuthURLPort 改写授权链接的 port 参数（华为 portal 据此拼回调地址）。
func rewriteAuthURLPort(authURL string, port int) string {
	u, err := url.Parse(authURL)
	if err != nil {
		return authURL
	}
	q := u.Query()
	q.Set("port", fmt.Sprint(port))
	u.RawQuery = q.Encode()
	return u.String()
}
