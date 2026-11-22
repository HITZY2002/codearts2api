// Package server 暴露 OpenAI 兼容接口：/v1/chat/completions、/v1/models、/status、WebUI。
package server

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/scheduler"
	"codearts2api/internal/telemetry"
	"codearts2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	APIKey       string
	MaxRotate    int
	SoftCooldown time.Duration
	ErrThreshold int
	ErrCooldown  time.Duration
	// CodeArts 上游把并发会话/TPM 排队既可能返回 HTTP 400/429，
	// 也可能嵌在 HTTP 200 SSE 内。默认每 10s 重试一次、最多 30 次（约 5 分钟）。
	QueueRetryDelay  time.Duration
	QueueMaxAttempts int
	// QueueReacquireTimeout bounds the wait for a slot after a queue retry.
	QueueReacquireTimeout time.Duration
	DefaultModel          string
	ConvStateFile         string
	WatchInfo             map[string]any
	AuthDir               string
	Listen                string
	// OAuthClient 短超时客户端，用于 WebUI 登录换取 token（可注入测试端点）。
	OAuthClient *upstream.Client
	// Telemetry 可选 SQLite 观测存储。
	Telemetry                  *telemetry.Store
	ObservabilityRetentionDays int
	ObservabilityMaxRows       int
	ObservabilityMaxDBBytes    int64
	SnapshotInterval           time.Duration
	// LoginConfig WebUI 登录配置（client_id 以官方客户端为准）。
	LoginConfig upstream.LoginConfig
	// OAuthCallbackHost 可选：覆盖授权链接回调 host（如 https://oneapi.example.com/codearts）。
	OAuthCallbackHost string
	// Settings 是 WebUI 设置页的运行时配置存储。
	Settings *SettingsStore
	// Scheduler allows settings changes to reach the token watchdog.
	Scheduler *scheduler.Scheduler
}

var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

// staticModel 静态兜底模型：动态发现失败时 /v1/models 仍能列出实测可用模型。
type staticModel struct {
	ID            string
	ContextWindow int64
	Benefit       bool
}

