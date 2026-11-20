package pool

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"codearts2api/internal/upstream"
)

// CheckinClient 允许注入独立的活动客户端；默认使用各账号的上游客户端。
type CheckinClient interface {
	Checkin(context.Context, upstream.SignCredential) (upstream.CheckinResult, error)
}

type CheckinStatus struct {
	OK        bool      `json:"ok"`
	Message   string    `json:"message"`
	Credits   float64   `json:"credits"`   // 本次实际领取到的积分
	CheckedAt time.Time `json:"checked_at"`
}

var creditsInMessage = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*积分`)

// creditsFromMessage 从上游文案里取积分数（"新用户注册送 4000 积分: 已领取"）。
func creditsFromMessage(msg string) float64 {
	m := creditsInMessage.FindStringSubmatch(msg)
	if len(m) < 2 {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
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
	token, ak, sk := a.Auth.Credentials()
	client := p.cfg.CheckinClient
	if client == nil {
		client = a.Client
	}
	result, err := client.Checkin(ctx, upstream.SignCredential{SecurityToken: token, AccessKeyID: ak, SecretAccessKey: sk})
	if err != nil {
		result.OK, result.Message = false, err.Error()
	}
	a.mu.Lock()
	if result.OK {
		// 上游对「今天已领过」只回文案（例如「新用户注册送 4000 积分: 已领取」），
		// 这里补一次解析，并按自然日去重，避免重复点击把同一天的积分累加多次。
		if result.Credits == 0 {
			result.Credits = creditsFromMessage(result.Message)
		}
		if result.Credits > 0 {
			if day := time.Now().Format("2006-01-02"); a.creditsDay != day {
				a.credits += result.Credits
				a.creditsDay = day
			}
		}
	}
	a.checkin = &CheckinStatus{OK: result.OK, Message: result.Message, Credits: result.Credits, CheckedAt: time.Now()}
	a.mu.Unlock()
	return result, err
}
