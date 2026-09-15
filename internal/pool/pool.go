// Package pool 管理 CodeArts 账号池：token 校验/自动刷新、冷却、禁用与轮转。
package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

// CoolKind 冷却原因。
type CoolKind int

const (
	CoolSoft CoolKind = iota
	CoolErr
	CoolPlan
)

// Account 一个上游账号。
type Account struct {
	Name         string `json:"name"`
	UID          string `json:"uid"`
	UserName     string `json:"user_name"`
	DefaultModel string `json:"default_model"`

	Auth   *auth.Auth       `json:"-"`
	Client *upstream.Client `json:"-"`

	mu                sync.Mutex
	refreshMu         sync.Mutex
	lastValidated     time.Time
	errCount          int
	coolUntil         time.Time
	coolKind          CoolKind
	disabled          bool
	disabledReason    string
	lastErr           string
	activeConcurrent  int       // 当前活跃并发请求数
	maxConcurrent     int       // 最大允许并发数
	keepaliveLastPing time.Time // 最后保活心跳时间
}

// Config 池配置。
type Config struct {
	ErrThreshold    int
	ErrCooldown     time.Duration
	SoftCooldown    time.Duration
	RefreshSkew     time.Duration        // token 到期前多久自动刷新
	MaxConcurrent   int                  // 单账号最大并发数（默认 5）
	KeepaliveWindow time.Duration        // 保活心跳窗口，超过此时间无活动则发送心跳
	LoginConfig     upstream.LoginConfig // OAuth/refresh 端点；零值使用生产默认
}

// Pool 账号池。
type Pool struct {
	cfg      Config
	state    string
	mu       sync.Mutex
	accounts []*Account
}

// New 构建池。
func New(auths []*auth.Auth, cfg Config, stateFile string) (*Pool, error) {
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 30 * time.Minute
	}
	if cfg.MaxConcurrent <= 0 {
		// 上游单账号最多 3 个并发会话（TM.00001041），但会话槽位释放慢，
		// 串行（1）最稳，避免撞限。可按多账号情况调高。
		cfg.MaxConcurrent = 1
	}
	if cfg.KeepaliveWindow <= 0 {
		cfg.KeepaliveWindow = 10 * time.Minute // 10 分钟无活动则保活
	}
	if cfg.LoginConfig.ClientID == "" {
		cfg.LoginConfig = upstream.DefaultLoginConfig()
	}
	p := &Pool{cfg: cfg, state: stateFile}
	for _, a := range auths {
		acct := &Account{
			Name:              a.UserID,
			UID:               a.UserID,
			UserName:          a.UserName,
			Auth:              a,
			Client:            upstream.New(120 * time.Second),
			maxConcurrent:     cfg.MaxConcurrent,
			keepaliveLastPing: time.Now(),
		}
		p.accounts = append(p.accounts, acct)
	}
	p.loadState()
	return p, nil
}

// Accounts 返回全部账号。
func (p *Pool) Accounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accounts
}

// Get 按名字取账号。
func (p *Pool) Get(name string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// AddAccount 动态添加账号（WebUI 登录成功后调用）。
func (p *Pool) AddAccount(a *auth.Auth) *Account {
	acct := &Account{
		Name:              a.UserID,
		UID:               a.UserID,
		UserName:          a.UserName,
		Auth:              a,
		Client:            upstream.New(120 * time.Second),
		maxConcurrent:     p.cfg.MaxConcurrent,
		keepaliveLastPing: time.Now(),
	}
	p.mu.Lock()
	for i, existing := range p.accounts {
		if existing.Name == acct.Name {
			existing.mu.Lock()
			existing.Auth = a
			existing.UserName = a.UserName
			existing.maxConcurrent = p.cfg.MaxConcurrent
			existing.lastValidated = time.Time{}
			existing.disabled = false
			existing.disabledReason = ""
			existing.lastErr = ""
			existing.mu.Unlock()
			p.mu.Unlock()
			p.saveState()
			return existing
		}
		p.accounts[i] = existing
	}
	p.accounts = append(p.accounts, acct)
	p.mu.Unlock()
	p.saveState()
	log.Printf("pool account added name=%s uid=%s", acct.Name, a.UserID)
	return acct
}

// List 状态快照（脱敏；字段对齐 WorkBuddy 面板）。
func (p *Pool) List() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.accounts))
	now := time.Now()
	for _, a := range p.accounts {
		a.mu.Lock()
		cooling := !a.disabled && now.Before(a.coolUntil)
		until := ""
		if cooling {
			until = a.coolUntil.Format(time.RFC3339)
		}
		reason := a.disabledReason
		if reason == "" {
			reason = a.lastErr
		}
		out = append(out, map[string]any{
			"name":              a.Name,
			"uid":               a.UID,
			"nickname":          a.UserName,
			"user_name":         a.UserName,
			"default_model":     a.DefaultModel,
			"credits":           int64(0), // CodeArts 无积分字段；面板仍显示
			"disabled":          a.disabled,
			"cooling":           cooling,
			"until":             until,
			"err_count":         a.errCount,
			"reason":            reason,
			"last_error":        a.lastErr,
			"active_concurrent": a.activeConcurrent,
			"max_concurrent":    a.maxConcurrent,
			"token_remaining":   a.Auth.Remaining().Round(time.Minute).String(),
			"expires_at":        a.Auth.ExpiresAt().Format(time.RFC3339),
		})
		a.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["uid"].(string) < out[j]["uid"].(string) })
	return out
}