// staticModels 静态兜底表：**只放实测确认可用的模型**。
//
// 上游的模型发现（agent-center / builtin）会列出当前账号未注册的模型，静态表
// 若照抄发现结果，就等于把 404 写进了兜底路径。以下清单来自 2026-09-15 真实账号
// 逐个调用验证（见 docs/reverse-engineering.md §7）：
//
//	✅ GLM-5.2、glm-5.2-sft-harmony、Qwen3-VL-235B
//	❌ GLM-5.2-ArkTS-SPARK、OpenPangu-2.0-Pro、OpenPangu-2.0-Flash（002002009.404）
var staticModels = []staticModel{
	{ID: "GLM-5.2", ContextWindow: 202752},
	{ID: "glm-5.2-sft-harmony", ContextWindow: 131072},
	{ID: "Qwen3-VL-235B", ContextWindow: 131072},
	// 限时福利（需领取；领取后实测可用，聊天自动带 maas_type: benefit）
	{ID: "deepseek-v4-flash-0731", ContextWindow: 1048576, Benefit: true},
	{ID: "deepseek-v4-pro-0813", ContextWindow: 1048576, Benefit: true},
	{ID: "glm-5.3-flash", ContextWindow: 1048576, Benefit: true},
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

const maxBodyBytes = 8 << 20

const (
	maxConversationBindings = 4096
	conversationBindingTTL  = 7 * 24 * time.Hour
)

type conversationBinding struct {
	Account   string    `json:"account"`
	UpdatedAt time.Time `json:"updated_at"`
}

type turnIDBinding struct {
	ID        string    `json:"id"`
	UpdatedAt time.Time `json:"updated_at"`
}

type conversationState struct {
	Conversations map[string]conversationBinding `json:"conversations"`
	Turns         map[string]turnIDBinding       `json:"turns,omitempty"`
}

// Handler 主路由。
type Handler struct {
	cfg   Config
	mux   *http.ServeMux
	oauth *oauthStore

	// cfgMu 保护设置页热更新的 cfg 可变字段（DefaultModel、QueueRetryDelay、
	// QueueMaxAttempts、WatchInfo）。请求路径只读，applySettings 写入。
	cfgMu sync.RWMutex

	convMu sync.Mutex
	// 黏性路由：conversation_id → account_name（多轮续接锁定同一账号，减少上游并发会话占用）。
	convAcct map[string]conversationBinding
	// 上游消息 ID 按账号 + 用户轮次复用；跨账号不复用，避免积分归属错误。
	turnIDs map[string]turnIDBinding
	// 会话状态延迟合并写盘：热路径只改内存，200ms 后由一次性定时器落盘。
	convDirty        bool
	convFlushPending bool
	convFileMu       sync.Mutex
}

// runtimeTuning 读取可热更新的请求参数快照。
func (h *Handler) runtimeTuning() (defaultModel string, queueDelay time.Duration, queueAttempts int) {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	return h.cfg.DefaultModel, h.cfg.QueueRetryDelay, h.cfg.QueueMaxAttempts
}

// watchInfoSnapshot 返回 WatchInfo 的副本，避免与 applySettings 并发读写同一 map。
func (h *Handler) watchInfoSnapshot() map[string]any {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	out := make(map[string]any, len(h.cfg.WatchInfo))
	for k, v := range h.cfg.WatchInfo {
		out[k] = v
	}
	return out
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.Settings == nil {
		cfg.Settings = newSettingsStore("", fallbackSettings(cfg))
	}
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.QueueRetryDelay <= 0 {
		cfg.QueueRetryDelay = 10 * time.Second
	}
	if cfg.QueueMaxAttempts <= 0 {
		// 上游并发/TPM 排队是瞬时的，但 180×10s = 30 分钟对交互式调用等于挂死；
		// 取 30 次（约 5 分钟）作为默认，可用 queue_max_attempts 调整。
		cfg.QueueMaxAttempts = 30
	}
	if cfg.QueueReacquireTimeout <= 0 {
		cfg.QueueReacquireTimeout = 30 * time.Second
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = "glm-5.2"
	}
	if cfg.Upstream == nil {
		cfg.Upstream = upstream.New(120 * time.Second)
	}
	if cfg.OAuthClient == nil {
		cfg.OAuthClient = upstream.New(15 * time.Second)
	}
	if cfg.LoginConfig.ClientID == "" {
		cfg.LoginConfig = upstream.DefaultLoginConfig()
	}
	h := &Handler{
		cfg: cfg, mux: http.NewServeMux(), oauth: newOAuthStore(),
		convAcct: map[string]conversationBinding{},
		turnIDs:  map[string]turnIDBinding{},
	}
	h.loadConversations()
	h.mux.HandleFunc("POST /v1/chat/completions", h.withTelemetry(h.withAuth(h.chatCompletions)))
	h.mux.HandleFunc("GET /v1/models", h.withTelemetry(h.withAuth(h.models)))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// WorkBuddy 风格控制台（页面不鉴权，API 走 Bearer）
	h.mux.HandleFunc("GET /{$}", h.servePanel)
	h.mux.HandleFunc("GET /admin", h.servePanel)
	h.mux.HandleFunc("GET /panel", h.servePanel)
	h.mux.HandleFunc("GET /panel/", h.servePanel)
	h.mux.HandleFunc("GET /assets/", h.servePanelAssets)
	h.mux.HandleFunc("GET /admin/api/overview", h.withAuth(h.adminOverview))
	h.mux.HandleFunc("POST /admin/api/credits", h.withAuth(h.adminCredits))
	h.mux.HandleFunc("GET /admin/api/credits/balance", h.withAuth(h.adminCreditBalances))
	h.mux.HandleFunc("POST /admin/api/checkin", h.withAuth(h.adminCheckin))
	h.mux.HandleFunc("GET /admin/api/checkin/status", h.withAuth(h.adminCheckinStatuses))
	h.mux.HandleFunc("POST /admin/api/keepalive", h.withAuth(h.adminKeepalive))
	h.mux.HandleFunc("POST /admin/api/reload", h.withAuth(h.adminReload))
	h.mux.HandleFunc("POST /admin/api/accounts/enable", h.withAuth(h.adminEnable))
	h.mux.HandleFunc("POST /admin/api/accounts/disable", h.withAuth(h.adminDisable))
	h.mux.HandleFunc("POST /admin/api/accounts/clear-cooldown", h.withAuth(h.adminClearCooldown))
	h.mux.HandleFunc("POST /admin/api/oauth/start", h.withAuth(h.adminOAuthStart))
	h.mux.HandleFunc("POST /admin/api/oauth/poll", h.withAuth(h.adminOAuthPoll))
	h.mux.HandleFunc("POST /admin/api/oauth/import-callback", h.withAuth(h.adminOAuthImportCallback))
	h.mux.HandleFunc("GET /admin/api/logs", h.withAuth(h.adminLogs))
	h.mux.HandleFunc("GET /admin/api/events", h.withAuth(h.adminEvents))
	h.mux.HandleFunc("GET /admin/api/metrics/overview", h.withAuth(h.adminMetricsOverview))
	h.mux.HandleFunc("GET /admin/api/metrics/series", h.withAuth(h.adminMetricsSeries))
	h.mux.HandleFunc("POST /admin/api/observability/purge", h.withAuth(h.adminObservabilityPurge))
	h.mux.HandleFunc("GET /admin/api/settings", h.withAuth(h.adminSettingsGet))
	h.mux.HandleFunc("POST /admin/api/settings", h.withAuth(h.adminSettingsUpdate))
	h.mux.HandleFunc("GET /oauth/callback", h.oauthCallback)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey == "" {
			writeOpenAIError(w, http.StatusServiceUnavailable, "server_misconfigured", "API key is not configured")
			return
		}
		authz := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		key := authz[len(prefix):]
		if subtle.ConstantTimeCompare([]byte(key), []byte(h.cfg.APIKey)) != 1 {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": h.cfg.Pool.List()})
}

// oauthCallback 本地回调：浏览器同机时由 portal 携带 code 跳到这里。
func (h *Handler) oauthCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	secret := r.URL.Query().Get("secret")
	redirect := r.URL.Query().Get("redirect")
	log.Printf("oauth callback hit: has_code=%t has_secret=%t has_redirect=%t", code != "", secret != "", redirect != "")
	// portal 第一次回调：带 secret + redirect，要求 307 跳转（登录页链路的一部分）。
	if secret != "" && redirect != "" {
		next, nextErr := url.Parse(redirect)
		portal, portalErr := url.Parse(h.cfg.LoginConfig.PortalHost)
		ticketID := ""
		if nextErr == nil {
			ticketID = next.Query().Get("ticket_id")
		}
		if nextErr != nil || portalErr != nil || next.Scheme != "https" || next.User != nil ||
			!strings.EqualFold(next.Host, portal.Host) || ticketID == "" ||
			!h.oauth.enableTicketFallbackByTicket(ticketID, secret) {
			http.Error(w, "invalid OAuth continuation", http.StatusBadRequest)
			return
		}
		log.Printf("oauth callback: updated secret for ticket=%s", shortID(ticketID))
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if code == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<h3>登录失败：缺少 code</h3><p>请回到 WebUI 重新发起登录。</p>"))
		return
	}
	candidates := h.oauth.codeSessionCandidates(r.URL.Query().Get("ticket_id"), secret)
	if len(candidates) == 0 {
		// 没有匹配（浏览器在远端时 code 通道不可用），提示用 ticket 通道
		_, _ = w.Write([]byte("<h3>登录已提交，请回到 WebUI 等待结果。</h3>"))
		return
	}
	// portal 的回调可能不带任何配对信息（实测只带 code），因此逐个候选尝试。
	// 授权码与 PKCE verifier 一对一校验：用错只会 STS5.1805，不会消耗授权码。
	var (
		tok     *upstream.TokenResponse
		sess    *oauthSession
		lastErr error
	)
	for _, cand := range candidates {
		t, err := h.cfg.OAuthClient.ExchangeCode(r.Context(), h.cfg.LoginConfig, code, cand.Verifier, cand.Port, cand.DPoPPrivateKey)
		if err == nil {
			tok, sess, lastErr = t, cand, nil
			break
		}
		lastErr = err
	}
	if lastErr != nil {
		// STS5.1805 = 授权码与服务端保存的 PKCE verifier 对不上，最常见的成因是
		// 链接生成后服务重启过（会话只在内存里）或重复使用了旧链接。
		hint := ""
		if strings.Contains(lastErr.Error(), "STS5.1805") || strings.Contains(lastErr.Error(), "invalid authorization code") {
			hint = "<p>授权码已失效：请回到 WebUI 重新点「登录」获取新链接，" +
				"并确保从生成到完成授权期间服务没有重启。</p>"
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<h3>换取凭证失败：" + lastErr.Error() + "</h3>" + hint))
		return
	}
	if err := h.saveLoginResult(tok, sess.Verifier, sess.DPoPPrivateKey); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<h3>保存账号失败：" + err.Error() + "</h3>"))
		return
	}
	h.oauth.complete(sess.ID)
	_, _ = w.Write([]byte("<h3>登录成功，可关闭此页面并回到 WebUI。</h3>"))
}

func tokName(t *upstream.TokenResponse) string {
	if t == nil {
		return ""
	}
	return t.UserName
}

func tokToken(t *upstream.TokenResponse) string {
	if t == nil {
		return ""
	}
	return t.Credentials.SecurityToken
}

// saveLoginResult 落盘 auth 并加入账号池。
func (h *Handler) saveLoginResult(tok *upstream.TokenResponse, codeVerifier string, dpopPrivateJWK ...upstream.DPoPPrivateJWK) error {
	if h.cfg.AuthDir == "" {
		return errors.New("auth_dir not configured")
	}
	cred := tok.Credentials
	var privateJWK map[string]string
	if len(dpopPrivateJWK) > 0 {
		privateJWK = map[string]string(dpopPrivateJWK[0])
	}
	a := auth.New(tok.UserID, tok.UserName, tok.DomainID,
		cred.SecurityToken, cred.AccessKeyID, cred.SecretAccessKey,
		cred.Expiration, tok.RefreshToken, codeVerifier)
	a.SetClientID(h.cfg.LoginConfig.ClientID)
	a.SetDPoPPrivateKey(privateJWK)

	// portal 回调只回传 code，不带身份；user_id 为空会让 auth.FileName() 退化成
	// codearts-unknown.json，账号池按 UserID 建索引就会加载不到（或与既有账号重复）。
	// 因此落盘前用 STS 凭证把身份补全，补全失败也要保证 user_id 有值。
	if a.UserID == "" || a.UserName == "" {
		uid, uname, domain := lookupIdentity(a)
		if uid == "" {
			// 兜底：refresh_token 里的 sub 是唯一且稳定的，避免写出 unknown 文件
			uid = identityFromRefreshToken(tok.RefreshToken)
			log.Printf("webui login: identity lookup failed, fallback user_id=%q", uid)
		}
		if uid != "" {
			a.UserID, a.UserName = uid, firstNonEmpty(uname, a.UserName)
			if domain != "" {
				a.DomainID = domain
			}
		}
	}
	if err := auth.SaveNew(h.cfg.AuthDir, a); err != nil {
		return err
	}
	if p := h.cfg.Pool.AddAccount(a); p != nil && p.AuthSnapshot() != nil {
		// 刚登录的账号先不探测：用户很可能马上就要用，探测占用的上游会话
		// 槽位释放很慢，会把第一个真实请求挤到排队。
		noteTraffic(a.UserID)
		log.Printf("webui login success user_id=%s name=%s", a.UserID, a.UserName)
	} else {
		log.Printf("webui login saved user_id=%s name=%s (pool add failed)", a.UserID, a.UserName)
	}
	return nil
}

// lookupIdentity 用刚拿到的 STS 凭证查身份（先 caller-identity，再 current/user）。
func lookupIdentity(a *auth.Auth) (uid, name, domain string) {
	cred := upstream.SignCredential{
		AccessKeyID:     a.AccessKeyID,
		SecretAccessKey: a.SecretAccessKey,
		SecurityToken:   a.CloudDragonTok,
	}
	if uid, name, domain, err := upstream.CallerIdentity(cred); err == nil && uid != "" {
		return uid, name, domain
	} else if err != nil {
		log.Printf("webui login: caller-identity: %v", err)
	}
	if uid, name, domain, err := upstream.CurrentUser(cred); err == nil && uid != "" {
		return uid, name, domain
	} else if err != nil {
		log.Printf("webui login: current/user: %v", err)
	}
	return "", "", ""
}

// identityFromRefreshToken 从 JWT 形式的 refresh_token 里取 sub 作为兜底 user_id。
func identityFromRefreshToken(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.Sub
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// models
// ---------------------------------------------------------------------------

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表（仅精确模型名）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList 动态获取模型列表并包装成 OpenAI 格式。
func (h *Handler) modelList() []map[string]any {
	entries := modelEntries(h.fetchDynamicModels())
	// 面板设置：是否返回福利模型，以及要隐藏的特定模型。
	returnBenefit, hidden := h.modelListFilters()
	entries = filterModelEntries(entries, returnBenefit, hidden)
	// 过滤掉「已知不可用」的模型：这些条目在客户端点开就是 404 / benefit not
	// found，列出来只会误导。必须作用在最终条目上——静态兜底表也会补位，
	// 只在动态结果上过滤会让被下架的模型又从静态表冒出来。
	// 未知（还没探测出结论）的保留：宁可让用户试，也别把可用模型藏起来。
	if len(entries) > 0 {
		kept := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			id, _ := e["id"].(string)
			if !h.modelKnownUnusable(id) {
				kept = append(kept, e)
			}
		}
		if len(kept) > 0 {
			entries = kept
		}
	}
	// 刚发现到的模型还没有可用性结论：后台补一轮探测，供后续请求过滤。
	// （首次请求返回的是「发现结果」，之后才收敛到「真实可用」。）
	h.probeUnknownAsync()
	return entries
}

// modelListFilters 从面板设置读取 /v1/models 的返回内容控制。
func (h *Handler) modelListFilters() (returnBenefit bool, hidden map[string]struct{}) {
	returnBenefit = true
	hidden = map[string]struct{}{}
	if h.cfg.Settings == nil {
		return
	}
	settings := h.cfg.Settings.Get()
	returnBenefit = settings.ModelsReturnBenefit
	for _, token := range strings.Split(settings.ModelsHidden, ",") {
		token = strings.TrimSpace(token)
		if token != "" {
			hidden[token] = struct{}{}
		}
	}
	return returnBenefit, hidden
}

// filterModelEntries 按面板设置过滤 /v1/models 条目：
//   - returnBenefit=false：去掉带 benefit 标记的限时福利模型
//   - hidden：要隐藏的模型 ID 集合，大小写不敏感匹配
func filterModelEntries(entries []map[string]any, returnBenefit bool, hidden map[string]struct{}) []map[string]any {
	if returnBenefit && len(hidden) == 0 {
		return entries
	}
	effectiveHidden := make(map[string]struct{}, len(hidden)*2)
	for token := range hidden {
		effectiveHidden[token] = struct{}{}
		effectiveHidden[strings.ToLower(token)] = struct{}{}
	}
	hidden = effectiveHidden
	kept := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		id, _ := e["id"].(string)
		if id == "" {
			kept = append(kept, e)
			continue
		}
		if _, ok := hidden[id]; ok {
			continue
		}
		if _, ok := hidden[strings.ToLower(id)]; ok {
			continue
		}
		if !returnBenefit {
			if benefit, _ := e["benefit"].(bool); benefit {
				continue
			}
		}
		kept = append(kept, e)
	}
	return kept
}

