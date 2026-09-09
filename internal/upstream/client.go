// CodeArts Agent 云端客户端。
//
// 纯 HTTP：
//   - 登录：华为云 CodeArts OAuth2（PKCE + 本地回调）→ snap-manager /v1/oauth2/tokens
//     换 STS 临时 AK/SK + security_token；ticket 轮询为兜底通道
//   - 聊天：POST snap-access/v1/chat/chat，header x-auth-token=security_token，SSE 流式
//   - 刷新：POST /v1/oauth2/tokens grant_type=refresh_token
package upstream

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"codearts2api/internal/auth"
)

// ApiError 带业务 code 的上游错误。
type ApiError struct {
	Code    int
	Status  int
	Message string
	Path    string
}

func (e *ApiError) Error() string {
	return fmt.Sprintf("codearts api code=%d http=%d path=%s msg=%s", e.Code, e.Status, e.Path, e.Message)
}

// Credentials 登录返回的 STS 临时凭证。
type Credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token"`
	Expiration      string `json:"expiration"`
}

// LegacyCredential ticket 通道（旧登录）返回的凭证字段。
type LegacyCredential struct {
	Access        string `json:"access"`
	Secret        string `json:"secret"`
	SecurityToken string `json:"securitytoken"`
	ExpiresAt     string `json:"expires_at"`
}

// TokenResponse oauth2/tokens 响应（含用户信息与凭证）。
type TokenResponse struct {
	UserID       string           `json:"user_id"`
	UserName     string           `json:"user_name"`
	DomainID     string           `json:"domain_id"`
	RefreshToken string           `json:"refresh_token"`
	Credentials  Credentials      `json:"credentials"`
	Credential   LegacyCredential `json:"credential"`
}

// LoginConfig 登录相关配置。
type LoginConfig struct {
	ClientID      string
	PortalHost    string
	SnapManager   string
	STSHost       string
	RedirectPath  string // 本地回调路径
	PluginName    string
	PluginVersion string
}

// DefaultLoginConfig 生产默认值（逆向自 huaweicloud.authentication 扩展）。
func DefaultLoginConfig() LoginConfig {
	return LoginConfig{
		ClientID:      CLIENT_ID,
		PortalHost:    PortalHost,
		SnapManager:   SnapManagerHost,
		STSHost:       STSHost,
		RedirectPath:  "/oauth/callback",
		PluginName:    "snap_AIIDE",
		PluginVersion: "5.1.0",
	}
}

// Client CodeArts 云 API 客户端。
type Client struct {
	http       *http.Client // 短请求，带总超时
	streamHTTP *http.Client // SSE 长流，无总超时
}

// New 构造客户端。
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	tr := &http.Transport{
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		http:       &http.Client{Timeout: timeout, Transport: tr},
		streamHTTP: &http.Client{Transport: tr},
	}
}

// ---------------------------------------------------------------------------
// 认证
// ---------------------------------------------------------------------------

// BuildAuthorizeURL 构造 portal 登录链接（PKCE）。
// ticketID 客户端生成的随机 hex；port 为本地回调端口。
func (c *Client) BuildAuthorizeURL(cfg LoginConfig, ticketID, codeChallenge, codeChallengeMethod string, port int) string {
	q := url.Values{}
	q.Set("theme", "dark")
	q.Set("locale", "zh-cn")
	q.Set("uri_scheme", cfg.ClientID)
	q.Set("client_id", cfg.ClientID)
	q.Set("port", fmt.Sprint(port))
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", codeChallengeMethod)
	q.Set("ticket_id", ticketID)
	q.Set("plugin-name", cfg.PluginName)
	q.Set("plugin-version", cfg.PluginVersion)
	return cfg.PortalHost + "/authorize?" + q.Encode()
}

// ExchangeCode 用授权码换 token（OAuth2 authorization_code）。
func (c *Client) ExchangeCode(ctx context.Context, cfg LoginConfig, code, codeVerifier string, port int) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("code", code)
	form.Set("code_verifier", codeVerifier)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", fmt.Sprintf("http://127.0.0.1:%d%s", port, cfg.RedirectPath))
	return c.requestToken(ctx, cfg, form)
}

