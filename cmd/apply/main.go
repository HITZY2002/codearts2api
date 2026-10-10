// apply 工具：批量刷新即将过期的 token。
// 每日签到/新手积分由服务的 watchdog 或面板签到按钮领取，与续期独立。
// 用法：apply [-auth-dir ./auths] [-force]
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
)

func main() {
	authDir := flag.String("auth-dir", "./auths", "auth dir")
	force := flag.Bool("force", false, "refresh even if token still valid")
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

	refreshed := 0
	for _, acct := range p.Accounts() {
		need := *force || acct.Auth.ExpiringSoon(30*time.Minute) || acct.Auth.Expired()
		if !need {
			fmt.Printf("%s (%s): token ok, remaining=%s, skip\n", 
				acct.Name, acct.UserName, acct.Auth.Remaining().Round(time.Minute))
			continue
		}
		if acct.Auth.Refresh() == "" {
			fmt.Printf("%s (%s): no refresh_token, need re-login\n", acct.Name, acct.UserName)
			continue
		}
		
		// 使用 pool 的 RefreshToken 方法
		if err := p.RefreshToken(acct.Name); err != nil {
			fmt.Printf("%s (%s): refresh error: %v\n", acct.Name, acct.UserName, err)
			continue
		}
		fmt.Printf("%s (%s): refreshed, expires=%s\n", 
			acct.Name, acct.UserName, acct.Auth.ExpiresAt().Format(time.RFC3339))
		refreshed++
	}
	
	fmt.Printf("\ndone, refreshed=%d/%d\n", refreshed, len(auths))
	
	// 退出码：如果全部成功则 0，否则 1
	if refreshed < len(auths) {
		os.Exit(1)
	}
}