// Stats 汇总。
func (p *Pool) Stats() (total, healthy, disabled, cooling int, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, a := range p.accounts {
		total++
		a.mu.Lock()
		if a.disabled {
			disabled++
		} else if now.Before(a.coolUntil) {
			cooling++
		} else {
			healthy++
		}
		a.mu.Unlock()
	}
	credits = int64(healthy) // 面板「积分合计」位复用为健康数
	return
}

// Enable 解禁账号。
func (p *Pool) Enable(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name || a.UID == name {
			a.mu.Lock()
			a.disabled = false
			a.disabledReason = ""
			a.lastErr = ""
			a.mu.Unlock()
			p.saveState()
			return true
		}
	}
	return false
}

// ClearCooldown 清冷却。
func (p *Pool) ClearCooldown(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name || a.UID == name {
			a.mu.Lock()
			a.coolUntil = time.Time{}
			a.coolKind = CoolSoft
			a.mu.Unlock()
			p.saveState()
			return true
		}
	}
	return false
}

// SyncToDir 用磁盘 auths 全量对齐。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	byUID := map[string]*Account{}
	for _, a := range p.accounts {
		byUID[a.UID] = a
	}
	next := make([]*Account, 0, len(auths))
	for _, a := range auths {
		if existing, ok := byUID[a.UserID]; ok {
			existing.mu.Lock()
			existing.Auth = a
			existing.UserName = a.UserName
			existing.maxConcurrent = p.cfg.MaxConcurrent
			existing.mu.Unlock()
			next = append(next, existing)
			delete(byUID, a.UserID)
		} else {
			next = append(next, &Account{
				Name:              a.UserID,
				UID:               a.UserID,
				UserName:          a.UserName,
				Auth:              a,
				Client:            upstream.New(120 * time.Second),
				maxConcurrent:     p.cfg.MaxConcurrent,
				keepaliveLastPing: time.Now(),
			})
		}
	}
	p.accounts = next
}

// PickExcluding 挑一个健康账号。
func (p *Pool) PickExcluding(tried map[string]bool) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, a := range p.accounts {
		if tried != nil && tried[a.Name] {
			continue
		}
		a.mu.Lock()
		healthy := !a.disabled && now.After(a.coolUntil)
		a.mu.Unlock()
		if healthy {
			return a
		}
	}
	return nil
}