// probeUnknownAsync 后台探测「还没有结论」的模型，不阻塞请求。
func (h *Handler) probeUnknownAsync() {
	availability.Lock()
	busy := availability.sweeping
	if !busy {
		availability.sweeping = true
	}
	availability.Unlock()
	if busy {
		return
	}
	go func() {
		defer func() {
			availability.Lock()
			availability.sweeping = false
			availability.Unlock()
		}()
		h.runProbes(h.pendingProbes())
	}()
}

// modelKnownUnusable 报告该模型是否在**所有**健康账号上都被判定为不可用。
// 只要有一个账号没结论或可用，就返回 false（列表保留该模型）。
func (h *Handler) modelKnownUnusable(model string) bool {
	anyKnown, allUnusable := false, true
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		st, _ := modelAvailability(acct.UID, model)
		if st == availUnknown {
			return false // 还没结论，别急着下架
		}
		anyKnown = true
		if st != availUnusable {
			allUnusable = false
		}
	}
	return anyKnown && allUnusable
}

// modelEntries 把账号模型目录 + 静态兜底包装成 OpenAI /v1/models 条目。
//
// /v1/models 统一只返回全小写、去重后的模型 ID；客户端传入大写或混合大小写时，
// 聊天链路会先通过大小写不敏感匹配 / CanonicalModel 自动归一回上游精确 ID。
// 静态表只在动态目录缺失该 ID 时补位，顺序固定（动态已排序，静态按表序）。
func modelEntries(infos []upstream.ModelInfo) []map[string]any {
	out := make([]map[string]any, 0, len(infos)+len(staticModels))
	seen := map[string]bool{}
	add := func(id string, ctx, maxOut int64, benefit bool) {
		lower := strings.ToLower(strings.TrimSpace(id))
		if lower == "" || seen[lower] {
			return
		}
		seen[lower] = true
		if ctx == 0 {
			ctx = 131072 // 兜底
		}
		entry := map[string]any{
			"id":             lower,
			"object":         "model",
			"created":        1753600000,
			"owned_by":       "codearts",
			"context_length": ctx,
		}
		if maxOut > 0 {
			entry["max_output_tokens"] = maxOut
		}
		if benefit {
			entry["benefit"] = true
		}
		out = append(out, entry)
	}
	for _, mi := range infos {
		add(mi.ID, mi.ContextWindow, mi.MaxTokens, mi.Benefit)
	}
	for _, sm := range staticModels {
		add(sm.ID, sm.ContextWindow, 0, sm.Benefit)
	}
	return out
}

