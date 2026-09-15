// Package auth 管理 auths/ 目录下的 CodeArts 凭证文件 codearts-{user_id}.json。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Auth 单个账号凭证（登录 oauth2/tokens 返回）。
type Auth struct {
	UserID          string `json:"user_id"`
	UserName        string `json:"user_name"`
	DomainID        string `json:"domain_id"`
	CloudDragonTok  string `json:"cloud_dragon_token"` // = STS security_token，chat 用 x-auth-token
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Expiration      string `json:"expiration"` // RFC3339
	RefreshToken    string `json:"refresh_token"`
	CodeVerifier    string `json:"code_verifier"` // PKCE verifier，refresh 需要
	UpdatedAt       int64  `json:"updated_at"`

	// ClientID 签发该 refresh_token 的 OAuth client。refresh_token 与 client_id
	// 绑定，换 client_id 刷新会被 STS 拒（invalid client id）。留空表示旧凭证，
	// 走 DefaultLoginConfig 的默认值。
	ClientID string `json:"client_id,omitempty"`

	// DPoPPrivateKey JWK（RFC 9449，ES256）。refresh_token 与签发时的 DPoP 公钥
	// 绑定：刷新必须用同一密钥对，否则 STS 报 InvalidDPoPHeader。留空表示旧凭证
	// （刷新时退化为临时密钥，仅在服务端不校验绑定时可用）。
	DPoPPrivateKey map[string]string `json:"dpop_private_key,omitempty"`

	path string
	mu   sync.Mutex
}

// ClientIDOr 返回该凭证绑定的 OAuth client_id；未记录时返回 fallback。
func (a *Auth) ClientIDOr(fallback string) string {
	if a == nil {
		return fallback
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ClientID != "" {
		return a.ClientID
	}
	return fallback
}

// SetClientID 记录签发该 refresh_token 的 client_id。
func (a *Auth) SetClientID(v string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ClientID = v
}

// DPoPPrivateJWK 返回该凭证绑定的 DPoP 私钥副本（可能为 nil，表示旧凭证）。
func (a *Auth) DPoPPrivateJWK() map[string]string {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.DPoPPrivateKey) == 0 {
		return nil
	}
	out := make(map[string]string, len(a.DPoPPrivateKey))
	for k, v := range a.DPoPPrivateKey {
		out[k] = v
	}
	return out
}

// SetDPoPPrivateKey 记录该凭证绑定的 DPoP 私钥。
func (a *Auth) SetDPoPPrivateKey(jwk map[string]string) {
	if len(jwk) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.DPoPPrivateKey = make(map[string]string, len(jwk))
	for k, v := range jwk {
		a.DPoPPrivateKey[k] = v
	}
}

// New 构造 Auth（登录落盘用）。
func New(userID, userName, domainID, token, ak, sk, expiration, refreshToken, codeVerifier string, dpopPrivateJWK ...map[string]string) *Auth {
	a := &Auth{
		UserID:          userID,
		UserName:        userName,
		DomainID:        domainID,
		CloudDragonTok:  token,
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		Expiration:      expiration,
		RefreshToken:    refreshToken,
		CodeVerifier:    codeVerifier,
		UpdatedAt:       time.Now().Unix(),
	}
	if len(dpopPrivateJWK) > 0 && len(dpopPrivateJWK[0]) > 0 {
		a.DPoPPrivateKey = make(map[string]string, len(dpopPrivateJWK[0]))
		for k, v := range dpopPrivateJWK[0] {
			a.DPoPPrivateKey[k] = v
		}
	}
	return a
}

// FileName 返回 auth 文件名。
func (a *Auth) FileName() string {
	id := strings.ReplaceAll(a.UserID, string(filepath.Separator), "_")
	if id == "" {
		id = "unknown"
	}
	return fmt.Sprintf("codearts-%s.json", id)
}

// Token 返回 cloud_dragon_token（并发安全快照）。
func (a *Auth) Token() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.CloudDragonTok
}

// Refresh 返回 refresh_token。
func (a *Auth) Refresh() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// Verifier 返回 PKCE code_verifier。
func (a *Auth) Verifier() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.CodeVerifier
}

// Credentials 返回签名请求所需的 STS 凭据快照。
func (a *Auth) Credentials() (token, accessKeyID, secretAccessKey string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.CloudDragonTok, a.AccessKeyID, a.SecretAccessKey
}

// UpdateCredentials 原子替换刷新后的 STS 凭据并写回磁盘。
func (a *Auth) UpdateCredentials(token, accessKeyID, secretAccessKey, expiration, refreshToken string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.CloudDragonTok = token
	a.AccessKeyID = accessKeyID
	a.SecretAccessKey = secretAccessKey
	a.Expiration = expiration
	if refreshToken != "" {
		a.RefreshToken = refreshToken
	}
	a.UpdatedAt = time.Now().Unix()
	return saveLocked(a.path, a)
}

// UpdateIdentity 回填登录身份字段（WebUI 回调可能拿不到 user_id/user_name）。
func (a *Auth) UpdateIdentity(userID, userName, domainID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if userID != "" {
		a.UserID = userID
	}
	if userName != "" {
		a.UserName = userName
	}
	if domainID != "" {
		a.DomainID = domainID
	}
	a.UpdatedAt = time.Now().Unix()
	return saveLocked(a.path, a)
}

// SetPathAndSave 设置落盘路径并写出（WebUI 登录在拿到身份后才能确定文件名）。
func (a *Auth) SetPathAndSave(path string) error {
	a.mu.Lock()
	a.path = path
	a.mu.Unlock()
	return a.Save()
}

// ExpiresAt 返回 token 过期时间。
func (a *Auth) ExpiresAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, err := time.Parse(time.RFC3339, a.Expiration); err == nil {
		return t
	}
	return time.Time{}
}

// Remaining 返回剩余有效期。
func (a *Auth) Remaining() time.Duration {
	t := a.ExpiresAt()
	if t.IsZero() {
		return 24 * time.Hour
	}
	return time.Until(t)
}

// Expired 报告 token 是否已过期。
func (a *Auth) Expired() bool { return a.Remaining() <= 0 }

// ExpiringSoon 报告 token 是否将在 within 内过期。
func (a *Auth) ExpiringSoon(within time.Duration) bool {
	r := a.Remaining()
	return r > 0 && r <= within
}

// Save 原子写回 auth 文件（0600）。
func (a *Auth) Save() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return saveLocked(a.path, a)
}

// SetPath 设置文件路径（LoadDir 内部调用）。
func (a *Auth) SetPath(p string) { a.path = p }

// LoadDir 加载 auths 目录下全部 codearts-*.json。
func LoadDir(dir string) ([]*Auth, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Auth
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "codearts-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var a Auth
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		if a.CloudDragonTok == "" {
			continue
		}
		a.path = p
		out = append(out, &a)
	}
	return out, nil
}

// SaveNew 把新账号写入 auths 目录。
func SaveNew(dir string, a *Auth) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	a.path = filepath.Join(dir, a.FileName())
	return a.Save()
}

func saveLocked(path string, v any) error {
	if path == "" {
		return fmt.Errorf("auth: empty path")
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