// Validate 校验并（必要时）自动刷新 token。带 5 分钟缓存。
func (p *Pool) Validate(a *Account) (bool, error) {
	a.mu.Lock()
	if time.Since(a.lastValidated) < 5*time.Minute && !a.disabled {
		ok := true
		a.mu.Unlock()
		return ok, nil
	}
	a.mu.Unlock()

	authz := a.Auth
	if authz.Expired() && authz.Refresh() == "" {
		p.Disable(a.Name, "token expired; re-login required")
		return false, nil
	}
	if authz.ExpiringSoon(p.cfg.RefreshSkew) || authz.Expired() {
		if authz.Refresh() == "" {
			// 没有 refresh_token（华为云 ticket 通道不返回），不立即 disable，
			// 未过期时只打告警；到期后由上面的分支明确标记为需重新登录。
			log.Printf("pool token account=%s expiring soon (remaining=%s) but no refresh_token, re-login required",
				a.Name, authz.Remaining().Round(time.Minute))
			a.mu.Lock()
			a.lastValidated = time.Now()
			a.mu.Unlock()
			return true, nil
		}
		if err := p.RefreshToken(a.Name); err != nil {
			var expired *upstream.RefreshTokenExpiredError
			if errors.As(err, &expired) {
				p.Disable(a.Name, "refresh_token expired; re-login required")
				return false, nil
			}
			// 网络/5xx 属于可重试错误：保持账号可调度，由后续
			// watchdog tick 重试，不得把短暂故障固化为永久禁用。
			return false, err
		}
	}
	a.mu.Lock()
	a.lastValidated = time.Now()
	a.mu.Unlock()
	return true, nil
}

// Cooldown 冷却账号。
func (p *Pool) Cooldown(name string, kind CoolKind, dur time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		a.coolUntil = time.Now().Add(dur)
		a.coolKind = kind
		a.lastErr = reason
		a.mu.Unlock()
		log.Printf("pool cooldown account=%s kind=%d dur=%s reason=%s", name, kind, dur, reason)
	}
	p.saveState()
}

// NoteError 累计错误。
func (p *Pool) NoteError(name string, threshold int, cooldown time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		a.errCount++
		if a.errCount >= threshold {
			a.coolUntil = time.Now().Add(cooldown)
			a.lastErr = "consecutive errors"
		}
		a.mu.Unlock()
	}
	p.saveState()
}

// NoteSuccess 清零错误计数。
func (p *Pool) NoteSuccess(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			a.errCount = 0
			a.mu.Unlock()
		}
	}
}

// Disable 禁用账号。
func (p *Pool) Disable(name, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			a.disabled = true
			a.disabledReason = reason
			a.lastErr = reason
			a.mu.Unlock()
			log.Printf("pool disable account=%s reason=%s", name, reason)
		}
	}
	p.saveState()
}

// Healthy 查询账号可用。
// Busy 报告账号当前是否有活跃请求占用并发槽位（供探测等后台任务让位）。
func (p *Pool) Busy(name string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return false
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	return target.activeConcurrent > 0
}

func (p *Pool) Healthy(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			h := !a.disabled && time.Now().After(a.coolUntil)
			a.mu.Unlock()
			return h
		}
	}
	return false
}

// AcquireLock 尝试获取账号的并发请求锁。返回 true 表示成功，false 表示已达上限。
func (p *Pool) AcquireLock(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.activeConcurrent >= a.maxConcurrent {
			log.Printf("pool concurrent limit reached account=%s current=%d max=%d",
				name, a.activeConcurrent, a.maxConcurrent)
			return false
		}
		a.activeConcurrent++
		return true
	}
	return false
}

// ReleaseLock 释放账号的并发请求锁，并唤醒等待槽位的请求。
func (p *Pool) ReleaseLock(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			if a.activeConcurrent > 0 {
				a.activeConcurrent--
			}
			a.mu.Unlock()
			break
		}
	}
	// 唤醒等待槽位的 AcquireLockWait（轮询方式无需 broadcast，此处保留语义占位）。
}

// AcquireLockWait 阻塞等待账号并发槽位，超时返回 false。
//
// 上游单账号并发会话上限 3，本池 maxConcurrent 默认 2 留余量。
// 高并发请求在此排队等待槽位释放，而非立即失败跳号。
// timeout=0 时等价于非阻塞 AcquireLock。
func (p *Pool) AcquireLockWait(name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		p.mu.Lock()
		acquired := false
		for _, a := range p.accounts {
			if a.Name != name {
				continue
			}
			a.mu.Lock()
			if a.activeConcurrent < a.maxConcurrent {
				a.activeConcurrent++
				acquired = true
			}
			a.mu.Unlock()
			break
		}
		p.mu.Unlock()
		if acquired {
			return true
		}
		if timeout <= 0 || time.Now().After(deadline) {
			return false
		}
		// 避免忙轮询：每次 100ms 重试。
		time.Sleep(100 * time.Millisecond)
	}
}