// fetchDynamicModels 刷新各账号模型目录并返回合并后的展示列表，缓存 1h。
//
// 目录按账号存放（限时福利按账号授予，账号之间不能互相污染），因此这里逐个健康
// 账号发现并写入其目录；发现失败的账号沿用旧目录或冷启动种子。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	var sets [][]upstream.ModelInfo
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		authz := acct.AuthSnapshot()
		if authz == nil {
			continue
		}
		if !upstream.AccountCatalogStale(acct.UID) {
			if infos, ok := upstream.AccountModels(acct.UID); ok {
				sets = append(sets, infos)
				continue
			}
		}
		infos, err := h.cfg.Upstream.FetchModels(authz) // 成功即写入该账号目录
		if err != nil {
			log.Printf("model discovery failed for account %s: %v", acct.Name, err)
			if h.cfg.Telemetry != nil {
				_ = h.cfg.Telemetry.RecordEvent("model_discovery_failed", "error", acct.UID, "", "model discovery failed", nil)
			}
			continue
		}
		if len(infos) == 0 {
			log.Printf("model discovery returned no models for account %s", acct.Name)
			if h.cfg.Telemetry != nil {
				_ = h.cfg.Telemetry.RecordEvent("model_discovery_empty", "warn", acct.UID, "", "model discovery returned no models", nil)
			}
			continue
		}
		sets = append(sets, infos)
		if h.cfg.Telemetry != nil {
			_ = h.cfg.Telemetry.RecordEvent("model_discovery_succeeded", "info", acct.UID, "", "models discovered", map[string]any{"model_count": len(infos)})
		}
	}
	infos := upstream.MergeModels(sets...)
	if len(infos) == 0 {
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// pickAccount 挑一个「目录里登记了这个模型」的健康账号，没有则退回普通轮转。
//
// 混合账号池里各账号的套餐不同：/v1/models 展示的是并集，若把请求先发给目录里
// 没有该模型的账号，上游会按「未注册模型」报 400，既浪费一次轮转（MaxRotate 只有
// 3 次，池子超过 3 个账号时可能永远轮不到有能力的那台），又会给健康账号记错误、
// 累计到阈值就冷却 10 分钟。所以按能力优先挑号。
func (h *Handler) pickAccount(tried map[string]bool, model string) *pool.Account {
	want := strings.ToLower(upstream.CanonicalModel(model))
	if acct := h.pickByCatalog(tried, want); acct != nil {
		return acct
	}
	// 目录里没人登记这个模型。两种可能：① 谁都没有；② 目录还没发现
	// （客户端只调 /v1/chat/completions、从没调过 /v1/models 就属于这种）。
	// 直接退回轮转的话，混合池里没有该模型的账号会先吃一次 400，白耗 MaxRotate
	// （默认 3 次，账号多于 3 个时可能永远轮不到有能力的那台），还会给健康账号
	// 记错误、到阈值冷却 10 分钟。所以先补齐缺失的目录再挑一次。
	h.discoverMissingCatalogs(tried)
	if acct := h.pickByCatalog(tried, want); acct != nil {
		return acct
	}
	return h.cfg.Pool.PickExcluding(tried)
}

// pickByCatalog 在已发现目录的健康账号里挑第一个能服务该模型的。
func (h *Handler) pickByCatalog(tried map[string]bool, wantLower string) *pool.Account {
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil || tried[acct.Name] || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		if models, known := upstream.AccountModels(acct.UID); known && catalogHas(models, wantLower) {
			if st, _ := modelAvailability(acct.UID, wantLower); st == availUnusable {
				continue // 该账号实测调不通这个模型，别选它
			}
			return acct
		}
	}
	return nil
}

// discoverMissingCatalogs 为「目录未知或已过期」的健康账号补一次模型发现。
//
// 只在聊天挑号失败时调用（不是热路径），且有 CatalogTTL 与失败退避兜底：
// 刚发现过的账号不会被重复打，上游某一路挂了也不会每次请求都空等超时。
func (h *Handler) discoverMissingCatalogs(tried map[string]bool) {
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil || tried[acct.Name] || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		authz := acct.AuthSnapshot()
		if authz == nil {
			continue
		}
		if !upstream.AccountCatalogStale(acct.UID) {
			continue
		}
		if _, err := h.cfg.Upstream.FetchModels(authz); err != nil {
			log.Printf("lazy model discovery failed for account %s: %v", acct.Name, err)
		}
	}
}

// accountCanServe 报告账号目录里是否登记了该模型；账号不存在或目录未知时乐观放行
// （交给上游判定，不凭缺失的目录拒请求）。
func (h *Handler) accountCanServe(acct *pool.Account, model string) bool {
	if acct == nil {
		return true
	}
	models, known := upstream.AccountModels(acct.UID)
	if !known {
		return true
	}
	return catalogHas(models, strings.ToLower(upstream.CanonicalModel(model)))
}

