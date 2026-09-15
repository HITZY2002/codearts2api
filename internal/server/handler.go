// Package server 暴露 OpenAI 兼容接口：/v1/chat/completions、/v1/models、/status、WebUI。
package server

import (
	crand "crypto/rand"
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
	"codearts2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool          *pool.Pool
	Upstream      *upstream.Client
	APIKey        string
	MaxRotate     int
	SoftCooldown  time.Duration
	ErrThreshold  int
	ErrCooldown   time.Duration
	DefaultModel  string
	ConvStateFile string
	WatchInfo     map[string]any
	AuthDir       string
	Listen        string
	// OAuthClient 短超时客户端，用于 WebUI 登录换取 token（可注入测试端点）。
	OAuthClient *upstream.Client
	// LoginConfig WebUI 登录配置（client_id 以官方客户端为准）。
	LoginConfig upstream.LoginConfig
	// OAuthCallbackHost 可选：覆盖授权链接回调 host（如 https://oneapi.example.com/codearts）。
	OAuthCallbackHost string
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

var staticModels = []staticModel{
	// 内置（agent-center / /v1/model/builtin 实测，精确大小写）
	{ID: "GLM-5.2", ContextWindow: 202752},
	{ID: "GLM-5.2-ArkTS-SPARK", ContextWindow: 202752},
	{ID: "OpenPangu-2.0-Pro", ContextWindow: 524288},
	{ID: "OpenPangu-2.0-Flash", ContextWindow: 524288},
	{ID: "Qwen3-VL-235B", ContextWindow: 131072},
	// 限时福利（免费套餐，福利网关实测；聊天自动带 maas_type: benefit 头）
	{ID: "deepseek-v4-flash-0731", ContextWindow: 1048576, Benefit: true},
	{ID: "deepseek-v4-pro-0813", ContextWindow: 1048576, Benefit: true},
	{ID: "glm-5.3-flash", ContextWindow: 1048576, Benefit: true},
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

const maxBodyBytes = 8 << 20

// Handler 主路由。
type Handler struct {
	cfg   Config
	mux   *http.ServeMux
	oauth *oauthStore

	convMu sync.Mutex
	chats  map[string]string // account → 最近 chat_id
	// 黏性路由：conversation_id → account_name（多轮续接锁定同一账号，减少上游并发会话占用）。
	convAcct map[string]string

	loginMu sync.Mutex
	logins  map[string]*pendingLogin
}

// pendingLogin WebUI 登录中间状态。
type pendingLogin struct {
	TicketID string
	Secret   string
	Verifier string
	Port     int
	Done     bool
	Err      string
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
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
		chats: map[string]string{}, logins: map[string]*pendingLogin{},
		convAcct: map[string]string{},
	}
	h.loadChats()
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// WorkBuddy 风格控制台（页面不鉴权，API 走 Bearer）
	h.mux.HandleFunc("GET /{$}", h.servePanel)
	h.mux.HandleFunc("GET /admin", h.servePanel)
	h.mux.HandleFunc("GET /panel", h.servePanel)
	h.mux.HandleFunc("GET /panel/", h.servePanel)
	h.mux.HandleFunc("GET /admin/api/overview", h.withAuth(h.adminOverview))
	h.mux.HandleFunc("POST /admin/api/credits", h.withAuth(h.adminCredits))
	h.mux.HandleFunc("POST /admin/api/checkin", h.withAuth(h.adminCheckin))
	h.mux.HandleFunc("POST /admin/api/keepalive", h.withAuth(h.adminKeepalive))
	h.mux.HandleFunc("POST /admin/api/reload", h.withAuth(h.adminReload))
	h.mux.HandleFunc("POST /admin/api/accounts/enable", h.withAuth(h.adminEnable))
	h.mux.HandleFunc("POST /admin/api/accounts/disable", h.withAuth(h.adminDisable))
	h.mux.HandleFunc("POST /admin/api/accounts/clear-cooldown", h.withAuth(h.adminClearCooldown))
	h.mux.HandleFunc("POST /admin/api/oauth/start", h.withAuth(h.adminOAuthStart))
	h.mux.HandleFunc("POST /admin/api/oauth/poll", h.withAuth(h.adminOAuthPoll))
	h.mux.HandleFunc("GET /oauth/callback", h.oauthCallback)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
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
		// 用 redirect 里的 ticket_id 定位 pending login，并把华为云下发的 secret 换进去
		// （ticket 轮询必须用 portal 的 secret，而不是本地生成的）。
		if u, err := url.Parse(redirect); err == nil {
			tid := u.Query().Get("ticket_id")
			if tid != "" {
				h.oauth.enableTicketFallbackByTicket(tid, secret)
				h.loginMu.Lock()
				if p, ok := h.logins[tid]; ok {
					p.Secret = secret
					log.Printf("oauth callback: updated secret for ticket=%s", shortID(tid))
				}
				h.loginMu.Unlock()
			}
		}
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if code == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<h3>登录失败：缺少 code</h3><p>请回到 WebUI 重新发起登录。</p>"))
		return
	}
	sess := h.oauth.codeSession(r.URL.Query().Get("ticket_id"))
	if sess == nil {
		// 没有匹配（浏览器在远端时 code 通道不可用），提示用 ticket 通道
		_, _ = w.Write([]byte("<h3>登录已提交，请回到 WebUI 等待结果。</h3>"))
		return
	}
	tok, err := h.cfg.OAuthClient.ExchangeCode(r.Context(), h.cfg.LoginConfig, code, sess.Verifier, sess.Port, sess.DPoPPrivateKey)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<h3>换取凭证失败：" + err.Error() + "</h3>"))
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
	if p := h.cfg.Pool.AddAccount(a); p != nil && p.Auth != nil {
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
	infos := h.fetchDynamicModels()
	// 过滤掉已知不可用的模型（探测或真实请求学到的结论）：这些条目在客户端
	// 点开就是 404 / benefit not found，列出来只会误导。
	if len(infos) > 0 {
		kept := make([]upstream.ModelInfo, 0, len(infos))
		for _, mi := range infos {
			if h.modelUsableAnywhere(mi.ID) {
				kept = append(kept, mi)
			}
		}
		// 全被过滤时保留原列表，避免上游/探测异常导致列表空掉。
		if len(kept) > 0 {
			infos = kept
		}
	}
	return modelEntries(infos)
}

// modelUsableAnywhere 报告池中是否至少有一个健康账号能用该模型。
func (h *Handler) modelUsableAnywhere(model string) bool {
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		if st, _ := modelAvailability(acct.UID, model); st == availUsable {
			return true
		}
	}
	return false
}

