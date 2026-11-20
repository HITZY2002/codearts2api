package pool

import (
	"context"
	"fmt"
	"time"

	"codearts2api/internal/upstream"
)

// CheckinClient 允许注入独立的活动客户端；默认使用各账号的上游客户端。
type CheckinClient interface {
	Checkin(context.Context, upstream.SignCredential) (upstream.CheckinResult, error)
}

// KeepaliveClient performs a read-only signed request for idle accounts.
type KeepaliveClient interface {
	FetchCreditCampaigns(context.Context, upstream.SignCredential) ([]upstream.CreditCampaign, error)
}

type CheckinStatus struct {
	OK        bool      `json:"ok"`
	Message   string    `json:"message"`
	CheckedAt time.Time `json:"checked_at"`
}

// Checkin 串行领取指定账号的每日签到和新手福利。活动故障只记录签到结果，
// 不计入推理错误、冷却或禁用；token 仍由原有 Validate/RefreshToken 处理。
func (p *Pool) Checkin(ctx context.Context, name string) (upstream.CheckinResult, error) {
	a := p.Get(name)
	if a == nil {
		return upstream.CheckinResult{}, fmt.Errorf("account not found")
	}
	a.checkinMu.Lock()
	defer a.checkinMu.Unlock()
	if err := ctx.Err(); err != nil {
		return upstream.CheckinResult{}, err
	}
	a.mu.Lock()
	disabled := a.disabled
	a.mu.Unlock()
	if disabled {
		return upstream.CheckinResult{}, fmt.Errorf("account disabled")
	}
	ok, err := p.Validate(a)
	if err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("token invalid; re-login required")
		}
		return upstream.CheckinResult{}, err
	}
	authz := a.AuthSnapshot()
	token, ak, sk := authz.Credentials()
	client := p.cfg.CheckinClient
	if client == nil {
		client = a.Client
	}
	result, err := client.Checkin(ctx, upstream.SignCredential{SecurityToken: token, AccessKeyID: ak, SecretAccessKey: sk})
	if err != nil {
		result.OK, result.Message = false, err.Error()
	}
	attemptedAt := time.Now()
	a.mu.Lock()
	if err == nil {
		a.checkinAttemptedAt = attemptedAt
	}
	if result.Wrote || (err != nil && !result.OK) {
		a.checkin = &CheckinStatus{OK: result.OK, Message: result.Message, CheckedAt: attemptedAt}
	}
	a.mu.Unlock()
	if err == nil {
		p.saveState()
	}
	if p.cfg.Telemetry != nil && (err != nil || !result.OK || result.Wrote) {
		level := "info"
		if err != nil || !result.OK {
			level = "error"
		}
		_ = p.cfg.Telemetry.RecordEvent("credit_checkin", level, name, "", result.Message, nil)
	}
	return result, err
}

// CheckinIfDue runs the automatic check-in at most once per local calendar day.
func (p *Pool) CheckinIfDue(ctx context.Context, name string) (upstream.CheckinResult, bool, error) {
	account := p.Get(name)
	if account == nil {
		return upstream.CheckinResult{}, false, fmt.Errorf("account not found")
	}
	now := time.Now()
	account.mu.Lock()
	attempted := account.checkinAttemptedAt
	account.mu.Unlock()
	if !attempted.IsZero() && sameLocalDate(attempted, now) {
		return upstream.CheckinResult{
			OK: true, Skipped: true, Message: "今日已检查签到",
		}, false, nil
	}
	result, err := p.Checkin(ctx, name)
	return result, true, err
}

// DailyCheckinStatus reports whether the account completed today's 150-credit check-in.
func (p *Pool) DailyCheckinStatus(ctx context.Context, name string) (upstream.DailyCheckinStatus, error) {
	account := p.Get(name)
	if account == nil {
		return upstream.DailyCheckinStatus{}, fmt.Errorf("account not found")
	}
	ok, err := p.Validate(account)
	if err != nil {
		return upstream.DailyCheckinStatus{}, err
	}
	if !ok {
		return upstream.DailyCheckinStatus{}, fmt.Errorf("token invalid; re-login required")
	}
	authz := account.AuthSnapshot()
	token, ak, sk := authz.Credentials()
	return account.Client.FetchDailyCheckinStatus(ctx, upstream.SignCredential{
		SecurityToken: token, AccessKeyID: ak, SecretAccessKey: sk,
	})
}

// Keepalive verifies an idle account with a read-only upstream request. The
// local heartbeat timestamp advances only after the upstream request succeeds.
func (p *Pool) Keepalive(ctx context.Context, name string) error {
	account := p.Get(name)
	if account == nil {
		return fmt.Errorf("account not found")
	}
	ok, err := p.Validate(account)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("token invalid; re-login required")
	}
	authz := account.AuthSnapshot()
	token, ak, sk := authz.Credentials()
	client := p.cfg.KeepaliveClient
	if client == nil {
		client = account.Client
	}
	if _, err := client.FetchCreditCampaigns(ctx, upstream.SignCredential{
		SecurityToken: token, AccessKeyID: ak, SecretAccessKey: sk,
	}); err != nil {
		return err
	}
	p.PingKeepalive(name)
	return nil
}

func sameLocalDate(a, b time.Time) bool {
	ay, am, ad := a.In(time.Local).Date()
	by, bm, bd := b.In(time.Local).Date()
	return ay == by && am == bm && ad == bd
}