// catalogHas 按小写 ID 在模型目录里查模型。
func catalogHas(models []upstream.ModelInfo, wantLower string) bool {
	for _, mi := range models {
		if strings.ToLower(mi.ID) == wantLower {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	rec := requestRecorderFromContext(r.Context())
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(body) > maxBodyBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8MB limit")
		return
	}
	req, err := parseChatRequest(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	tools, err := nativeTools(req.Tools, req.ToolChoice)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	affinity := r.Header.Get("X-Codearts-Chat-Id")
	if affinity == "" {
		affinity = r.Header.Get("X-Session-Affinity")
	}
	if affinity == "" {
		affinity = r.Header.Get("X-Session-Id")
	}
	req.ConversationID = conversationIDFor(req, affinity)

	toolsOn := toolsActive(req)
	defaultModel, queueRetryDelay, queueMaxAttempts := h.runtimeTuning()
	model := defaultModel
	if req.Model != "" && req.Model != "auto" {
		model = req.Model
	}
	if rec != nil {
		rec.record.Model = model
		rec.record.Stream = req.Stream
		if req.Stream {
			rec.enableStreamOutput()
		}
	}

	// 显式会话 ID 用于账号亲和与响应侧 X-Codearts-Chat-Id；上游不接收 chat_id。
	// V2 不保留语义上下文，所以每轮仍重放完整 OpenAI 历史。
	explicitChat := validChatID(req.ConversationID)
	stickyAcct := ""
	if explicitChat {
		h.convMu.Lock()
		stickyAcct = h.convAcct[req.ConversationID].Account
		h.convMu.Unlock()
	}
	msgs := buildUpstreamMessages(req, toolsOn)
	turnKey := upstreamTurnKey(req)

	// 已经确认「所有健康账号都用不了这个模型」时直接快速失败：不必再打上游、
	// 也不必把账号轮转一遍（轮转只会拿到同样的 not registered / benefit not found）。
	if h.modelKnownUnusable(model) {
		writeOpenAIError(w, http.StatusNotFound, "model_not_available",
			"model "+model+" is not available on any configured account")
		return
	}

	tried := map[string]bool{}
	for _, acct := range h.cfg.Pool.Accounts() {
		if st, reason := modelAvailability(acct.UID, model); st == availUnusable && reason == modelAccessDeniedReason {
			tried[acct.Name] = true
		}
	}
	if tried[stickyAcct] {
		stickyAcct = ""
	}
	var lastErr error
	var modelAccessErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		var acct *pool.Account
		if stickyAcct != "" {
			// 续接会话：锁定原账号（若仍健康）。
			acct = h.cfg.Pool.Get(stickyAcct)
			if acct == nil || !h.cfg.Pool.Healthy(stickyAcct) {
				stickyAcct = ""
				acct = h.pickAccount(tried, model)
			} else {
				tried[acct.Name] = true
			}
		}
		if acct == nil {
			acct = h.pickAccount(tried, model)
		}
		if acct == nil {
			break
		}
		tried[acct.Name] = true

		if rec != nil {
			rec.record.AccountUID = acct.UID
			rec.record.AccountName = acct.UserName
		}

		// 阻塞等待并发槽位（上游单账号并发会话释放慢，串行最稳）。
		// 超时 180s 排队（5 个请求 × ~30s 上限），避免高并发立即失败跳号。
		queueStart := time.Now()
		if !h.cfg.Pool.AcquireLockWait(acct.Name, 180*time.Second) {
			log.Printf("account %s concurrent limit reached after wait, trying next", acct.Name)
			continue
		}
		lockHeld := true
		releaseLock := func() {
			if lockHeld {
				h.cfg.Pool.ReleaseLock(acct.Name)
				lockHeld = false
			}
		}
		if rec != nil {
			rec.record.QueueWaitMs = time.Since(queueStart).Milliseconds()
		}

		ok, verr := h.cfg.Pool.Validate(acct)
		if verr == nil && !ok {
			releaseLock()
			lastErr = errors.New("token invalid")
			continue
		}
		if verr != nil {
			releaseLock()
			lastErr = verr
			continue
		}

		// 检查是否需要保活
		if h.cfg.Pool.NeedKeepalive(acct.Name) {
			h.cfg.Pool.PingKeepalive(acct.Name)
		}

		chatID := req.ConversationID
		// chat_id 仅用于本网关亲和/日志/响应头，统一为 32 位十六进制；无显式 id 时每次新建。
		if !validChatID(chatID) {
			chatID = randHex(32)
		}
		msgID := h.upstreamMsgIDFor(acct.UID, turnKey)

		authz := acct.AuthSnapshot()
		token, accessKeyID, secretAccessKey := authz.Credentials()
		cred := upstream.SignCredential{
			AccessKeyID:     accessKeyID,
			SecretAccessKey: secretAccessKey,
			SecurityToken:   token,
		}
		chatOpts := upstream.ChatOptions{
			ReasoningEffort: req.ReasoningEffort,
			MessageID:       msgID,
			SessionID:       upstreamSessionID(acct.UID, chatID),
			MaxTokens:       req.MaxTokens,
			Temperature:     req.Temperature,
			TopP:            req.TopP,
			Tools:           tools,
			ToolChoice:      nativeToolChoice(req.ToolChoice),
		}
		// 上游并发会话/TPM 排队是瞬时的。对齐 CodeArts Agent IDE
		// 参考实现：默认每 10s 重试同一账号，最多 180 次（30min）。
		// 等待期间释放并发锁，让排队的请求也能尝试（避免死锁式串行等待）。
		var rc io.ReadCloser
		var serr error
		authRetried := false
		// 限时福利路由按「本次实际使用的账号」判定：账号池里套餐可能不同。
		benefit := upstream.IsBenefitModel(acct.UID, model)
		noteTraffic(acct.UID) // 有真实流量：本轮探测整体避让
		for retry := 0; retry < queueMaxAttempts; retry++ {
			if rec != nil {
				rec.record.Attempts++
			}
			rc, serr = acct.Client.ChatStreamWithOptions(r.Context(), msgs, "", cred, acct.UserName, model, chatOpts, benefit)
			if serr == nil && rec != nil {
				rec.record.UpstreamMsgID = msgID
			}
			if serr == nil {
				break
			}
			var ae *upstream.ApiError
			if errors.As(serr, &ae) && (ae.Status == 401 || ae.Code == 401) && !authRetried && authz.Refresh() != "" {
				if rerr := h.cfg.Pool.RefreshToken(acct.Name); rerr == nil {
					authz = acct.AuthSnapshot()
					token, accessKeyID, secretAccessKey = authz.Credentials()
					cred = upstream.SignCredential{
						AccessKeyID:     accessKeyID,
						SecretAccessKey: secretAccessKey,
						SecurityToken:   token,
					}
					authRetried = true
					log.Printf("upstream 401 account=%s: token refreshed, retrying request once", acct.Name)
					continue
				} else {
					log.Printf("upstream 401 account=%s: refresh before retry failed: %v", acct.Name, rerr)
				}
			}
			if errors.As(serr, &ae) && isQueueLimitError(ae) {
				log.Printf("upstream queue limit retry=%d/%d account=%s, waiting %s", retry+1, queueMaxAttempts, acct.Name, queueRetryDelay)
				// 释放锁让其他请求有机会，等待后重新获取
				releaseLock()
				select {
				case <-time.After(queueRetryDelay):
				case <-r.Context().Done():
					writeOpenAIError(w, http.StatusServiceUnavailable, "client_cancelled", "client disconnected")
					return
				}
				if !h.cfg.Pool.AcquireLockWait(acct.Name, h.cfg.QueueReacquireTimeout) {
					lastErr = errors.New("concurrent limit: could not reacquire lock after wait")
					break
				}
				lockHeld = true
				continue
			}
			break // 非并发错误，跳出重试
		}
		var comp *upstream.RawCompletion
		readCompletion := serr == nil && !req.Stream
		if readCompletion {
			comp, serr = upstream.AggregateRaw(rc)
			rc.Close()
		}
		if serr != nil {
			if isClientDisconnect(r, serr) {
				// 客户端已断开：不记账、不轮转。换号续传不可能成功，
				// 且每轮 AcquireLockWait 最长阻塞 180s，纯属空耗。
				releaseLock()
				if rec != nil {
					rec.record.ErrorType = "client_cancelled"
				}
				return
			}
			if rec != nil && rec.record.ErrorType == "" {
				rec.record.ErrorType = "upstream_error"
			}
			modelDenied := upstreamUnavailableReason(serr) == modelAccessDeniedReason
			if modelDenied {
				markUnusable(acct.UID, model, modelAccessDeniedReason)
			}
			releaseLock() // 释放槽位再换号
			if isParamInvalidError(serr) {
				// Client request-param problem: rotating accounts yields the same 400,
				// and cooling accounts takes down the whole pool for minutes.
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
					"upstream rejected request params: "+truncateMsg(serr.Error(), 200))
				return
			}
			if rec != nil {
				rec.record.ErrorType = "model_access"
			}
			if modelDenied {
				modelAccessErr = serr
				stickyAcct = ""
				continue
			}
			lastErr = serr
			if h.handleUpstreamError(r, acct, model, serr) {
				// 模型级错误：换账号也是同样结果，直接给客户端明确答复。
				writeOpenAIError(w, http.StatusNotFound, "model_not_available",
					"model "+model+" is not available: "+truncateMsg(serr.Error(), 200))
				return
			}
			continue
		}
		noteTraffic(acct.UID)

		fetchCreditUsage := func() *upstream.CreditUsage {
			if !acct.Client.CreditUsageEnabled() {
				return nil
			}
			queryCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			usage, err := acct.Client.FetchCreditUsage(queryCtx, msgID, cred)
			if err != nil {
				log.Printf("credit usage query failed account=%s msg=%s: %v", acct.Name, msgID, err)
				return nil
			}
			return usage
		}

		w.Header().Set("X-Codearts-Chat-Id", chatID)
		if rec != nil {
			rec.record.ChatID = chatID
		}

		storeChat := func() {
			// 仅缓存显式会话，便于同 conversation_id 续聊；不把一次性测连写进账号默认会话。
			if !explicitChat {
				return
			}
			h.storeConversation(req.ConversationID, acct.Name)
		}
		if req.Stream {
			var streamedComp *upstream.RawCompletion
			var usage func(*upstream.RawCompletion) map[string]any
			if req.IncludeUsage {
				usage = func(comp *upstream.RawCompletion) map[string]any { return usageEstimate(msgs, comp) }
			}
			werr := upstream.StreamCaptureWithUsage(w, rc, model, func(comp *upstream.RawCompletion) {
				streamedComp = comp
				storeChat()
				h.cfg.Pool.PingKeepalive(acct.Name)
			}, usage)
			rc.Close()
			if werr != nil {
				log.Printf("chat stream account=%s error: %v", acct.Name, werr)
				if isClientDisconnect(r, werr) {
					// 用户点「停止」/断网不是账号故障：不记账、不冷却
					//（否则累计 3 次会把健康账号冷却 10 分钟）。
					if rec != nil {
						rec.record.ErrorType = "client_cancelled"
					}
				} else {
					if rec != nil && rec.record.ErrorType == "" {
						rec.record.ErrorType = "stream_error"
					}
					h.handleUpstreamError(r, acct, model, werr)
				}
			} else {
				markUsable(acct.UID, model)
				h.cfg.Pool.NoteSuccess(acct.Name)
			}
			releaseLock()
			if werr == nil && streamedComp != nil {
				rec.markResponseDone()
				// 对齐官方：整轮结束才查积分（见下方非流式注释）。
				if streamedComp.CreditUsage == nil && streamedComp.Finish != "tool_calls" {
					streamedComp.CreditUsage = fetchCreditUsage()
				}
				applyCompletionMetrics(rec, msgs, streamedComp)
			}
			return
		}

		markUsable(acct.UID, model)
		releaseLock()

		content, finish := comp.Content, comp.Finish
		calls := fromUpstreamToolCalls(comp.ToolCalls)
		// 原生 tool_calls 是 GLM-5.2 的主路径。仅保留文本协议解析作为
		// 旧模型/异常输出的兼容兜底，并同时检查 reasoning_content，
		// 避免模型把工具 JSON 放到思考通道时错误以 stop 结束。
		if toolsOn && len(calls) == 0 {
			if c, cleanContent, found := toolResponse(comp.Content + "\n" + comp.Reasoning); found {
				calls = c
				assignCallIDs(calls)
				content = cleanContent
				finish = "tool_calls"
			}
		}
		if len(calls) > 0 {
			finish = "tool_calls"
		}
		storeChat()
		h.cfg.Pool.NoteSuccess(acct.Name)

		writeJSON(w, http.StatusOK, buildCompletion(model, comp.Reasoning, content, calls, finish, chatID, usageEstimate(msgs, comp)))
		// 积分只进观测记录，不进响应体（usageEstimate 不读 CreditUsage）。
		// 响应写完后再补查，避免阻塞客户端。
		//
		// 查询时机对齐官方桌面客户端：renderer 在 session 进入终态
		// （COMPLETED/CANCELED/FAILED → recordTurnStats）时按 user-msg-id
		// 查一次 /v1/ops/credit/usage?type=msg，而不是每次上游推理都查。
		// 网关侧一轮 = 共享同一 msg_id 的若干请求；finish_reason=tool_calls
		// 说明本轮还有工具续请求，跳过查询，等最终 stop 的那次请求查整轮汇总
		// （上游按 msg_id 聚合计费，request_count 累计）。
		rec.markResponseDone()
		if comp.CreditUsage == nil && finish != "tool_calls" {
			comp.CreditUsage = fetchCreditUsage()
		}
		applyCompletionMetrics(rec, msgs, comp)
		return
	}
	if lastErr == nil && modelAccessErr != nil {
		writeOpenAIError(w, http.StatusNotFound, "model_not_available",
			"model "+model+" is not available: "+truncateMsg(modelAccessErr.Error(), 200))
		return
	}
	msg := "all accounts unavailable (disabled/cooldown)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// buildUpstreamMessages 把 OpenAI 消息原生映射为 CodeArts V2 chat-completions。
// 会话亲和只在网关侧维护；多轮语义由完整 messages 承载。
func buildUpstreamMessages(req *chatRequest, toolsOn bool) []upstream.ChatMessage {
	_ = toolsOn // 保留参数以稳定内部调用面；工具 schema 由 ChatOptions 原生传递。
	keepCalls, keepResults := pairedToolMessages(req.Messages)
	out := make([]upstream.ChatMessage, 0, len(req.Messages))
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "user":
			var content any = m.Text
			if len(m.ContentParts) > 0 {
				// 多模态分片数组原样透传（官方 v2 接受 OpenAI content 形态）。
				content = m.ContentParts
			}
			out = append(out, upstream.ChatMessage{Role: m.Role, Content: content})
		case "assistant":
			reasoning := m.ReasoningContent
			wire := upstream.ChatMessage{Role: "assistant", Content: m.Text, ReasoningContent: &reasoning}
			if keepCalls[i] {
				for _, c := range m.ToolCalls {
					wire.ToolCalls = append(wire.ToolCalls, upstream.ChatToolCall{
						ID: c.ID, Type: "function",
						Function: upstream.ChatToolFunction{Name: c.Name, Arguments: c.Arguments},
					})
				}
				if m.Text == "" {
					wire.Content = nil
				}
			}
			out = append(out, wire)
		case "tool":
			if keepResults[i] {
				content := m.Text
				if content == "" {
					content = "(no output)"
				}
				out = append(out, upstream.ChatMessage{
					Role: "tool", Content: content, ToolCallID: m.ToolCallID, Name: m.Name,
				})
			}
		}
	}
	return out
}

