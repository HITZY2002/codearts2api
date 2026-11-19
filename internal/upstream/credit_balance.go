package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// CreditBalance is the account's current package credit snapshot.
type CreditBalance struct {
	PackageName       string  `json:"package_name"`
	PackageStatus     string  `json:"package_status"`
	TotalAmount       float64 `json:"total_amount"`
	UsedAmount        float64 `json:"used_amount"`
	RemainingAmount   float64 `json:"remaining_amount"`
	ExpiringAmount    float64 `json:"expiring_amount"`
	BasicRemaining    float64 `json:"basic_remaining"`
	BonusRemaining    float64 `json:"bonus_remaining"`
	OnDemandRemaining float64 `json:"on_demand_remaining"`
	StartDate         string  `json:"start_date"`
	EndDate           string  `json:"end_date"`
}

type packageCreditSnapshot struct {
	Amount     float64 `json:"package_credit_amount"`
	Used       float64 `json:"package_credit_used"`
	Remaining  float64 `json:"package_credit_remain"`
	Expiring   float64 `json:"expiring_credit_amount"`
	UserAmount float64 `json:"package_credit_user_amount"`
	UserUsed   float64 `json:"package_credit_user_used"`
	UserRemain float64 `json:"package_credit_user_remain"`
}

// FetchPackageOverview reads the same package overview used by the official client.
func (c *Client) FetchPackageOverview(ctx context.Context, cred SignCredential) (json.RawMessage, error) {
	requestURL := c.snapURL(EpPackageOverview)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, nil, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, &ApiError{
			Code: resp.StatusCode, Status: resp.StatusCode,
			Message: truncateStr(string(raw), 300), Path: EpPackageOverview,
		}
	}
	return json.RawMessage(raw), nil
}

// FetchCreditBalance parses the package overview into a credit snapshot.
func (c *Client) FetchCreditBalance(ctx context.Context, cred SignCredential) (CreditBalance, error) {
	raw, err := c.FetchPackageOverview(ctx, cred)
	if err != nil {
		return CreditBalance{}, err
	}
	var payload struct {
		All      packageCreditSnapshot `json:"all_credit_package"`
		Basic    packageCreditSnapshot `json:"basic_package"`
		OnDemand packageCreditSnapshot `json:"ondemand_credit_package"`
		Bonus    packageCreditSnapshot `json:"bonus_credit_package"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return CreditBalance{}, fmt.Errorf("parse package overview: %w", err)
	}

	out := CreditBalance{
		TotalAmount:       payload.All.Amount,
		UsedAmount:        payload.All.Used,
		RemainingAmount:   payload.All.Remaining,
		ExpiringAmount:    payload.All.Expiring,
		BasicRemaining:    payload.Basic.Remaining,
		BonusRemaining:    payload.Bonus.Remaining,
		OnDemandRemaining: payload.OnDemand.Remaining,
	}
	if out.TotalAmount == 0 && out.RemainingAmount == 0 {
		out.RemainingAmount = out.BasicRemaining + out.BonusRemaining + out.OnDemandRemaining
	}
	return out, nil
}
