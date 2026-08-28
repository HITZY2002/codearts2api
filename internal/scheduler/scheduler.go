// Package scheduler 提供 token 自动续期看门狗。
//
// CodeArts Agent 没有每日签到/申请额度接口（免费额度按月重置，见
// codearts.huaweicloud.com/portal/settings/personal-usage）。本调度器
// 周期性校验全部账号，临近过期的 token 自动走 oauth2 refresh_token 续期。
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

// Tick 校验全部账号（自动续期 + 保活）。
func (s *Scheduler) Tick(ctx context.Context) {
	for _, acct := range s.cfg.Pool.Accounts() {
		ok, _ := s.cfg.Pool.Validate(acct)
		if ok {
			remaining := acct.Auth.Remaining().Round(time.Minute)
			log.Printf("token account=%s ok remaining=%s", acct.Name, remaining)

			// ticket 登录不返回 refresh_token；这种账号跳过无意义的周期刷新。
			if acct.Auth.Refresh() != "" {
				if err := s.cfg.Pool.CheckAndRefreshToken(acct.Name); err != nil {
					log.Printf("proactive refresh failed account=%s err=%v", acct.Name, err)
				} else if remaining <= time.Hour {
					log.Printf("token refreshed account=%s new_remaining=%s", acct.Name,
						acct.Auth.Remaining().Round(time.Minute))
				}
			}
		} else {
			log.Printf("token account=%s invalid/disabled", acct.Name)
		}
	}
}
