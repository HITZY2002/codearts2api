// Package scheduler 提供 token 自动续期和活动积分自动领取。
//
// 增强功能：
// - 主动保活：定期发送轻量级请求保持会话活跃
// - 更积极的刷新策略：token 剩余少于 1 小时即主动刷新
package scheduler

import (
	"context"
	"log"
	"sync"
	"time"

	"codearts2api/internal/pool"
)

// Config 调度器配置。
type Config struct {
	Pool              *pool.Pool
	Enabled           bool
	CreditsAutoClaim  bool          // 自动领取每日签到/新手积分，与 Token 模型福利独立
	PollInterval      time.Duration // 默认 30m
	RefreshSkew       time.Duration // 到期前多久刷新，默认 30m
	KeepaliveInterval time.Duration // 保活心跳间隔，默认 15m
}

// Scheduler 定时任务。
type Scheduler struct {
	mu   sync.RWMutex
	cfg  Config
	wake chan struct{}
}

// New 构造调度器。
func New(cfg Config) *Scheduler {
	cfg = normalizeConfig(cfg)
	return &Scheduler{cfg: cfg, wake: make(chan struct{}, 1)}
}

func normalizeConfig(cfg Config) Config {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 30 * time.Minute
	}
	if cfg.KeepaliveInterval <= 0 {
		cfg.KeepaliveInterval = 15 * time.Minute // 15 分钟保活一次
	}
	return cfg
}

// UpdateConfig applies watchdog settings without restarting the process.
// 周期参数立即生效：Run 循环每次被唤醒都重新读取配置并重建定时器。
func (s *Scheduler) UpdateConfig(cfg Config) {
	cfg = normalizeConfig(cfg)
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) snapshot() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Run 启动定时循环。
// 单个循环按配置计算下一次 poll/keepalive 的到期时间；设置变更通过 wake
// 立即重算，Enabled 开关也在运行中生效（关闭时挂起，重新打开立即补一轮）。
func (s *Scheduler) Run(ctx context.Context) {
	var lastPoll, lastKeepalive time.Time
	started := false
	wasEnabled := false
	for {
		cfg := s.snapshot()
		if cfg.Enabled != wasEnabled {
			if cfg.Enabled {
				log.Printf("token watchdog enabled: poll=%s refresh_skew=%s keepalive=%s",
					cfg.PollInterval, cfg.RefreshSkew, cfg.KeepaliveInterval)
			} else {
				log.Printf("token watchdog disabled")
			}
			wasEnabled = cfg.Enabled
		}
		if !cfg.Enabled {
			if !s.wait(ctx, 15*time.Second) {
				return
			}
			continue
		}
		now := time.Now()
		if !started {
			// 立即执行一次
			s.Tick(ctx)
			lastPoll = time.Now()
			lastKeepalive = lastPoll
			started = true
			continue
		}
		nextPoll := lastPoll.Add(cfg.PollInterval)
		nextKeepalive := lastKeepalive.Add(cfg.KeepaliveInterval)
		if !now.Before(nextPoll) {
			s.Tick(ctx)
			lastPoll = time.Now()
			now = lastPoll
		}
		if !now.Before(nextKeepalive) {
			s.KeepaliveTick(ctx)
			lastKeepalive = time.Now()
			now = lastKeepalive
		}
		next := nextPoll
		if nextKeepalive.Before(next) {
			next = nextKeepalive
		}
		wait := time.Until(next)
		if wait <= 0 {
			continue
		}
		if !s.wait(ctx, wait) {
			return
		}
	}
}

// wait 等待定时到期或配置变更；ctx 取消返回 false。
func (s *Scheduler) wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-s.wake:
		return true
	case <-timer.C:
		return true
	}
}

// Tick 校验启用账号（自动续期 + 保活 + 可选活动领取）。
func (s *Scheduler) Tick(ctx context.Context) {
	cfg := s.snapshot()
	for _, acct := range cfg.Pool.Accounts() {
		if err := ctx.Err(); err != nil {
			return
		}
		if !cfg.Pool.Enabled(acct.Name) {
			continue
		}
		valCtx, valCancel := context.WithTimeout(ctx, 30*time.Second)
		ok, _ := cfg.Pool.ValidateContext(valCtx, acct)
		valCancel()
		if ok {
			authz := acct.AuthSnapshot()
			remaining := authz.Remaining().Round(time.Minute)
			log.Printf("token account=%s ok remaining=%s", acct.Name, remaining)

			// ticket 登录不返回 refresh_token；这种账号跳过无意义的周期刷新。
			if authz.Refresh() != "" {
				refreshCtx, refreshCancel := context.WithTimeout(ctx, 30*time.Second)
				err := cfg.Pool.CheckAndRefreshTokenWithinContext(refreshCtx, acct.Name, cfg.RefreshSkew)
				refreshCancel()
				if err != nil {
					log.Printf("proactive refresh failed account=%s err=%v", acct.Name, err)
				} else if remaining <= cfg.RefreshSkew {
					log.Printf("token refreshed account=%s new_remaining=%s", acct.Name,
						authz.Remaining().Round(time.Minute))
				}
			}
			if cfg.CreditsAutoClaim {
				claimCtx, cancel := context.WithTimeout(ctx, time.Minute)
				result, due, err := cfg.Pool.CheckinIfDue(claimCtx, acct.Name)
				cancel()
				if err != nil {
					log.Printf("credit checkin failed account=%s err=%v", acct.Name, err)
				} else if due {
					log.Printf("credit checkin account=%s ok=%t wrote=%t message=%s",
						acct.Name, result.OK, result.Wrote, result.Message)
				}
			}
		} else {
			log.Printf("token account=%s invalid/disabled", acct.Name)
		}
	}
}

// KeepaliveTick performs a real read-only upstream request for idle accounts.
func (s *Scheduler) KeepaliveTick(ctx context.Context) {
	cfg := s.snapshot()
	for _, acct := range cfg.Pool.Accounts() {
		if err := ctx.Err(); err != nil {
			return
		}
		if !cfg.Pool.Enabled(acct.Name) || !cfg.Pool.NeedKeepalive(acct.Name) {
			continue
		}
		keepaliveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := cfg.Pool.Keepalive(keepaliveCtx, acct.Name)
		cancel()
		if err != nil {
			log.Printf("keepalive failed account=%s err=%v", acct.Name, err)
		} else {
			log.Printf("keepalive account=%s ok", acct.Name)
		}
	}
}
