package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

// adminOverview 面板总览。
func (h *Handler) adminOverview(w http.ResponseWriter, r *http.Request) {
	total, healthy, disabled, cooling, credits := h.cfg.Pool.Stats()
	models := h.modelList()
	ids := make([]map[string]any, 0, len(models))
	for _, m := range models {
		ids = append(ids, map[string]any{"id": m["id"]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "codearts2api",
		"region":  "cn",
		"stats": map[string]any{
			"total":    total,
			"healthy":  healthy,
			"disabled": disabled,
			"cooling":  cooling,
			"credits":  credits,
		},
		"accounts": h.cfg.Pool.List(),
		"models":   ids,
		"schedule": map[string]any{
			"watch": h.watchInfoSnapshot(),
		},
	})
}

type uidBody struct {
	UID       string `json:"uid"`
	Account   string `json:"account"`
	Reason    string `json:"reason"`
	BatchSize int    `json:"batch_size"`
}

func readUIDBody(r *http.Request) (uidBody, error) {
	var b uidBody
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return b, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return b, nil
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, err
	}
	if b.UID == "" {
		b.UID = b.Account
	}
	return b, nil
}

type actionResult struct {
	UID       string                       `json:"uid"`
	OK        bool                         `json:"ok"`
	Skipped   bool                         `json:"skipped,omitempty"`
	Wrote     bool                         `json:"wrote,omitempty"`
	Credits   float64                      `json:"credits,omitempty"`
	Message   string                       `json:"message,omitempty"`
	Campaigns []upstream.CreditClaimResult `json:"campaigns,omitempty"`
}

type creditBalanceResult struct {
	UID         string `json:"uid"`
	AccountName string `json:"account_name,omitempty"`
	OK          bool   `json:"ok"`
	Message     string `json:"message,omitempty"`
	*upstream.CreditBalance
}

// adminCreditBalances returns current package credit balances for one or all accounts.
func (h *Handler) adminCreditBalances(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	targets := make([]string, 0)
	if uid != "" {
		if h.cfg.Pool.Get(uid) != nil {
			targets = append(targets, uid)
		}
	} else {
		for _, account := range h.cfg.Pool.List() {
			if accountUID, ok := account["uid"].(string); ok && accountUID != "" {
				targets = append(targets, accountUID)
			}
		}
	}

	results := make([]creditBalanceResult, 0, len(targets))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for _, accountUID := range targets {
		accountUID := accountUID
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			account := h.cfg.Pool.Get(accountUID)
			result := creditBalanceResult{UID: accountUID}
			if account == nil {
				result.Message = "no account"
			} else {
				result.AccountName = account.UserName
				ok, err := h.cfg.Pool.Validate(account)
				switch {
				case err != nil:
					result.Message = err.Error()
				case !ok:
					result.Message = "token invalid"
				default:
					ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
					authz := account.AuthSnapshot()
					token, ak, sk := authz.Credentials()
					balance, fetchErr := account.Client.FetchCreditBalance(ctx, upstream.SignCredential{
						SecurityToken: token, AccessKeyID: ak, SecretAccessKey: sk,
					})
					cancel()
					if fetchErr != nil {
						result.Message = fetchErr.Error()
					} else {
						result.OK = true
						result.CreditBalance = &balance
					}
				}
			}
			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].UID < results[j].UID })

	total := upstream.CreditBalance{}
	allOK := true
	for _, result := range results {
		if !result.OK || result.CreditBalance == nil {
			allOK = false
			continue
		}
		total.TotalAmount += result.TotalAmount
		total.UsedAmount += result.UsedAmount
		total.RemainingAmount += result.RemainingAmount
		total.ExpiringAmount += result.ExpiringAmount
		total.BasicRemaining += result.BasicRemaining
		total.BonusRemaining += result.BonusRemaining
		total.OnDemandRemaining += result.OnDemandRemaining
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": allOK, "total": total, "results": results,
		"message": summaryCreditBalance(total, results),
	})
}