// RefreshToken 用 refresh_token 换新凭证。
func (c *Client) RefreshToken(ctx context.Context, cfg LoginConfig, refreshToken, codeVerifier string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("code_verifier", codeVerifier)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	return c.requestToken(ctx, cfg, form)
}

// requestToken 向 STS 令牌端点发 form + DPoP 请求。
func (c *Client) requestToken(ctx context.Context, cfg LoginConfig, form url.Values) (*TokenResponse, error) {
	url := cfg.STSHost + EpOAuthTokens
	kp, err := newDpopKeyPair()
	if err != nil {
		return nil, fmt.Errorf("dpop keypair: %w", err)
	}
	proof, err := signDpopProof(kp, url)
	if err != nil {
		return nil, fmt.Errorf("dpop proof: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", proof)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: EpOAuthTokens}
	}
	var out TokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse token response: %w body=%s", err, truncateStr(string(raw), 200))
	}
	if out.Credentials.SecurityToken == "" {
		// 兼容 ticket/旧通道字段（legacy）
		if out.Credential.SecurityToken != "" {
			out.Credentials = Credentials{
				AccessKeyID:     out.Credential.Access,
				SecretAccessKey: out.Credential.Secret,
				SecurityToken:   out.Credential.SecurityToken,
				Expiration:      out.Credential.ExpiresAt,
			}
			return &out, nil
		}
		return nil, fmt.Errorf("token response missing credentials: %s", truncateStr(string(raw), 200))
	}
	return &out, nil
}