// pairedToolMessages 只保留完整成对的 assistant.tool_calls + tool results。
// CodeArts 会对孤儿或部分成对的工具历史返回 400，并使后续每轮持续失败。
func pairedToolMessages(messages []openAIMessage) (map[int]bool, map[int]bool) {
	resultIndex := make(map[string]int)
	for i, m := range messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			resultIndex[m.ToolCallID] = i
		}
	}
	keepCalls := make(map[int]bool)
	keepResults := make(map[int]bool)
	for i, m := range messages {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		complete := true
		for _, c := range m.ToolCalls {
			if c.ID == "" {
				complete = false
				break
			}
			if _, ok := resultIndex[c.ID]; !ok {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		keepCalls[i] = true
		for _, c := range m.ToolCalls {
			keepResults[resultIndex[c.ID]] = true
		}
	}
	return keepCalls, keepResults
}

// nativeTools preserves a named function choice by exposing only that function
// to the upstream, whose tool_choice accepts strings but not a named object.
func nativeTools(tools []map[string]any, choice toolChoiceOpenAI) ([]map[string]any, error) {
	if choice.Mode != "function" {
		return tools, nil
	}
	var selected map[string]any
	for _, tool := range tools {
		fn, ok := tool["function"].(map[string]any)
		if !ok || fn["name"] != choice.Function {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("tool_choice function %q is defined more than once", choice.Function)
		}
		selected = tool
	}
	if selected == nil {
		return nil, fmt.Errorf("tool_choice function %q is not defined in tools", choice.Function)
	}
	return []map[string]any{selected}, nil
}

func nativeToolChoice(choice toolChoiceOpenAI) any {
	switch choice.Mode {
	case "none", "required":
		return choice.Mode
	case "function":
		// CodeArts InferHub only accepts string-form tool_choice; the object form
		// is rejected with InferHub.001001005.400 param invalid (measured 2026-10-06).
		// nativeTools limits the tools to the named function, so "required"
		// preserves the client's forced-function constraint.
		return "required"
	default:
		return "auto"
	}
}

func fromUpstreamToolCalls(calls []upstream.ChatToolCall) []openAIToolCall {
	out := make([]openAIToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, openAIToolCall{ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	assignCallIDs(out)
	return out
}

func usageEstimate(msgs []upstream.ChatMessage, comp *upstream.RawCompletion) map[string]any {
	if comp.Usage != nil {
		return comp.Usage
	}
	var pt int
	for _, m := range msgs {
		switch content := m.Content.(type) {
		case string:
			pt += len([]rune(content))/4 + 1
		default:
			raw, _ := json.Marshal(content)
			pt += len([]rune(string(raw)))/4 + 1
		}
	}
	ct := (len([]rune(comp.Content))+len([]rune(comp.Reasoning)))/4 + 1
	return map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}
}

// buildCompletion 组装非流式响应。
func buildCompletion(model, reasoning, content string, calls []openAIToolCall, finish, chatID string, usage map[string]any) map[string]any {
	message := map[string]any{"role": "assistant"}
	if len(calls) > 0 {
		message["tool_calls"] = toOpenAIToolCalls(calls)
		if content == "" {
			message["content"] = nil
		} else {
			message["content"] = content
		}
	} else {
		message["content"] = content
	}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"chat_id": chatID,
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp
}

// isClientDisconnect 判断错误是否由客户端断开/取消引起（流式 UI 点「停止」、
// 网络闪断是常态操作）。这类情况不是账号故障：不记账、不冷却、不轮转换号。
func isClientDisconnect(r *http.Request, err error) bool {
	if r != nil && r.Context().Err() != nil {
		return true
	}
	return errors.Is(err, context.Canceled)
}

// handleUpstreamError 记录错误并决定是否处罚账号。
// 返回 true 表示这是「模型对这个账号不可用」的模型级错误：调用方应立即结束
// 轮转并向客户端返回明确错误（换账号也是同样的结果）。
func (h *Handler) handleUpstreamError(r *http.Request, acct *pool.Account, model string, err error) bool {
	if isClientDisconnect(r, err) {
		return false
	}
	if isParamInvalidError(err) {
		return false
	}
	var ae *upstream.ApiError
	if !errors.As(err, &ae) {
		var streamErr *upstream.UpstreamError
		if errors.As(err, &streamErr) {
			ae = streamErr.APIError()
		}
	}
	if ae != nil {
		// 「模型对这个账号不可用」是账号能力问题，不是账号故障，必须在状态码分支
		// **之前**判断：not registered / benefit not found 会被嵌入错误包装成
		// 5xx，落到下面的 `ae.Status >= 500` 就把健康账号冷却 10 分钟——实测
		// 调用一个未注册模型，整个账号被冷却，后续正常请求全部失败。
		if reason := upstreamUnavailableReason(err); reason != "" {
			markUnusable(acct.UID, model, reason)
			log.Printf("model unusable account=%s model=%s: %s", acct.Name, model, reason)
			if h.cfg.Telemetry != nil {
				_ = h.cfg.Telemetry.RecordEvent("model_unusable", "warn", acct.UID, model, reason, nil)
			}
			return true
		}
		switch {
		case ae.Status == 401 || ae.Code == 401:
			h.cfg.Pool.Disable(acct.Name, "401 "+ae.Message)
			if h.cfg.Telemetry != nil {
				_ = h.cfg.Telemetry.RecordEvent("upstream_auth", "error", acct.Name, model, "upstream 401", nil)
			}
		case ae.Status == 429:
			h.cfg.Pool.Cooldown(acct.Name, pool.CoolSoft, h.cfg.SoftCooldown, ae.Error())
			if h.cfg.Telemetry != nil {
				_ = h.cfg.Telemetry.RecordEvent("upstream_rate_limit", "warn", acct.Name, model, ae.Error(), nil)
			}
		case ae.Status == 400 && isConcurrentLimitError(ae.Message):
			// 并发会话上限（TM.00001041）是瞬时错误：上游会话槽位会被其他请求释放，
			// 不冷却账号——池的并发锁已防止过载，冷却反而误伤后续请求。
			log.Printf("upstream concurrent limit (transient) account=%s msg=%s", acct.Name, truncateMsg(ae.Message, 80))
		case ae.Status >= 500:
			h.cfg.Pool.Cooldown(acct.Name, pool.CoolErr, h.cfg.ErrCooldown, ae.Error())
		default:
			h.cfg.Pool.NoteError(acct.Name, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
		}
		return false
	}
	h.cfg.Pool.NoteError(acct.Name, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
	return false
}

// isParamInvalidError 判断是否为上游「请求参数无效」类错误（InferHub.001001005）。
// 这类错误是客户端请求形状问题：换账号结果相同，绝不能冷却账号（曾因此打停整池）。
func isParamInvalidError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *upstream.ApiError
	var streamErr *upstream.UpstreamError
	var message string
	switch {
	case errors.As(err, &apiErr):
		message = apiErr.Message
	case errors.As(err, &streamErr):
		message = streamErr.Code + " " + streamErr.Msg
	default:
		return false
	}
	low := strings.ToLower(message)
	return strings.Contains(low, "inferhub.001001005") || strings.Contains(low, "request param is invalid")
}

// isConcurrentLimitError 判断是否为上游并发会话上限错误（瞬时、可重试）。
func isConcurrentLimitError(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "tm.00001041") || strings.Contains(low, "并发会话")
}

func isQueueLimitError(err *upstream.ApiError) bool {
	if err == nil {
		return false
	}
	if err.Status == http.StatusTooManyRequests {
		return true
	}
	low := strings.ToLower(err.Message)
	return (err.Status == http.StatusBadRequest && isConcurrentLimitError(low)) ||
		strings.Contains(low, "inferhub.modelarts.81111.429") ||
		strings.Contains(low, "tpm limit")
}

func truncateMsg(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error", "code": code},
	})
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := crand.Read(b); err != nil {
		log.Printf("crypto/rand failed; using timestamp-only chat ID: %v", err)
		seed := fmt.Sprintf("%x", time.Now().UnixNano())
		for len(seed) < n {
			seed += seed
		}
		return seed[:n]
	}
	return hex.EncodeToString(b)[:n]
}