// modelEntries 把账号模型目录 + 静态兜底包装成 OpenAI /v1/models 条目。
//
// 上游模型 ID 区分大小写：既列精确 ID（GLM-5.2），也补一条小写别名（glm-5.2，
// 老客户端习惯用小写），聊天时由 CanonicalModel 归一回精确 ID。
// 静态表只在动态目录缺失该 ID 时补位，顺序固定（动态已排序，静态按表序）。
func modelEntries(infos []upstream.ModelInfo) []map[string]any {
	out := make([]map[string]any, 0, len(infos)+len(staticModels))
	seen := map[string]bool{}
	add := func(id string, ctx, maxOut int64, benefit bool) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		if ctx == 0 {
			ctx = 131072 // 兜底
		}
		entry := map[string]any{
			"id":             id,
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
	// addAlias 精确 ID + 就近补一条小写别名（已是小写则跳过）。
	addAlias := func(id string, ctx, maxOut int64, benefit bool) {
		add(id, ctx, maxOut, benefit)
		if lower := strings.ToLower(id); lower != id {
			add(lower, ctx, maxOut, benefit)
		}
	}
	for _, mi := range infos {
		addAlias(mi.ID, mi.ContextWindow, mi.MaxTokens, mi.Benefit)
	}
	for _, sm := range staticModels {
		addAlias(sm.ID, sm.ContextWindow, 0, sm.Benefit)
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
		if acct.Auth == nil || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		if !upstream.AccountCatalogStale(acct.UID) {
			if infos, ok := upstream.AccountModels(acct.UID); ok {
				sets = append(sets, infos)
				continue
			}
		}
		infos, err := h.cfg.Upstream.FetchModels(acct.Auth) // 成功即写入该账号目录
		if err != nil {
			log.Printf("model discovery failed for account %s: %v", acct.Name, err)
			continue
		}
		if len(infos) == 0 {
			log.Printf("model discovery returned no models for account %s", acct.Name)
			continue
		}
		sets = append(sets, infos)
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
		if acct == nil || acct.Auth == nil || tried[acct.Name] || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		if !upstream.AccountCatalogStale(acct.UID) {
			continue
		}
		if _, err := h.cfg.Upstream.FetchModels(acct.Auth); err != nil {
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
	if req.ConversationID == "" {
		req.ConversationID = r.Header.Get("X-Codearts-Chat-Id")
	}

	toolsOn := toolsActive(req)
	model := h.cfg.DefaultModel
	if req.Model != "" && req.Model != "auto" {
		model = req.Model
	}

	// 仅当客户端显式传入 conversation_id / X-Codearts-Chat-Id 时续会话。
	// 自动复用账号级 chat_id 会把无关历史拼进下一次独立请求（如测连 "hi"），
	// 上游可能回 related_question_answer 或残留上下文，表现为「不相干 JSON」。
	explicitChat := req.ConversationID != "" && validChatID(req.ConversationID)
	msgs := buildUpstreamMessages(req, toolsOn, explicitChat)

	// 黏性路由：客户端带 conversation_id 续接时锁定原账号，减少上游并发会话占用。
	stickyAcct := ""
	if explicitChat {
		h.convMu.Lock()
		stickyAcct = h.convAcct[req.ConversationID]
		h.convMu.Unlock()
	}

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		var acct *pool.Account
		if stickyAcct != "" {
			// 续接会话：锁定原账号（仍健康且目录里有这个模型）。
			acct = h.cfg.Pool.Get(stickyAcct)
			if acct == nil || !h.cfg.Pool.Healthy(stickyAcct) || !h.accountCanServe(acct, model) {
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

		// 阻塞等待并发槽位（上游单账号并发会话释放慢，串行最稳）。
		// 超时 180s 排队（5 个请求 × ~30s 上限），避免高并发立即失败跳号。
		if !h.cfg.Pool.AcquireLockWait(acct.Name, 180*time.Second) {
			log.Printf("account %s concurrent limit reached after wait, trying next", acct.Name)
			continue
		}

		ok, verr := h.cfg.Pool.Validate(acct)
		if verr == nil && !ok {
			h.cfg.Pool.ReleaseLock(acct.Name)
			lastErr = errors.New("token invalid")
			continue
		}
		if verr != nil {
			h.cfg.Pool.ReleaseLock(acct.Name)
			lastErr = verr
			continue
		}

		// 检查是否需要保活
		if h.cfg.Pool.NeedKeepalive(acct.Name) {
			h.cfg.Pool.PingKeepalive(acct.Name)
		}

		chatID := req.ConversationID
		// 上游要求 chat_id 为 32 位十六进制（UUID 去连字符）；无显式 id 时每次新建。
		if !validChatID(chatID) {
			chatID = randHex(32)
		}

		token, accessKeyID, secretAccessKey := acct.Auth.Credentials()
		cred := upstream.SignCredential{
			AccessKeyID:     accessKeyID,
			SecretAccessKey: secretAccessKey,
			SecurityToken:   token,
		}
		chatOpts := upstream.ChatOptions{
			ReasoningEffort: req.ReasoningEffort,
			MaxTokens:       req.MaxTokens,
			Temperature:     req.Temperature,
			TopP:            req.TopP,
		}
		// 上游并发会话上限（TM.00001041）是瞬时的：已完成的会话槽位释放较慢
		//（实测 >15s）。遇到时等待后重试同一账号，最多 10 次（每次 5s，共 50s）。
		// 等待期间释放并发锁，让排队的请求也能尝试（避免死锁式串行等待）。
		// 限时福利路由按「本次实际使用的账号」判定：账号池里套餐可能不同。
		// 限时福利路由按「本次实际使用的账号」判定：账号池里套餐可能不同。
		benefit := upstream.IsBenefitModel(acct.UID, model)
		var rc io.ReadCloser
		var serr error
		authRetried := false
		for retry := 0; retry < 10; retry++ {
			rc, serr = acct.Client.ChatStreamWithOptions(r.Context(), chatID, msgs, "", cred, acct.UserName, model, chatOpts, benefit)
			if serr == nil {
				break
			}
			var ae *upstream.ApiError
			if errors.As(serr, &ae) && (ae.Status == 401 || ae.Code == 401) && !authRetried && acct.Auth.Refresh() != "" {
				if rerr := h.cfg.Pool.RefreshToken(acct.Name); rerr == nil {
					token, accessKeyID, secretAccessKey = acct.Auth.Credentials()
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
			if errors.As(serr, &ae) && ae.Status == 400 && isConcurrentLimitError(ae.Message) {
				log.Printf("upstream concurrent limit retry=%d account=%s, waiting 5s", retry+1, acct.Name)
				// 释放锁让其他请求有机会，等待后重新获取
				h.cfg.Pool.ReleaseLock(acct.Name)
				select {
				case <-time.After(5 * time.Second):
				case <-r.Context().Done():
					writeOpenAIError(w, http.StatusServiceUnavailable, "client_cancelled", "client disconnected")
					return
				}
				if !h.cfg.Pool.AcquireLockWait(acct.Name, 30*time.Second) {
					lastErr = errors.New("concurrent limit: could not reacquire lock after wait")
					break
				}
				continue
			}
			break // 非并发错误，跳出重试
		}
		if serr != nil {
			h.cfg.Pool.ReleaseLock(acct.Name) // 释放槽位再换号
			lastErr = serr
			h.handleUpstreamError(acct, model, serr)
			continue
		}
		// 真实调用成功：记下「这个账号能用这个模型」，供 /v1/models 过滤参考。
		markUsable(acct.UID, model)

		w.Header().Set("X-Codearts-Chat-Id", chatID)

		storeChat := func() {
			// 仅缓存显式会话，便于同 conversation_id 续聊；不把一次性测连写进账号默认会话。
			if !explicitChat {
				return
			}
			h.convMu.Lock()
			h.chats[acct.Name] = chatID
			h.convAcct[req.ConversationID] = acct.Name // 黏性路由：续接锁定同账号
			h.convMu.Unlock()
			h.saveChats()
		}
		if req.Stream && !toolsOn {
			werr := upstream.StreamCapture(w, rc, model, func(comp *upstream.RawCompletion) {
				storeChat()
				// 流式传输中定期保活
				h.cfg.Pool.PingKeepalive(acct.Name)
			})
			rc.Close()
			h.cfg.Pool.ReleaseLock(acct.Name) // 释放锁
			if werr != nil {
				log.Printf("chat stream account=%s error: %v", acct.Name, werr)
				h.cfg.Pool.NoteError(acct.Name, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			} else {
				h.cfg.Pool.NoteSuccess(acct.Name)
			}
			return
		}

		comp, aerr := upstream.AggregateRaw(rc)
		rc.Close()
		h.cfg.Pool.ReleaseLock(acct.Name) // 释放锁
		if aerr != nil {
			lastErr = aerr
			h.cfg.Pool.Cooldown(acct.Name, pool.CoolErr, h.cfg.ErrCooldown, aerr.Error())
			continue
		}

		content, finish := comp.Content, comp.Finish
		var calls []openAIToolCall
		if toolsOn {
			if c, rest, found := extractToolCalls(comp.Content); found {
				calls = c
				assignCallIDs(calls)
				content = rest
				finish = "tool_calls"
			}
		}
		storeChat()
		h.cfg.Pool.NoteSuccess(acct.Name)

		if req.Stream {
			h.emitSyntheticStream(w, model, comp.Reasoning, content, calls, finish)
		} else {
			writeJSON(w, http.StatusOK, buildCompletion(model, comp.Reasoning, content, calls, finish, chatID, usageEstimate(msgs, comp)))
		}
		return
	}
	msg := "all accounts unavailable (disabled/cooldown)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// buildUpstreamMessages 把 OpenAI 消息转成 CodeArts raw payload（content block 数组）。
//
// 核心修复：不再只发单条消息（prompt_tokens:0、上下文污染），
// 而是发送完整的 raw JSON payload，让 upstream 正确接收消息和 system。
// 这解决了 "底层请求就有问题" 的根本原因。
// 修改版：使用更通用的客户端标识以避免被识别为测试请求
func buildUpstreamMessages(req *chatRequest, toolsOn bool, continueChat bool) []upstream.ChatMessage {
	var toolsBlock string
	if toolsOn {
		toolsBlock = buildToolsPrompt(normalizeTools(req.Tools), req.ToolChoice)
	}

	var prompt string
	if continueChat {
		// 续接会话只发尾部增量（通常是 tool 结果 + 最新 user）
		tail := req.Messages
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				tail = req.Messages[i:]
				break
			}
		}
		prompt = renderTailPrompt(tail, toolsBlock)
	} else {
		prompt = renderFullPrompt(req.Messages, toolsBlock)
	}
	return []upstream.ChatMessage{{Type: "text", Text: prompt}}
}

func usageEstimate(msgs []upstream.ChatMessage, comp *upstream.RawCompletion) map[string]int {
	var pt int
	for _, m := range msgs {
		pt += len([]rune(m.Text))/4 + 1
	}
	ct := (len([]rune(comp.Content))+len([]rune(comp.Reasoning)))/4 + 1
	return map[string]int{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}
}

// emitSyntheticStream 工具场景：聚合后合成 OpenAI SSE。
func (h *Handler) emitSyntheticStream(w http.ResponseWriter, model, reasoning, content string, calls []openAIToolCall, finish string) {
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

	writeChunk := func(delta map[string]any, fin string) {
		choice := map[string]any{"index": 0, "delta": delta}
		if fin != "" {
			choice["finish_reason"] = fin
		}
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{choice},
		}
		raw, _ := json.Marshal(chunk)
		_, _ = io.WriteString(w, "data: "+string(raw)+"\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
	if reasoning != "" {
		writeChunk(map[string]any{"reasoning_content": reasoning}, "")
	}
	if content != "" {
		writeChunk(map[string]any{"content": content}, "")
	}
	for i, c := range calls {
		writeChunk(map[string]any{"tool_calls": []any{
			map[string]any{"index": i, "id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": ""}},
		}}, "")
		writeChunk(map[string]any{"tool_calls": []any{
			map[string]any{"index": i, "function": map[string]any{"arguments": c.Arguments}},
		}}, "")
	}
	writeChunk(map[string]any{}, finish)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// buildCompletion 组装非流式响应。
func buildCompletion(model, reasoning, content string, calls []openAIToolCall, finish, chatID string, usage map[string]int) map[string]any {
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

func (h *Handler) handleUpstreamError(acct *pool.Account, model string, err error) {
	var ae *upstream.ApiError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == 401 || ae.Code == 401:
			h.cfg.Pool.Disable(acct.Name, "401 "+ae.Message)
		case ae.Status == 429:
			h.cfg.Pool.Cooldown(acct.Name, pool.CoolSoft, h.cfg.SoftCooldown, ae.Error())
		case ae.Status == 400 && isConcurrentLimitError(ae.Message):
			// 并发会话上限（TM.00001041）是瞬时错误：上游会话槽位会被其他请求释放，
			// 不冷却账号——池的并发锁已防止过载，冷却反而误伤后续请求。
			log.Printf("upstream concurrent limit (transient) account=%s msg=%s", acct.Name, truncateMsg(ae.Message, 80))
		case ae.Status >= 500:
			h.cfg.Pool.Cooldown(acct.Name, pool.CoolErr, h.cfg.ErrCooldown, ae.Error())
		default:
			// 「模型对这个账号不可用」是账号能力问题，不是账号故障：记下来用于
			// /v1/models 过滤与路由避开，不能给它记错误冷却（否则健康账号被误伤）。
			if reason := upstreamUnavailableReason(err); reason != "" {
				markUnusable(acct.UID, model, reason)
				log.Printf("model unusable account=%s model=%s: %s", acct.Name, model, reason)
				return
			}
			h.cfg.Pool.NoteError(acct.Name, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
		}
		return
	}
	h.cfg.Pool.NoteError(acct.Name, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
}

// isConcurrentLimitError 判断是否为上游并发会话上限错误（瞬时、可重试）。
func isConcurrentLimitError(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "tm.00001041") || strings.Contains(low, "并发会话")
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

func (h *Handler) loadChats() {
	if h.cfg.ConvStateFile == "" {
		return
	}
	raw, err := os.ReadFile(h.cfg.ConvStateFile)
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, &h.chats)
}

func (h *Handler) saveChats() {
	if h.cfg.ConvStateFile == "" {
		return
	}
	raw, _ := json.MarshalIndent(h.chats, "", "  ")
	if err := os.WriteFile(h.cfg.ConvStateFile+".tmp", raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(h.cfg.ConvStateFile+".tmp", h.cfg.ConvStateFile)
}