// PollTicket 轮询登录结果（兜底通道）。
func (c *Client) PollTicket(ctx context.Context, cfg LoginConfig, ticketID, secret string) (*TokenResponse, error) {
	path := EpLoginTicket + "?ticket_id=" + url.QueryEscape(ticketID) + "&secret=" + url.QueryEscape(secret)
	headers := map[string]string{
		"plugin-name":    cfg.PluginName,
		"plugin-version": cfg.PluginVersion,
	}
	var out TokenResponse
	if err := c.doJSON(ctx, http.MethodGet, cfg.SnapManager, path, headers, nil, &out); err != nil {
		return nil, err
	}
	// 旧通道凭证归一化
	if out.Credentials.SecurityToken == "" && out.Credential.SecurityToken != "" {
		out.Credentials = Credentials{
			AccessKeyID:     out.Credential.Access,
			SecretAccessKey: out.Credential.Secret,
			SecurityToken:   out.Credential.SecurityToken,
			Expiration:      out.Credential.ExpiresAt,
		}
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// 聊天
// ---------------------------------------------------------------------------

// ChatMessage 单条消息（实测：Anthropic 风格内容块，无 role）。
type ChatMessage struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// CanonicalModel 把用户友好模型 ID 映射为上游精确模型 ID。
//
// 免费套餐（福利网关）模型名经常变化（如 deepseek-v4-pro-0813），
// 这里优先按动态注册表做大小写不敏感匹配；旧版小写别名保留做兼容。
func CanonicalModel(id string) string {
	if exact, ok := resolveBenefitModel(id); ok {
		return exact
	}
	if exact, ok := resolveKnownModel(id); ok {
		return exact
	}
	switch id {
	case "snap-chat", "glm-5.2":
		return "GLM-5.2"
	case "glm-5.1":
		return "GLM-5.1"
	case "glm-4.7":
		return "GLM-4.7"
	case "qwen3-vl-235b":
		return "Qwen3-VL-235B"
	case "qwen3.5-397b-a17b-vl":
		return "Qwen3.5-397B-A17B-VL"
	case "qwen3.6-27b-vl":
		return "Qwen3.6-27B-VL"
	default:
		return id
	}
}

// 福利模型路由头（官方客户端行为：renderer 检测到 isFreeBenefit 即加
// "maas_type": "benefit"，经 kernel 转发到上游聊天请求做网关路由）。
const (
	HeaderMaasType = "maas_type"
	MaasBenefit    = "benefit"
)

// 冷启动种子：动态发现（FetchModels，每小时刷新）会覆盖/增补。
// 免费套餐轮换时即使首次 fetch 失败，已知福利模型仍能正确路由。
func init() {
	NoteKnownModels([]string{
		"GLM-5.2", "GLM-5.2-ArkTS-SPARK", "GLM-5.1", "GLM-4.7",
		"OpenPangu-2.0-Pro", "OpenPangu-2.0-Flash",
		"Qwen3-VL-235B", "Qwen3.5-397B-A17B-VL", "Qwen3.6-27B-VL",
	})
	NoteBenefitModels([]string{
		"deepseek-v4-flash-0731", "deepseek-v4-pro-0813", "glm-5.3-flash",
	})
}

// ChatHeadersV2 组装 /api/v2/chat/completions 请求头。
// 与官方 AgentKernel 一致：x-auth-token 鉴权 + AK/SK 签名，无 Agent-Type。
func ChatHeadersV2(token, traceID, language string) map[string]string {
	if traceID == "" {
		traceID = fmt.Sprintf("%x", time.Now().UnixNano())
	}
	if language == "" {
		language = "zh-cn"
	}
	return map[string]string{
		"Content-Type":    "application/json",
		"Accept":          "text/event-stream",
		"x-auth-token":    token,
		"x-snap-traceid":  traceID,
		"X-Language":      language,
		"app-id":          "CodeAgent3.0",
		"is_confidential": "false",
	}
}

// ChatStream 发送 /api/v2/chat/completions（OpenAI 兼容，AK/SK 签名 + x-auth-token）
// 并返回 SSE 流（调用方负责 Close）。
func (c *Client) ChatStream(ctx context.Context, chatID string, messages []ChatMessage, traceID string, cred SignCredential, userName string, model string) (io.ReadCloser, error) {
	body := map[string]any{
		"model":    CanonicalModel(model),
		"stream":   true,
		"messages": chatMessagesToOpenAI(messages),
	}
	return c.SendChatV2(ctx, body, traceID, cred, cred.SecurityToken)
}

// chatMessagesToOpenAI 把上游消息折叠为 OpenAI user 消息。
func chatMessagesToOpenAI(msgs []ChatMessage) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{"role": "user", "content": m.Text})
	}
	return out
}

// SendChatV2 发送自定义 OpenAI 兼容 body 到 /api/v2/chat/completions。
// 鉴权：x-auth-token（STS security token）+ 华为云 SDK-HMAC-SHA256 AK/SK 签名。
// 福利模型（免费套餐）按官方行为追加 maas_type: benefit 请求头做网关路由。
func (c *Client) SendChatV2(ctx context.Context, body map[string]any, traceID string, cred SignCredential, userToken string) (io.ReadCloser, error) {
	raw, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, SnapEngineApiHost+EpChatV2, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range ChatHeadersV2(userToken, traceID, "zh-cn") {
		httpReq.Header.Set(k, v)
	}
	if model, _ := body["model"].(string); IsBenefitModel(CanonicalModel(model)) {
		httpReq.Header.Set(HeaderMaasType, MaasBenefit)
	}
	signRequest(httpReq, raw, cred)
	resp, err := c.streamHTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(rawBody), 200), Path: EpChatV2}
	}
	return resp.Body, nil
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

func (c *Client) doJSON(ctx context.Context, method, baseURL, path string, headers map[string]string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: path}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("parse %s: %w body=%s", path, err, truncateStr(string(raw), 200))
		}
	}
	return nil
}

