// credit 工具：查询全部账号登录态（token 剩余有效期/状态）。
//
// 本工具只查询登录态；活动积分由服务的 watchdog 或面板签到按钮领取。
// 用法：credit [-auth-dir ./auths] [-json] [-uid <uid>]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
)

func main() {
	authDir := flag.String("auth-dir", "./auths", "auth dir")
	jsonOut := flag.Bool("json", false, "raw JSON output")
	uid := flag.String("uid", "", "only this account")
	flag.Parse()

	auths, err := auth.LoadDir(*authDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	if len(auths) == 0 {
		log.Fatalf("no accounts in %s", *authDir)
	}

	p, err := pool.New(auths, pool.Config{
		ErrThreshold: 3,
		ErrCooldown:  10 * time.Minute,
		SoftCooldown: 60 * time.Second,
		RefreshSkew:  30 * time.Minute,
	}, "")
	if err != nil {
		log.Fatalf("build pool: %v", err)
	}

	type row struct {
		UserID           string `json:"user_id"`
		UserName         string `json:"user_name"`
		ExpiresAt        string `json:"expires_at"`
		Remaining        string `json:"remaining"`
		Expired          bool   `json:"expired"`
		HasRefresh       bool   `json:"has_refresh_token"`
		ActiveConcurrent int    `json:"active_concurrent"`
		Cooling          bool   `json:"cooling"`
		Disabled         bool   `json:"disabled"`
	}
	var rows []row
	// pool.List() 返回脱敏状态快照（map），避免直接访问私有字段。
	listMap := map[string]map[string]any{}
	for _, m := range p.List() {
		listMap[m["name"].(string)] = m
	}
	for _, acct := range p.Accounts() {
		if *uid != "" && acct.Name != *uid {
			continue
		}
		st := listMap[acct.Name]
		activeConc, _ := st["active_concurrent"].(int)
		rows = append(rows, row{
			UserID:           acct.Name,
			UserName:         acct.UserName,
			ExpiresAt:        acct.Auth.ExpiresAt().Format(time.RFC3339),
			Remaining:        acct.Auth.Remaining().Round(time.Minute).String(),
			Expired:          acct.Auth.Expired(),
			HasRefresh:       acct.Auth.Refresh() != "",
			ActiveConcurrent: activeConc,
			Cooling:          st["cooling"].(bool),
			Disabled:         st["disabled"].(bool),
		})
	}
	if *jsonOut {
		raw, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(raw))
		return
	}
	for _, r := range rows {
		status := "ok"
		if r.Disabled {
			status = "DISABLED"
		} else if r.Cooling {
			status = "COOLING"
		} else if r.Expired {
			status = "EXPIRED"
		}
		refresh := "no"
		if r.HasRefresh {
			refresh = "yes"
		}
		conc := fmt.Sprintf("%d", r.ActiveConcurrent)
		fmt.Printf("%s (%s): %s expires=%s remaining=%s refresh=%s concurrent=%s\n",
			r.UserID, r.UserName, status, r.ExpiresAt, r.Remaining, refresh, conc)
	}
}