// NeedKeepalive 检查账号是否需要保活心跳。
func (p *Pool) NeedKeepalive(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		// 超过 KeepaliveWindow 无活动则需保活
		return time.Since(a.keepaliveLastPing) > p.cfg.KeepaliveWindow
	}
	return false
}

// PingKeepalive 更新账号的保活心跳时间。
func (p *Pool) PingKeepalive(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			a.keepaliveLastPing = time.Now()
			a.mu.Unlock()
			break
		}
	}
}

// CheckAndRefreshToken 检查 token 是否快过期并主动刷新（比 scheduler 更积极）。
func (p *Pool) CheckAndRefreshToken(name string) error {
	p.mu.Lock()
	var acct *Account
	for _, a := range p.accounts {
		if a.Name == name {
			acct = a
			break
		}
	}
	p.mu.Unlock()
	if acct == nil {
		return fmt.Errorf("account not found: %s", name)
	}

	acct.mu.Lock()
	remaining := acct.Auth.Remaining()
	acct.mu.Unlock()

	// 如果 token 剩余少于 1 小时，主动刷新
	if remaining <= time.Hour {
		if acct.Auth.Refresh() == "" {
			return nil
		}
		log.Printf("pool proactive refresh account=%s remaining=%s", name, remaining)
		return p.RefreshToken(name)
	}
	return nil
}

// RefreshToken 手动刷新指定账号的 token。
func (p *Pool) RefreshToken(name string) error {
	p.mu.Lock()
	var acct *Account
	for _, a := range p.accounts {
		if a.Name == name {
			acct = a
			break
		}
	}
	p.mu.Unlock()
	if acct == nil {
		return fmt.Errorf("account not found: %s", name)
	}
	acct.refreshMu.Lock()
	defer acct.refreshMu.Unlock()

	authz := acct.Auth
	refreshToken := authz.Refresh()
	if refreshToken == "" {
		return fmt.Errorf("no refresh_token available")
	}
	dpopPrivateJWK := authz.DPoPPrivateJWK()
	if dpopPrivateJWK["d"] == "" {
		return fmt.Errorf("no DPoP private key available; re-login required")
	}

	resp, err := acct.Client.RefreshToken(context.Background(), p.cfg.LoginConfig, refreshToken, authz.Verifier(), upstream.DPoPPrivateJWK(dpopPrivateJWK))
	if err != nil {
		return fmt.Errorf("refresh failed: %w", err)
	}

	if err := authz.UpdateCredentials(
		resp.Credentials.SecurityToken,
		resp.Credentials.AccessKeyID,
		resp.Credentials.SecretAccessKey,
		resp.Credentials.Expiration,
		resp.RefreshToken,
	); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	acct.mu.Lock()
	acct.disabled = false
	acct.disabledReason = ""
	acct.lastErr = ""
	acct.lastValidated = time.Now()
	acct.mu.Unlock()
	log.Printf("pool token refreshed manually account=%s", name)
	return nil
}

// RefreshAllTokens 批量刷新所有临期 token（用于 cron 或脚本）。
func (p *Pool) RefreshAllTokens() int {
	count := 0
	for _, acct := range p.Accounts() {
		if err := p.CheckAndRefreshToken(acct.Name); err == nil {
			count++
		}
	}
	return count
}

// Acquire 尝试获取账号并发 slot（返回 true 表示成功，调用方需随后 Release）。
func (p *Pool) Acquire(name string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return false
	}
	target.mu.Lock()
	defer target.mu.Unlock()

	// 检查是否超过最大并发限制
	if target.activeConcurrent >= target.maxConcurrent {
		return false
	}
	// 检查是否正在冷却
	if !time.Now().After(target.coolUntil) {
		return false
	}
	// 增加活跃计数
	target.activeConcurrent++
	target.keepaliveLastPing = time.Now()
	return true
}

// Release 释放账号并发 slot。
func (p *Pool) Release(name string) {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.activeConcurrent > 0 {
		target.activeConcurrent--
	}
}

// CheckKeepalive 检查是否需要保活心跳。
func (p *Pool) CheckKeepalive(name string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return false
	}
	target.mu.Lock()
	defer target.mu.Unlock()

	// 如果超过保活窗口无活动，需要保活
	needsKeepalive := time.Since(target.keepaliveLastPing) > p.cfg.KeepaliveWindow
	if needsKeepalive {
		target.keepaliveLastPing = time.Now()
	}
	return needsKeepalive
}