func validChatID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func (h *Handler) loadConversations() {
	if h.cfg.ConvStateFile == "" {
		return
	}
	raw, err := os.ReadFile(h.cfg.ConvStateFile)
	if err != nil {
		return
	}
	var state conversationState
	if json.Unmarshal(raw, &state) != nil {
		return
	}
	now := time.Now()
	for id, binding := range state.Conversations {
		if validChatID(id) && binding.Account != "" && !binding.UpdatedAt.IsZero() && now.Sub(binding.UpdatedAt) < conversationBindingTTL {
			h.convAcct[id] = binding
		}
	}
	for key, binding := range state.Turns {
		if binding.ID != "" && !binding.UpdatedAt.IsZero() && now.Sub(binding.UpdatedAt) < conversationBindingTTL {
			h.turnIDs[key] = binding
		}
	}
	h.pruneConversationsLocked(now)
}

func (h *Handler) storeConversation(id, account string) {
	if !validChatID(id) || account == "" {
		return
	}
	h.convMu.Lock()
	now := time.Now()
	h.pruneConversationsLocked(now)
	h.convAcct[id] = conversationBinding{Account: account, UpdatedAt: now}
	h.markConvDirtyLocked()
	h.convMu.Unlock()
}

// upstreamSessionID 派生官方 x-ot-session-id：同一账号+同一会话稳定不变，
// 换账号即换会话 ID（官方会话与账号 1:1，计费/亲和都按签名账号归属）。
func upstreamSessionID(accountUID, chatID string) string {
	sum := sha256.Sum256([]byte("codearts-session:" + accountUID + ":" + chatID))
	return hex.EncodeToString(sum[:16])
}

