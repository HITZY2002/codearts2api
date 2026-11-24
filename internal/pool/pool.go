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
	"codearts2api/internal/telemetry"
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

	mu                 sync.Mutex
	refreshMu          sync.Mutex
	checkinMu          sync.Mutex
	checkin            *CheckinStatus
	checkinAttemptedAt time.Time
	lastValidated      time.Time
	errCount           int
	coolUntil          time.Time
	coolKind           CoolKind
	disabled           bool
	manualDisabled     bool // 管理员手动禁用：token 刷新成功不得解除，仅 Enable 显式解禁
	disabledReason     string
	lastErr            string
	activeConcurrent   int       // 当前活跃并发请求数
	maxConcurrent      int       // 最大允许并发数
	keepaliveLastPing  time.Time // 最后保活心跳时间
}

// Config 池配置。
type Config struct {
	ErrThreshold    int
	ErrCooldown     time.Duration
	SoftCooldown    time.Duration
	RefreshSkew     time.Duration        // token 到期前多久自动刷新
	MaxConcurrent   int                  // 单账号最大并发数（默认 5）
	KeepaliveWindow time.Duration        // 保活心跳窗口，超过此时间无活动则发送心跳
	UpstreamTimeout time.Duration        // 单个上游请求超时，默认 120s
	LoginConfig     upstream.LoginConfig // OAuth/refresh 端点；零值使用生产默认
	CheckinClient   CheckinClient        // 可选活动客户端；nil 使用账号 Client
	KeepaliveClient KeepaliveClient      // 可选只读保活客户端；nil 使用账号 Client
	Telemetry       *telemetry.Store
}