// SetKeepalive 手动更新保活时间。
func (p *Pool) SetKeepalive(name string) {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	target.keepaliveLastPing = time.Now()
}

// GetConcurrentStats 获取账号并发统计信息。
func (p *Pool) GetConcurrentStats() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]map[string]any, 0, len(p.accounts))
	now := time.Now()
	for _, a := range p.accounts {
		a.mu.Lock()
		cooling := !a.disabled && now.Before(a.coolUntil)
		concurrent := a.activeConcurrent
		maxConc := a.maxConcurrent
		lastPing := a.keepaliveLastPing
		a.mu.Unlock()

		out = append(out, map[string]any{
			"name":              a.Name,
			"uid":               a.UID,
			"user_name":         a.UserName,
			"active_concurrent": concurrent,
			"max_concurrent":    maxConc,
			"utilization":       float64(concurrent) / float64(maxConc),
			"last_keepalive":    lastPing.Format(time.RFC3339),
			"cooling":           cooling,
			"disabled":          a.disabled,
		})
	}
	return out
}

// TryAcquire 尝试获取账号的并发槽位，成功返回 true 并增加计数。
func (p *Pool) TryAcquire(name string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()

	if target == nil {
		return false
	}

	target.mu.Lock()
	defer target.mu.Unlock()

	if target.disabled || time.Now().Before(target.coolUntil) {
		return false
	}

	if target.activeConcurrent >= target.maxConcurrent {
		log.Printf("pool concurrent limit reached account=%s active=%d max=%d",
			name, target.activeConcurrent, target.maxConcurrent)
		return false
	}

	target.activeConcurrent++
	target.keepaliveLastPing = time.Now()
	return true
}

// NeedsKeepalive 检查账号是否需要保活心跳。
func (p *Pool) NeedsKeepalive(name string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()

	if target == nil {
		return false
	}

	target.mu.Lock()
	defer target.mu.Unlock()

	// 超过窗口期无活动则需保活
	return time.Since(target.keepaliveLastPing) > p.cfg.KeepaliveWindow
}

// UpdateKeepalive 更新账号的最后保活时间。
func (p *Pool) UpdateKeepalive(name string) {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()

	if target != nil {
		target.mu.Lock()
		target.keepaliveLastPing = time.Now()
		target.mu.Unlock()
	}
}

// SetMaxConcurrent 设置账号的最大并发数。
func (p *Pool) SetMaxConcurrent(name string, max int) {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()

	if target != nil {
		target.mu.Lock()
		target.maxConcurrent = max
		target.mu.Unlock()
	}
}

func (p *Pool) loadState() {
	if p.state == "" {
		return
	}
	raw, err := os.ReadFile(p.state)
	if err != nil {
		return
	}
	var data []struct {
		Name      string    `json:"name"`
		ErrCount  int       `json:"err_count"`
		CoolUntil time.Time `json:"cool_until"`
		Disabled  bool      `json:"disabled"`
		Reason    string    `json:"reason"`
		LastErr   string    `json:"last_error"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	for _, a := range p.accounts {
		for _, s := range data {
			if s.Name == a.Name {
				a.mu.Lock()
				a.errCount = s.ErrCount
				a.coolUntil = s.CoolUntil
				a.disabled = s.Disabled
				a.disabledReason = s.Reason
				a.lastErr = s.LastErr
				a.mu.Unlock()
			}
		}
	}
}

func (p *Pool) saveState() {
	if p.state == "" {
		return
	}
	type entry struct {
		Name      string    `json:"name"`
		ErrCount  int       `json:"err_count"`
		CoolUntil time.Time `json:"cool_until"`
		Disabled  bool      `json:"disabled"`
		Reason    string    `json:"reason"`
		LastErr   string    `json:"last_error"`
	}
	out := make([]entry, 0, len(p.accounts))
	for _, a := range p.accounts {
		a.mu.Lock()
		out = append(out, entry{
			Name:      a.Name,
			ErrCount:  a.errCount,
			CoolUntil: a.coolUntil,
			Disabled:  a.disabled,
			Reason:    a.disabledReason,
			LastErr:   a.lastErr,
		})
		a.mu.Unlock()
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	tmp := p.state + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.state)
}