// PKCE 生成 code_verifier / code_challenge（S256）。
func PKCE() (verifier, challenge string, err error) {
	b := make([]byte, 64)
	if _, err := crand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// RandomHex 生成 n 字节的 hex 随机串。
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Log 上游日志。
func Log(format string, args ...any) {
	log.Printf("upstream: "+format, args...)
}

func truncateStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ModelInfo 模型信息（与 workbuddy/trae 一致）。
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int64  `json:"contextWindow,omitempty"`
	MaxTokens     int64  `json:"maxTokens,omitempty"`
	// Benefit 限时福利（免费套餐）模型：聊天时需带 maas_type: benefit 头。
	Benefit bool   `json:"benefit,omitempty"`
	Desc    string `json:"desc,omitempty"`
}

// 福利/已知模型注册表：动态发现的结果，用于大小写兼容与福利路由判断。
var modelRegistry struct {
	sync.RWMutex
	benefit map[string]string // lower(id) -> exact id
	known   map[string]string // lower(id) -> exact id
}

// NoteBenefitModels 登记福利模型精确 ID（同时登记已知表做大小写兼容）。
func NoteBenefitModels(ids []string) {
	modelRegistry.Lock()
	defer modelRegistry.Unlock()
	if modelRegistry.benefit == nil {
		modelRegistry.benefit = map[string]string{}
	}
	if modelRegistry.known == nil {
		modelRegistry.known = map[string]string{}
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		modelRegistry.benefit[strings.ToLower(id)] = id
		modelRegistry.known[strings.ToLower(id)] = id
	}
}

// NoteKnownModels 登记内置模型精确 ID（大小写兼容用，不标记福利）。
func NoteKnownModels(ids []string) {
	modelRegistry.Lock()
	defer modelRegistry.Unlock()
	if modelRegistry.known == nil {
		modelRegistry.known = map[string]string{}
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := modelRegistry.known[strings.ToLower(id)]; !ok {
			modelRegistry.known[strings.ToLower(id)] = id
		}
	}
}

// IsBenefitModel 报告精确模型 ID 是否为福利模型（大小写不敏感）。
func IsBenefitModel(id string) bool {
	modelRegistry.RLock()
	defer modelRegistry.RUnlock()
	_, ok := modelRegistry.benefit[strings.ToLower(id)]
	return ok
}

func resolveBenefitModel(id string) (string, bool) {
	modelRegistry.RLock()
	defer modelRegistry.RUnlock()
	exact, ok := modelRegistry.benefit[strings.ToLower(id)]
	return exact, ok
}

func resolveKnownModel(id string) (string, bool) {
	modelRegistry.RLock()
	defer modelRegistry.RUnlock()
	exact, ok := modelRegistry.known[strings.ToLower(id)]
	return exact, ok
}

// FetchModels 拉取当前账号可用模型（动态缓存由调用方负责）。
//
// 与官方 ModelService 对齐的三路合并：
//  1. agent-center：useragents 找 CodeAgent（alias 智能体 + show_in_ide）→ detail 的 gpts.models
//  2. 内置接口：GET /v1/model/builtin（Agent-Type: PromptCenter）的 builtinModels
//  3. 限时福利：opengw 网关 /api/v1/gateway/config 的 models（聊天需 maas_type: benefit 头）
//
// 福利模型同时写入注册表（大小写兼容 + 路由判断）。
func (c *Client) FetchModels(acct *auth.Auth) ([]ModelInfo, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for model fetch")
	}
	cred := SignCredential{
		AccessKeyID:     acct.AccessKeyID,
		SecretAccessKey: acct.SecretAccessKey,
		SecurityToken:   acct.CloudDragonTok,
	}
	merged := map[string]ModelInfo{}
	put := func(mi ModelInfo) {
		k := strings.ToLower(mi.ID)
		cur, ok := merged[k]
		if !ok {
			merged[k] = mi
			return
		}
		// 去重：优先保留精确大小写 / 参数更全的一条。
		curScore := boolToInt(cur.ContextWindow > 0) + boolToInt(cur.ID != strings.ToLower(cur.ID))
		newScore := boolToInt(mi.ContextWindow > 0) + boolToInt(mi.ID != strings.ToLower(mi.ID))
		if newScore > curScore {
			merged[k] = mi
		}
	}
	// 1. agent-center detail
	if infos, err := c.fetchAgentModels(cred); err != nil {
		Log("agent models: %v", err)
	} else {
		for _, mi := range infos {
			put(mi)
		}
	}
	// 2. /v1/model/builtin
	if infos, err := c.fetchBuiltinModels(cred); err != nil {
		Log("builtin models: %v", err)
	} else {
		for _, mi := range infos {
			put(mi)
		}
	}
	// 3. 限时福利（先 claim，幂等；失败不阻断内置模型）
	if infos, err := c.fetchBenefitModels(cred); err != nil {
		Log("benefit models: %v", err)
	} else {
		for _, mi := range infos {
			put(mi)
		}
	}
	if len(merged) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	known := make([]string, 0, len(merged))
	benefit := make([]string, 0)
	out := make([]ModelInfo, 0, len(merged))
	for _, mi := range merged {
		out = append(out, mi)
		known = append(known, mi.ID)
		if mi.Benefit {
			benefit = append(benefit, mi.ID)
		}
	}
	NoteKnownModels(known)
	NoteBenefitModels(benefit)
	return out, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fetchAgentModels 官方 fetchAgentModels 等价实现。