// Pool 账号池。
type Pool struct {
	cfg      Config
	state    string
	stateMu  sync.Mutex
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
	if cfg.UpstreamTimeout <= 0 {
		cfg.UpstreamTimeout = 120 * time.Second
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
			Client:            upstream.New(cfg.UpstreamTimeout),
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
	return append([]*Account(nil), p.accounts...)
}

// SetRuntimeConfig updates concurrency and token refresh limits for existing accounts.
func (p *Pool) SetRuntimeConfig(maxConcurrent int, refreshSkew, keepaliveWindow time.Duration) {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	if refreshSkew <= 0 {
		refreshSkew = 30 * time.Minute
	}
	if keepaliveWindow <= 0 {
		keepaliveWindow = 10 * time.Minute
	}
	p.mu.Lock()
	p.cfg.MaxConcurrent = maxConcurrent
	p.cfg.RefreshSkew = refreshSkew
	p.cfg.KeepaliveWindow = keepaliveWindow
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	for _, account := range accounts {
		account.mu.Lock()
		account.maxConcurrent = maxConcurrent
		account.mu.Unlock()
	}
}

// AuthSnapshot returns the current credential pointer under the account lock.
// The Auth object has its own lock for subsequent credential reads.
func (a *Account) AuthSnapshot() *auth.Auth {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Auth
}

// UserNameSnapshot returns the current user display name under the account lock.
func (a *Account) UserNameSnapshot() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.UserName
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
	uid := a.UserIDSnapshot()
	p.mu.Lock()
	upstreamTimeout := p.cfg.UpstreamTimeout
	maxConcurrent := p.cfg.MaxConcurrent
	for _, existing := range p.accounts {
		if existing.Name == uid {
			existing.mu.Lock()
			existing.Auth = a
			existing.UserName = a.UserName
			existing.maxConcurrent = maxConcurrent
			existing.lastValidated = time.Time{}
			existing.disabled = false
			existing.manualDisabled = false
			existing.disabledReason = ""
			existing.lastErr = ""
			existing.mu.Unlock()
			p.mu.Unlock()
			p.saveState()
			return existing
		}
	}
	acct := &Account{
		Name:              uid,
		UID:               uid,
		UserName:          a.UserName,
		Auth:              a,
		Client:            upstream.New(upstreamTimeout),
		maxConcurrent:     maxConcurrent,
		keepaliveLastPing: time.Now(),
	}
	p.accounts = append(p.accounts, acct)
	p.mu.Unlock()
	p.saveState()
	log.Printf("pool account added name=%s uid=%s", acct.Name, uid)
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
		var checkinCopy *CheckinStatus
		if a.checkin != nil {
			cp := *a.checkin
			checkinCopy = &cp
		}
		out = append(out, map[string]any{
			"name":              a.Name,
			"uid":               a.UID,
			"nickname":          a.UserName,
			"user_name":         a.UserName,
			"default_model":     a.DefaultModel,
			"credits":           int64(0), // 保留旧字段；未查询真实余额
			"checkin":           checkinCopy,
			"disabled":          a.disabled,
			"manual_disabled":   a.manualDisabled,
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
	credits = 0 // 真实余额未确认，不再用健康数冒充积分
	return
}

// Enable 解禁账号。
func (p *Pool) Enable(name string) bool {
	p.mu.Lock()
	found := false
	for _, a := range p.accounts {
		if a.Name == name || a.UID == name {
			a.mu.Lock()
			a.disabled = false
			a.manualDisabled = false
			a.disabledReason = ""
			a.lastErr = ""
			a.mu.Unlock()
			found = true
			break
		}
	}
	p.mu.Unlock()
	if found {
		p.saveState()
	}
	return found
}

// ClearCooldown 清冷却并重置连续错误计数。
func (p *Pool) ClearCooldown(name string) bool {
	p.mu.Lock()
	found := false
	for _, a := range p.accounts {
		if a.Name == name || a.UID == name {
			a.mu.Lock()
			a.coolUntil = time.Time{}
			a.coolKind = CoolSoft
			a.errCount = 0
			a.lastErr = ""
			a.mu.Unlock()
			found = true
			break
		}
	}
	p.mu.Unlock()
	if found {
		p.saveState()
	}
	return found
}

// ClearAllCooldowns 清除全部处于冷却或有错误计数的账号状态，返回受影响账号数。
func (p *Pool) ClearAllCooldowns() int {
	p.mu.Lock()
	now := time.Now()
	cleared := 0
	for _, a := range p.accounts {
		a.mu.Lock()
		if now.Before(a.coolUntil) || a.errCount > 0 {
			a.coolUntil = time.Time{}
			a.coolKind = CoolSoft
			a.errCount = 0
			a.lastErr = ""
			cleared++
		}
		a.mu.Unlock()
	}
	p.mu.Unlock()
	if cleared > 0 {
		p.saveState()
	}
	return cleared
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
				Client:            upstream.New(p.cfg.UpstreamTimeout),
				maxConcurrent:     p.cfg.MaxConcurrent,
				keepaliveLastPing: time.Now(),
			})
		}
	}
	p.accounts = next
}

// PickExcluding 挑一个健康账号：并发利用率（activeConcurrent/maxConcurrent）
// 最低者优先，让同时进来的新会话摊开到整个账号池，而不是全挤在配置序第一个
// 账号上排队；利用率相同保持配置顺序。槽位排队与错误轮转仍由调用方兜底。
func (p *Pool) PickExcluding(tried map[string]bool) *Account {
	return p.PickWhere(tried, nil)
}

// PickWhere 在满足 match 条件的健康账号里按并发利用率最低者优先挑选。
// match 为 nil 时不附加过滤，等价于 PickExcluding。
func (p *Pool) PickWhere(tried map[string]bool, match func(*Account) bool) *Account {
	p.mu.Lock()
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	now := time.Now()
	var best *Account
	bestUtil := 0.0
	for _, a := range accounts {
		if a == nil || (tried != nil && tried[a.Name]) {
			continue
		}
		a.mu.Lock()
		healthy := !a.disabled && now.After(a.coolUntil)
		util := 0.0
		if a.maxConcurrent > 0 {
			util = float64(a.activeConcurrent) / float64(a.maxConcurrent)
		}
		a.mu.Unlock()
		if !healthy {
			continue
		}
		if match != nil && !match(a) {
			continue
		}
		if best == nil || util < bestUtil {
			best, bestUtil = a, util
		}
	}
	return best
}

