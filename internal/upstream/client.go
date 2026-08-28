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

// ChatOptions 是需要原样传给 OpenAI 兼容上游的可选生成参数。
type ChatOptions struct {
	ReasoningEffort string
	MaxTokens       *int
	Temperature     *float64
	TopP            *float64
}

// CanonicalModel 把用户友好模型 ID 映射为 InferHub 注册的模型 ID（区分大小写）。
// 旧版 /v1/chat/chat 用小写 id（glm-5.2 / snap-chat），新 /api/v2/chat/completions
// 按 InferHub 注册名匹配（GLM-5.2 / deepseek-v4-flash / Qwen3-VL-235B）。
func CanonicalModel(id string) string {
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
	return c.ChatStreamWithOptions(ctx, chatID, messages, traceID, cred, userName, model, ChatOptions{})
}

// ChatStreamWithOptions 在基础聊天请求上附加推理等级与采样参数。
func (c *Client) ChatStreamWithOptions(ctx context.Context, chatID string, messages []ChatMessage, traceID string, cred SignCredential, userName string, model string, opts ChatOptions) (io.ReadCloser, error) {
	body := map[string]any{
		"model":    CanonicalModel(model),
		"stream":   true,
		"messages": chatMessagesToOpenAI(messages),
	}
	if opts.ReasoningEffort != "" {
		body["reasoning_effort"] = opts.ReasoningEffort
	}
	if opts.MaxTokens != nil {
		body["max_tokens"] = *opts.MaxTokens
	}
	if opts.Temperature != nil {
		body["temperature"] = *opts.Temperature
	}
	if opts.TopP != nil {
		body["top_p"] = *opts.TopP
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
func (c *Client) SendChatV2(ctx context.Context, body map[string]any, traceID string, cred SignCredential, userToken string) (io.ReadCloser, error) {
	raw, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, SnapEngineApiHost+EpChatV2, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range ChatHeadersV2(userToken, traceID, "zh-cn") {
		httpReq.Header.Set(k, v)
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
}

// FetchModels 从 agent-center 拉取当前账号可用的模型列表（动态缓存 1h）。
// 链路：useragents 找默认 CodeAgent → detail 的 gpts.models 返回精确模型 ID。
func (c *Client) FetchModels(acct *auth.Auth) ([]ModelInfo, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for model fetch")
	}
	cred := SignCredential{
		AccessKeyID:     acct.AccessKeyID,
		SecretAccessKey: acct.SecretAccessKey,
		SecurityToken:   acct.CloudDragonTok,
	}
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
				Params     struct {
					ContextWindow int64 `json:"context_window"`
					MaxTokens     int64 `json:"max_tokens"`
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
		id := firstNonEmpty(m.ModelName, m.ModelAlias)
		if id == "" {
			continue
		}
		out = append(out, ModelInfo{
			ID:            id,
			Name:          id,
			ContextWindow: m.Params.ContextWindow,
			MaxTokens:     m.Params.MaxTokens,
		})
	}
	return out, nil
}

// defaultAgentID 拉取用户 agent 列表，返回默认 CodeAgent 的 agent_id。
func (c *Client) defaultAgentID(cred SignCredential) (string, error) {
	raw, err := c.getSigned(context.Background(), SnapEngineApiHost+EpAgentList+"?offset=0&limit=100", cred, true)
	if err != nil {
		return "", err
	}
	var out struct {
		Agents []struct {
			AgentID   string `json:"agent_id"`
			AgentName string `json:"agent_name"`
			Primary   bool   `json:"is_primary_agent"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parse agents: %w", err)
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