func summaryCreditBalance(total upstream.CreditBalance, results []creditBalanceResult) string {
	success := 0
	for _, result := range results {
		if result.OK {
			success++
		}
	}
	return fmt.Sprintf("已查询 %d/%d 个账号，总剩余积分 %.2f", success, len(results), total.RemainingAmount)
}

// adminCredits 保留旧路由，校验/刷新登录态；不把它当作真实积分查询。
func (h *Handler) adminCredits(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "bad json: " + err.Error()})
		return
	}
	targets := h.pickTargets(body.UID)
	results := make([]actionResult, 0, len(targets))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for _, uid := range targets {
		uid := uid
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			acct := h.cfg.Pool.Get(uid)
			res := actionResult{UID: uid}
			if acct == nil {
				res.Message = "no account"
			} else if ok, err := h.cfg.Pool.Validate(acct); err != nil {
				res.Message = err.Error()
			} else if !ok {
				res.Message = "token invalid"
			} else {
				res.OK = true
				res.Message = "ok"
			}
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": allOK(results), "message": summaryMsg("刷新状态", results), "results": results,
	})
}

// adminCheckin 领取指定账号或全部启用账号的签到/新手活动积分。
func (h *Handler) adminCheckin(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "bad json: " + err.Error()})
		return
	}
	if body.UID != "" && h.cfg.Pool.Get(body.UID) == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found"})
		return
	}
	targets := h.pickTargets(body.UID)
	results := make([]actionResult, len(targets))
	batchSize := body.BatchSize
	if batchSize <= 0 {
		batchSize = 3
	}
	if batchSize > 10 {
		batchSize = 10
	}
	for start := 0; start < len(targets); start += batchSize {
		end := start + batchSize
		if end > len(targets) {
			end = len(targets)
		}
		var wg sync.WaitGroup
		for index := start; index < end; index++ {
			index, uid := index, targets[index]
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
				result, err := h.cfg.Pool.Checkin(ctx, uid)
				cancel()
				res := actionResult{
					UID: uid, OK: result.OK, Skipped: result.Skipped,
					Wrote: result.Wrote, Credits: result.Credits,
					Message: result.Message, Campaigns: result.Campaigns,
				}
				if err != nil {
					res.OK, res.Message = false, err.Error()
				}
				results[index] = res
			}()
		}
		wg.Wait()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": allOK(results), "message": summaryMsg("签到/新手领取", results),
		"results": results, "batch_size": batchSize,
	})
}

// adminCheckinStatuses reads check-in campaign state without claiming anything.
func (h *Handler) adminCheckinStatuses(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	targets := make([]string, 0)
	if uid != "" {
		if h.cfg.Pool.Get(uid) != nil {
			targets = append(targets, uid)
		}
	} else {
		for _, account := range h.cfg.Pool.List() {
			if accountUID, ok := account["uid"].(string); ok && accountUID != "" {
				targets = append(targets, accountUID)
			}
		}
	}
	batchSize := intParam(r, "batch_size", 3)
	if batchSize <= 0 {
		batchSize = 3
	}
	if batchSize > 10 {
		batchSize = 10
	}
	results := make([]actionResult, len(targets))
	for start := 0; start < len(targets); start += batchSize {
		end := start + batchSize
		if end > len(targets) {
			end = len(targets)
		}
		var wg sync.WaitGroup
		for index := start; index < end; index++ {
			index, accountUID := index, targets[index]
			wg.Add(1)
			go func() {
				defer wg.Done()
				result := actionResult{UID: accountUID}
				account := h.cfg.Pool.Get(accountUID)
				if account == nil {
					result.Message = "no account"
					results[index] = result
					return
				}
				ok, err := h.cfg.Pool.Validate(account)
				if err != nil {
					result.Message = err.Error()
					results[index] = result
					return
				}
				if !ok {
					result.Message = "token invalid"
					results[index] = result
					return
				}
				ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
				authz := account.AuthSnapshot()
				token, ak, sk := authz.Credentials()
				campaigns, err := account.Client.FetchCreditCampaigns(ctx, upstream.SignCredential{
					SecurityToken: token, AccessKeyID: ak, SecretAccessKey: sk,
				})
				cancel()
				if err != nil {
					result.Message = err.Error()
					results[index] = result
					return
				}
				result.OK = true
				result.Skipped = true
				foundCampaign := false
				for _, campaign := range campaigns {
					if !upstream.IsCheckinCampaign(campaign) {
						continue
					}
					foundCampaign = true
					if campaign.Status == "ELIGIBLE" || campaign.Status == "CLAIMED" {
						result.Skipped = false
					}
					result.Campaigns = append(result.Campaigns, upstream.CreditClaimResult{
						CampaignID: campaign.ID,
						Title:      campaign.Title,
						OK:         true,
						Status:     campaign.Status,
						Message:    checkinCampaignStatusMessage(campaign.Status),
					})
				}
				if !foundCampaign {
					result.Message = "未找到签到活动"
				} else if result.Skipped {
					result.Message = "今日已签到"
				} else {
					result.Message = "存在待签到活动"
				}
				results[index] = result
			}()
		}
		wg.Wait()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": allOK(results), "results": results, "batch_size": batchSize,
		"message": summaryMsg("签到状态查询", results),
	})
}