// Validate 校验并（必要时）自动刷新 token。带 5 分钟缓存。
func (p *Pool) Validate(a *Account) (bool, error) {
	return p.ValidateContext(context.Background(), a)
}

// ValidateContext 在给定上下文超时约束内校验并按需续期账号 token。
func (p *Pool) ValidateContext(ctx context.Context, a *Account) (bool, error) {
	p.mu.Lock()
	refreshSkew := p.cfg.RefreshSkew
	p.mu.Unlock()

	authz := a.AuthSnapshot()
	a.mu.Lock()
	if time.Since(a.lastValidated) < 5*time.Minute && !a.disabled && !authz.Expired() && !authz.ExpiringSoon(refreshSkew) {
		ok := true
		a.mu.Unlock()
		return ok, nil
	}
	a.mu.Unlock()
	if authz.Expired() && authz.Refresh() == "" {
		p.Disable(a.Name, "token expired; re-login required")
		return false, nil
	}
	if authz.ExpiringSoon(refreshSkew) || authz.Expired() {
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
		if err := p.refreshToken(ctx, a.Name, false, refreshSkew); err != nil {
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
	p.mu.Unlock()
	p.saveState()
	if p.cfg.Telemetry != nil {
		_ = p.cfg.Telemetry.RecordEvent("pool_cooldown", "warn", name, "", reason, nil)
	}
}

// NoteError 累计错误。
func (p *Pool) NoteError(name string, threshold int, cooldown time.Duration) {
	p.mu.Lock()
	found := false
	cooled := false
	errCount := 0
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		found = true
		a.mu.Lock()
		a.errCount++
		errCount = a.errCount
		if a.errCount >= threshold {
			a.coolUntil = time.Now().Add(cooldown)
			a.coolKind = CoolErr
			a.lastErr = "consecutive errors"
			cooled = true
		}
		a.mu.Unlock()
	}
	p.mu.Unlock()
	if !found {
		return
	}
	p.saveState()
	if p.cfg.Telemetry != nil {
		msg := fmt.Sprintf("error count %d/%d", errCount, threshold)
		if cooled {
			msg = "consecutive errors; cooldown triggered"
		}
		_ = p.cfg.Telemetry.RecordEvent("pool_note_error", "warn", name, "", msg, nil)
	}
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

// Remove 从账号池中移除指定账号并持久化状态。
func (p *Pool) Remove(name string) (*Account, bool) {
	p.mu.Lock()
	var removed *Account
	next := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		if removed == nil && (a.Name == name || a.UID == name) {
			removed = a
			continue
		}
		next = append(next, a)
	}
	if removed != nil {
		p.accounts = next
	}
	p.mu.Unlock()
	if removed != nil {
		p.saveState()
		log.Printf("pool account removed name=%s uid=%s", removed.Name, removed.UID)
		return removed, true
	}
	return nil, false
}

// Disable 禁用账号（自动禁用：401/凭证失效等；刷新成功后自动解禁）。
func (p *Pool) Disable(name, reason string) bool {
	p.mu.Lock()
	found := false
	for _, a := range p.accounts {
		if a.Name == name || a.UID == name {
			a.mu.Lock()
			a.disabled = true
			a.disabledReason = reason
			a.lastErr = reason
			a.mu.Unlock()
			log.Printf("pool disable account=%s reason=%s", name, reason)
			found = true
		}
	}
	p.mu.Unlock()
	if found {
		p.saveState()
	}
	return found
}

// DisableManual 管理员手动禁用：与自动禁用区分，token 刷新成功不会解除，
// 只有 Enable 显式解禁（否则 watchdog/余额查询路径的 Validate 会把手动禁用
// 静默复活）。
func (p *Pool) DisableManual(name, reason string) bool {
	p.mu.Lock()
	found := false
	for _, a := range p.accounts {
		if a.Name == name || a.UID == name {
			a.mu.Lock()
			a.disabled = true
			a.manualDisabled = true
			a.disabledReason = reason
			a.lastErr = reason
			a.mu.Unlock()
			log.Printf("pool disable account=%s reason=%s (manual)", name, reason)
			found = true
		}
	}
	p.mu.Unlock()
	if found {
		p.saveState()
	}
	return found
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

// Enabled 报告账号是否启用；冷却期间仍需续期。
func (p *Pool) Enabled(name string) bool {
	a := p.Get(name)
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.disabled
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
	return p.AcquireLockWaitCtx(context.Background(), name, timeout)
}

// AcquireLockWaitCtx 同 AcquireLockWait，但同时响应 ctx 取消：客户端断开时
// 排队请求立即放弃，不再空占 goroutine 与等待时长（轮询重试路径原本就通过
// select r.Context().Done 处理取消，这里让首次排队也具备同一语义）。
func (p *Pool) AcquireLockWaitCtx(ctx context.Context, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		p.mu.Lock()
		acquired := false
		found := false
		for _, a := range p.accounts {
			if a.Name != name {
				continue
			}
			found = true
			a.mu.Lock()
			if a.activeConcurrent < a.maxConcurrent {
				a.activeConcurrent++
				acquired = true
			}
			a.mu.Unlock()
			break
		}
		p.mu.Unlock()
		if !found {
			return false
		}
		if acquired {
			return true
		}
		if timeout <= 0 || time.Now().After(deadline) {
			return false
		}
		// 避免忙轮询：每次 100ms 重试；ctx 取消立即放弃。
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return false
		}
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

// CheckAndRefreshTokenWithin 在剩余有效期低于 skew 时主动刷新 token。
//
// skew 来自 scheduler 的 refresh_skew（配置项 watch.refresh_skew_minutes）。
// 此前该配置从未生效：原实现硬编码 1 小时阈值，调度器传进来的 RefreshSkew
// 只出现在日志里，README 写的「提前刷新时间」实际不起作用。
func (p *Pool) CheckAndRefreshTokenWithin(name string, skew time.Duration) error {
	return p.CheckAndRefreshTokenWithinContext(context.Background(), name, skew)
}

// CheckAndRefreshTokenWithinContext 在给定上下文超时内按需提前刷新 token。
func (p *Pool) CheckAndRefreshTokenWithinContext(ctx context.Context, name string, skew time.Duration) error {
	if skew <= 0 {
		skew = time.Hour
	}
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

	authz := acct.AuthSnapshot()
	remaining := authz.Remaining()
	hasRefresh := authz.Refresh() != ""

	if remaining > skew {
		return nil
	}
	if !hasRefresh {
		// ticket 登录不返回 refresh_token，跳过无意义的周期刷新。
		return nil
	}
	log.Printf("pool proactive refresh account=%s remaining=%s skew=%s",
		name, remaining.Round(time.Minute), skew)
	return p.refreshToken(ctx, name, false, skew)
}

// RefreshToken 手动刷新指定账号的 token（强制刷新，不受剩余有效期跳过）。
func (p *Pool) RefreshToken(name string) error {
	return p.refreshToken(context.Background(), name, true, 0)
}

// RefreshTokenContext 在给定上下文超时/取消约束下强制刷新指定账号的 token。
func (p *Pool) RefreshTokenContext(ctx context.Context, name string) error {
	return p.refreshToken(ctx, name, true, 0)
}

// refreshToken 在 refreshMu 保护下执行刷新；非强制模式会在锁内复查剩余有效期，
// 避免并发 Validate / 主动刷新穿透后重复请求 STS。
func (p *Pool) refreshToken(ctx context.Context, name string, force bool, skew time.Duration) error {
	p.mu.Lock()
	loginCfg := p.cfg.LoginConfig
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

	authz := acct.AuthSnapshot()
	if !force && !authz.Expired() && (skew <= 0 || authz.Remaining() > skew) {
		acct.mu.Lock()
		acct.lastValidated = time.Now()
		acct.mu.Unlock()
		return nil
	}
	refreshToken := authz.Refresh()
	if refreshToken == "" {
		return fmt.Errorf("no refresh_token available")
	}
	dpopPrivateJWK := authz.DPoPPrivateJWK()
	if dpopPrivateJWK["d"] == "" {
		return fmt.Errorf("no DPoP private key available; re-login required")
	}

	loginCfg.ClientID = authz.ClientIDOr(loginCfg.ClientID)
	resp, err := acct.Client.RefreshToken(ctx, loginCfg, refreshToken, authz.Verifier(), upstream.DPoPPrivateJWK(dpopPrivateJWK))
	if err != nil {
		p.notifyEvent("token_refresh_failed", "error", name, err.Error())
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
	// 刷新成功只解除自动禁用（401/凭证失效的自愈路径）；管理员手动禁用
	// 不受影响，避免下一次保活/余额查询把手动下线的账号静默复活。
	if !acct.manualDisabled {
		acct.disabled = false
		acct.disabledReason = ""
		acct.lastErr = ""
	}
	acct.lastValidated = time.Now()
	acct.mu.Unlock()
	log.Printf("pool token refreshed manually account=%s", name)
	p.notifyEvent("token_refreshed", "info", name, "token refreshed")
	return nil
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
		Name               string         `json:"name"`
		ErrCount           int            `json:"err_count"`
		CoolUntil          time.Time      `json:"cool_until"`
		Disabled           bool           `json:"disabled"`
		ManualDisabled     bool           `json:"manual_disabled,omitempty"`
		Reason             string         `json:"reason"`
		LastErr            string         `json:"last_error"`
		CheckinAttemptedAt time.Time      `json:"checkin_attempted_at,omitempty"`
		Checkin            *CheckinStatus `json:"checkin,omitempty"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		log.Printf("pool state parse failed file=%s err=%v", p.state, err)
		return
	}
	for _, a := range p.accounts {
		for _, s := range data {
			if s.Name == a.Name {
				a.mu.Lock()
				a.errCount = s.ErrCount
				a.coolUntil = s.CoolUntil
				a.disabled = s.Disabled
				a.manualDisabled = s.ManualDisabled
				a.disabledReason = s.Reason
				a.lastErr = s.LastErr
				a.checkinAttemptedAt = s.CheckinAttemptedAt
				a.checkin = s.Checkin
				a.mu.Unlock()
			}
		}
	}
}

func (p *Pool) saveState() {
	if p.state == "" {
		return
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	p.mu.Lock()
	accounts := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	type entry struct {
		Name               string         `json:"name"`
		ErrCount           int            `json:"err_count"`
		CoolUntil          time.Time      `json:"cool_until"`
		Disabled           bool           `json:"disabled"`
		ManualDisabled     bool           `json:"manual_disabled,omitempty"`
		Reason             string         `json:"reason"`
		LastErr            string         `json:"last_error"`
		CheckinAttemptedAt time.Time      `json:"checkin_attempted_at,omitempty"`
		Checkin            *CheckinStatus `json:"checkin,omitempty"`
	}
	out := make([]entry, 0, len(accounts))
	for _, a := range accounts {
		a.mu.Lock()
		var checkin *CheckinStatus
		if a.checkin != nil {
			copy := *a.checkin
			checkin = &copy
		}
		out = append(out, entry{
			Name:               a.Name,
			ErrCount:           a.errCount,
			CoolUntil:          a.coolUntil,
			Disabled:           a.disabled,
			ManualDisabled:     a.manualDisabled,
			Reason:             a.disabledReason,
			LastErr:            a.lastErr,
			CheckinAttemptedAt: a.checkinAttemptedAt,
			Checkin:            checkin,
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

func (p *Pool) notifyEvent(kind, level, name, message string) {
	if p.cfg.Telemetry == nil {
		return
	}
	_ = p.cfg.Telemetry.RecordEvent(kind, level, name, "", message, nil)
}
