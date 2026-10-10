// Package scheduler 提供 token 自动续期和活动积分自动领取。
//
// 增强功能：
// - 主动保活：定期发送轻量级请求保持会话活跃
// - 更积极的刷新策略：token 剩余少于 1 小时即主动刷新
package scheduler

import (
	"context"
	"log"
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
	cfg Config
}

// New 构造调度器。
func New(cfg Config) *Scheduler {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 30 * time.Minute
	}
	if cfg.KeepaliveInterval <= 0 {
		cfg.KeepaliveInterval = 15 * time.Minute // 15 分钟保活一次
	}
	return &Scheduler{cfg: cfg}
}

// Run 启动定时循环。
func (s *Scheduler) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		log.Printf("token watchdog disabled")
		return
	}
	log.Printf("token watchdog enabled: poll=%s refresh_skew=%s keepalive=%s",
		s.cfg.PollInterval, s.cfg.RefreshSkew, s.cfg.KeepaliveInterval)

	// 立即执行一次
	s.Tick(ctx)

	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick 校验启用账号（自动续期 + 保活 + 可选活动领取）。
func (s *Scheduler) Tick(ctx context.Context) {
	for _, acct := range s.cfg.Pool.Accounts() {
		if err := ctx.Err(); err != nil {
			return
		}
		if !s.cfg.Pool.Enabled(acct.Name) {
			continue
		}
		ok, _ := s.cfg.Pool.Validate(acct)
		if ok {
			remaining := acct.Auth.Remaining().Round(time.Minute)
			log.Printf("token account=%s ok remaining=%s", acct.Name, remaining)

			// ticket 登录不返回 refresh_token；这种账号跳过无意义的周期刷新。
			if acct.Auth.Refresh() != "" {
				if err := s.cfg.Pool.CheckAndRefreshTokenWithin(acct.Name, s.cfg.RefreshSkew); err != nil {
					log.Printf("proactive refresh failed account=%s err=%v", acct.Name, err)
				} else if remaining <= s.cfg.RefreshSkew {
					log.Printf("token refreshed account=%s new_remaining=%s", acct.Name,
						acct.Auth.Remaining().Round(time.Minute))
				}
			}
			if s.cfg.CreditsAutoClaim {
				claimCtx, cancel := context.WithTimeout(ctx, time.Minute)
				result, err := s.cfg.Pool.Checkin(claimCtx, acct.Name)
				cancel()
				if err != nil {
					log.Printf("credit checkin failed account=%s err=%v", acct.Name, err)
				} else {
					log.Printf("credit checkin account=%s ok=%t message=%s", acct.Name, result.OK, result.Message)
				}
			}
		} else {
			log.Printf("token account=%s invalid/disabled", acct.Name)
		}
	}
}