// upstreamMsgIDFor returns a stable upstream message ID for one account and
// one logical user turn. The same account reuses it across tool continuations
// and retries; a different account gets its own ID because credit usage is
// queried with that account's credentials.
func (h *Handler) upstreamMsgIDFor(accountUID, turnKey string) string {
	if accountUID == "" || turnKey == "" {
		return upstream.NewUpstreamMsgID(time.Now())
	}
	key := accountUID + "\x00" + turnKey
	h.convMu.Lock()
	now := time.Now()
	h.pruneConversationsLocked(now)
	if binding, ok := h.turnIDs[key]; ok && binding.ID != "" {
		id := binding.ID
		h.convMu.Unlock()
		return id
	}
	id := upstream.NewUpstreamMsgID(now)
	h.turnIDs[key] = turnIDBinding{ID: id, UpdatedAt: now}
	h.markConvDirtyLocked()
	h.convMu.Unlock()
	return id
}

func (h *Handler) pruneConversationsLocked(now time.Time) {
	for id, binding := range h.convAcct {
		if binding.UpdatedAt.IsZero() || now.Sub(binding.UpdatedAt) >= conversationBindingTTL {
			delete(h.convAcct, id)
		}
	}
	for len(h.convAcct) >= maxConversationBindings {
		oldestID := ""
		var oldest time.Time
		for id, binding := range h.convAcct {
			if oldestID == "" || binding.UpdatedAt.Before(oldest) {
				oldestID, oldest = id, binding.UpdatedAt
			}
		}
		delete(h.convAcct, oldestID)
	}
	for key, binding := range h.turnIDs {
		if binding.UpdatedAt.IsZero() || now.Sub(binding.UpdatedAt) >= conversationBindingTTL {
			delete(h.turnIDs, key)
		}
	}
	for len(h.turnIDs) >= maxConversationBindings {
		oldestKey := ""
		var oldest time.Time
		for key, binding := range h.turnIDs {
			if oldestKey == "" || binding.UpdatedAt.Before(oldest) {
				oldestKey, oldest = key, binding.UpdatedAt
			}
		}
		delete(h.turnIDs, oldestKey)
	}
}

// markConvDirtyLocked 标记状态待落盘并调度一次合并写。convMu 已持有。
// 请求路径不再同步写盘：一次磁盘故障/慢盘不会拖住聊天请求。
func (h *Handler) markConvDirtyLocked() {
	h.convDirty = true
	if h.cfg.ConvStateFile == "" {
		return
	}
	if !h.convFlushPending {
		h.convFlushPending = true
		time.AfterFunc(200*time.Millisecond, h.flushConversations)
	}
}

// flushConversations 把内存状态快照落盘。convFileMu 保证临时文件单写者；
// 快照在 convMu 内序列化，写盘在锁外，不阻塞请求路径。
func (h *Handler) flushConversations() {
	h.convFileMu.Lock()
	defer h.convFileMu.Unlock()
	h.convMu.Lock()
	h.convFlushPending = false
	if !h.convDirty || h.cfg.ConvStateFile == "" {
		h.convMu.Unlock()
		return
	}
	h.convDirty = false
	raw, err := json.MarshalIndent(conversationState{Conversations: h.convAcct, Turns: h.turnIDs}, "", "  ")
	h.convMu.Unlock()
	if err != nil {
		log.Printf("encode conversation state: %v", err)
		return
	}
	if err := os.WriteFile(h.cfg.ConvStateFile+".tmp", raw, 0o600); err != nil {
		log.Printf("save conversation state: %v", err)
		return
	}
	if err := os.Rename(h.cfg.ConvStateFile+".tmp", h.cfg.ConvStateFile); err != nil {
		log.Printf("replace conversation state: %v", err)
	}
}

// FlushConversationState 立即落盘未保存的会话状态（进程退出前调用）。
func (h *Handler) FlushConversationState() {
	h.flushConversations()
}