func (c *Client) fetchAgentModels(cred SignCredential) ([]ModelInfo, error) {
	agentID, err := c.defaultAgentID(cred)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return nil, fmt.Errorf("no default agent found")
	}
	raw, err := c.getSigned(context.Background(), SnapEngineApiHost+EpAgentDetail+"?agent_id="+url.QueryEscape(agentID), cred, true)
	if err != nil {
		return nil, err
	}
	var detail struct {
		Gpts struct {
			Models []struct {
				ModelAlias string `json:"model_alias"`
				ModelName  string `json:"model_name"`
				ModelID    string `json:"model_id"`
				Params     struct {
					ContextWindow int64  `json:"context_window"`
					MaxTokens     int64  `json:"max_tokens"`
					ModelID       string `json:"model_id"`
					ModelDesc     string `json:"model_desc"`
				} `json:"model_parameters"`
			} `json:"models"`
		} `json:"gpts"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	if len(detail.Gpts.Models) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	out := make([]ModelInfo, 0, len(detail.Gpts.Models))
	for _, m := range detail.Gpts.Models {
		id := firstNonEmpty(m.ModelName, m.ModelID, m.ModelAlias, m.Params.ModelID)
		if id == "" {
			continue
		}
		out = append(out, ModelInfo{
			ID:            id,
			Name:          id,
			ContextWindow: m.Params.ContextWindow,
			MaxTokens:     m.Params.MaxTokens,
			Desc:          m.Params.ModelDesc,
		})
	}
	return out, nil
}

// fetchBuiltinModels GET /v1/model/builtin（Agent-Type: PromptCenter）。
func (c *Client) fetchBuiltinModels(cred SignCredential) ([]ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, SnapEngineApiHost+EpModelBuiltin, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, []byte{}, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: EpModelBuiltin}
	}
	var out struct {
		BuiltinModels []struct {
			ModelID       string `json:"model_id"`
			ModelName     string `json:"model_name"`
			ModelDesc     string `json:"model_desc"`
			ContextWindow int64  `json:"context_window"`
			MaxTokens     int64  `json:"max_tokens"`
		} `json:"builtinModels"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse builtin models: %w", err)
	}
	infos := make([]ModelInfo, 0, len(out.BuiltinModels))
	for _, m := range out.BuiltinModels {
		id := firstNonEmpty(m.ModelID, m.ModelName)
		if id == "" {
			continue
		}
		infos = append(infos, ModelInfo{ID: id, Name: id, ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, Desc: m.ModelDesc})
	}
	return infos, nil
}

// BenefitBalance 福利额度查询结果。
type BenefitBalance struct {
	UsedAmount   int64 `json:"used_amount"`
	TotalQuota   int64 `json:"total_quota"`
	TotalBalance int64 `json:"total_balance"`
}