func checkinCampaignStatusMessage(status string) string {
	switch status {
	case "CONFIRMED", "CONSUMED":
		return "已完成"
	case "CLAIMED":
		return "已领取，待确认"
	case "ELIGIBLE":
		return "可领取"
	default:
		return "未参与"
	}
}

// adminKeepalive 刷新即将过期的 token。
func (h *Handler) adminKeepalive(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "bad json: " + err.Error()})
		return
	}
	targets := h.pickTargets(body.UID)
	results := make([]actionResult, 0, len(targets))
	for _, uid := range targets {
		acct := h.cfg.Pool.Get(uid)
		res := actionResult{UID: uid}
		if acct == nil {
			res.Message = "no account"
			results = append(results, res)
			continue
		}
		// Validate 内部会在临近过期时自动 Refresh
		ok, verr := h.cfg.Pool.Validate(acct)
		if verr != nil {
			res.Message = verr.Error()
		} else if !ok {
			res.Message = "token invalid"
		} else {
			res.OK = true
			res.Message = "refreshed/validated"
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": allOK(results), "message": summaryMsg("保活", results), "results": results,
	})
}

func (h *Handler) adminReload(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AuthDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "auth_dir 未配置"})
		return
	}
	auths, err := auth.LoadDir(h.cfg.AuthDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	h.cfg.Pool.SyncToDir(auths)
	total, healthy, _, _, _ := h.cfg.Pool.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "已重载 auths", "loaded": len(auths), "total": total, "healthy": healthy,
	})
}

func (h *Handler) adminEnable(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	if !h.cfg.Pool.Enable(body.UID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已启用 " + body.UID})
}

func (h *Handler) adminDisable(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	reason := body.Reason
	if reason == "" {
		reason = "manual disable"
	}
	if !h.cfg.Pool.Disable(body.UID, reason) {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已禁用 " + body.UID})
}

func (h *Handler) adminClearCooldown(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	if !h.cfg.Pool.ClearCooldown(body.UID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已清冷却 " + body.UID})
}

func (h *Handler) pickTargets(uid string) []string {
	if uid != "" {
		if h.cfg.Pool.Get(uid) == nil {
			return nil
		}
		return []string{uid}
	}
	out := make([]string, 0)
	for _, st := range h.cfg.Pool.List() {
		if disabled, _ := st["disabled"].(bool); disabled {
			continue
		}
		if u, ok := st["uid"].(string); ok && u != "" {
			out = append(out, u)
		}
	}
	return out
}

func allOK(results []actionResult) bool {
	if len(results) == 0 {
		return true
	}
	for _, r := range results {
		if !r.OK {
			return false
		}
	}
	return true
}

func summaryMsg(action string, results []actionResult) string {
	if len(results) == 0 {
		return action + ": 无目标账号"
	}
	ok, fail := 0, 0
	for _, r := range results {
		if r.OK {
			ok++
		} else {
			fail++
		}
	}
	if fail == 0 {
		return action + "完成: " + itoa(ok) + " 成功"
	}
	return action + "完成: " + itoa(ok) + " 成功 / " + itoa(fail) + " 失败"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
