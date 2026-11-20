package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// EpCreditStatistics 积分统计（总量/已用/剩余/即将过期，按池拆分）。
// 与活动接口同在 snap 根路径，但走 snap-manager；AK/SK 签名 + Agent-Type: PromptCenter。
const EpCreditStatistics = "/snap-manager/v1/statistics/plugin"

// CreditBucket 是一类积分（基础套餐 / 活动赠送 / 按需购买）的额度明细。
type CreditBucket struct {
	Total    float64 `json:"total"`
	Used     float64 `json:"used"`
	Remain   float64 `json:"remain"`
	Expiring float64 `json:"expiring"`
}

// CreditStat 是某个账号的积分快照，全部字段来自上游 statistics/plugin，不做估算。
type CreditStat struct {
	Total    float64      `json:"total"`
	Used     float64      `json:"used"`
	Remain   float64      `json:"remain"`
	Expiring float64      `json:"expiring"`
	Basic    CreditBucket `json:"basic"`
	Bonus    CreditBucket `json:"bonus"`
	OnDemand CreditBucket `json:"on_demand"`
	// TokensUsed 取上游 usageTokenChatMessages.usage_token_num（token 口径）。
	TokensUsed int64  `json:"tokens_used"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	FetchedAt  int64  `json:"fetched_at"`
	Err        string `json:"err,omitempty"`
}

// CreditStatistics 读取账号积分统计；第二个返回值是上游原始 JSON（诊断用）。
func (c *Client) CreditStatistics(ctx context.Context, cred SignCredential) (*CreditStat, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.snapURL(EpCreditStatistics), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	signRequest(req, nil, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Path: EpCreditStatistics,
			Message: truncateStr(string(body), 300)}
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse credit statistics: %w", err)
	}
	st := ParseCreditStat(doc)
	st.FetchedAt = time.Now().Unix()
	return st, doc, nil
}

// ParseCreditStat 解析 statistics/plugin：口径在 metrics[] 里，按 name 取。
func ParseCreditStat(doc map[string]any) *CreditStat {
	st := &CreditStat{}
	if v, ok := doc["end_date"].(string); ok {
		st.ExpiresAt = v
	}
	metrics, _ := doc["metrics"].([]any)
	for _, raw := range metrics {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		b := CreditBucket{
			Total:    numOr0(m, "package_credit_amount"),
			Used:     numOr0(m, "package_credit_used"),
			Remain:   numOr0(m, "package_credit_remain"),
			Expiring: numOr0(m, "package_credit_expiring_amount"),
		}
		switch name {
		case "usageTotalPackageCredit":
			st.Total, st.Used, st.Remain, st.Expiring = b.Total, b.Used, b.Remain, b.Expiring
		case "usageBasicPackageCredit":
			st.Basic = b
		case "usageBonusPackageCredit":
			st.Bonus = b
		case "usageOnDemandPackageCredit":
			st.OnDemand = b
		case "usageTokenChatMessages":
			st.TokensUsed = int64(numOr0(m, "usage_token_num"))
		}
	}
	return st
}

func numOr0(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case json.Number:
		f, _ := v.Float64()
		return f
	case string:
		var f float64
		fmt.Sscanf(v, "%f", &f)
		return f
	}
	return 0
}