// ClaimBenefit 领取限时福利（幂等：已领取返回成功）。官方客户端打开模型菜单即调用。
func (c *Client) ClaimBenefit(cred SignCredential) error {
	body, _ := json.Marshal(map[string]any{})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, BenefitHost+EpBenefitClaim, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, body, cred)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := c.http.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: EpBenefitClaim}
	}
	var out struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("parse claim: %w", err)
	}
	if out.ErrorCode != "0000" {
		return fmt.Errorf("claim failed: error_code=%s msg=%s", out.ErrorCode, out.ErrorMsg)
	}
	return nil
}

// fetchBenefitModels 拉取限时福利模型（先 claim 再取 gateway/config）。
func (c *Client) fetchBenefitModels(cred SignCredential) ([]ModelInfo, error) {
	if err := c.ClaimBenefit(cred); err != nil {
		Log("benefit claim: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, BenefitHost+EpBenefitConfig, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, []byte{}, cred)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := c.http.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: EpBenefitConfig}
	}
	var out struct {
		ErrorCode string `json:"error_code"`
		Result    struct {
			BaseURL string `json:"base_url"`
			Models  []struct {
				ModelID       string `json:"model_id"`
				ModelName     string `json:"model_name"`
				ContextWindow int64  `json:"context_window"`
				MaxTokens     int64  `json:"max_tokens"`
				ModelDesc     string `json:"model_desc"`
			} `json:"models"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse benefit config: %w", err)
	}
	if out.ErrorCode != "0000" {
		return nil, fmt.Errorf("gateway/config error_code=%s", out.ErrorCode)
	}
	infos := make([]ModelInfo, 0, len(out.Result.Models))
	for _, m := range out.Result.Models {
		id := firstNonEmpty(m.ModelID, m.ModelName)
		if id == "" {
			continue
		}
		infos = append(infos, ModelInfo{
			ID: id, Name: id,
			ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, Desc: m.ModelDesc,
			Benefit: true,
		})
	}
	return infos, nil
}

// defaultAgentID 拉取用户 agent 列表，返回 CodeAgent 的 agent_id。
// 过滤条件与官方客户端一致：agent_name=="CodeAgent" && alias.alias_zh_cn=="智能体" && show_in_ide，
// 退化为 is_primary_agent，再退化为首个。
func (c *Client) defaultAgentID(cred SignCredential) (string, error) {
	raw, err := c.getSigned(context.Background(), SnapEngineApiHost+EpAgentList+"?offset=0&limit=100&is_primary_agent=true", cred, true)
	if err != nil {
		return "", err
	}
	var out struct {
		Agents []struct {
			AgentID   string `json:"agent_id"`
			AgentName string `json:"agent_name"`
			Primary   bool   `json:"is_primary_agent"`
			ShowInIDE bool   `json:"show_in_ide"`
			Alias     struct {
				ZhCN string `json:"alias_zh_cn"`
			} `json:"alias"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parse agents: %w", err)
	}
	// 官方过滤：CodeAgent + alias 智能体 + show_in_ide
	for _, a := range out.Agents {
		if a.AgentName == "CodeAgent" && a.Alias.ZhCN == "智能体" && a.ShowInIDE {
			return a.AgentID, nil
		}
	}
	for _, a := range out.Agents {
		if a.Primary {
			return a.AgentID, nil
		}
	}
	if len(out.Agents) > 0 {
		return out.Agents[0].AgentID, nil
	}
	return "", nil
}

// getSigned 发送带 Agent-Type 的 AK/SK 签名 GET（用于 agent-center 等管理接口）。
func (c *Client) getSigned(ctx context.Context, urlStr string, cred SignCredential, agentCenter bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if agentCenter {
		req.Header.Set("Agent-Type", "AgentCenter")
	}
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, []byte{}, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: req.URL.Path}
	}
	return raw, nil
}
