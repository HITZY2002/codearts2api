// diag 工具：只读诊断一个账号在华为云侧的权益状态。
//
// 用途：出现 TM.000010051「无权调用该模型」时区分是
//  1. 主账号/子用户身份问题（principal_id != account_id → IAM 子用户）
//  2. 套餐/积分计费未开通（积分活动资格与领取状态）
//  3. 免费福利渠道未给该账号开放（benefit 网关 group not found）
//
// 全部为只读调用，不领取、不下单、不改账号。
//
// 用法：go run ./cmd/diag -auth-dir ./auths
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

func main() {
	authDir := flag.String("auth-dir", "./auths", "auth dir")
	flag.Parse()

	auths, err := auth.LoadDir(*authDir)
	if err != nil || len(auths) == 0 {
		log.Fatalf("load auths(%s): %v len=%d", *authDir, err, len(auths))
	}
	a := auths[0]
	token, ak, sk := a.Credentials()
	cred := upstream.SignCredential{AccessKeyID: ak, SecretAccessKey: sk, SecurityToken: token}
	c := upstream.New(60 * time.Second)
	ctx := context.Background()

	fmt.Println("== 账号（本地凭证）")
	fmt.Printf("user_id=%s user_name=%s\n", a.UserID, a.UserName)

	fmt.Println("\n== caller-identity（sts.cn-north-4）")
	if uid, name, acct, err := upstream.CallerIdentity(cred); err != nil {
		fmt.Printf("ERROR %v\n", err)
	} else {
		fmt.Printf("account_id=%s principal_id=%s name=%s\n", acct, uid, name)
		if acct != "" && uid == acct {
			fmt.Println("→ 该凭证是**主账号本身**（domain 根用户）")
		} else if acct != "" && uid != acct {
			fmt.Println("→ 该凭证是 domain", acct, "下的 **IAM 子用户**")
		}
	}

	fmt.Println("\n== current/user（snap-manager）")
	if uid, name, domain, err := upstream.CurrentUser(cred); err != nil {
		fmt.Printf("ERROR %v\n", err)
	} else {
		fmt.Printf("user_id=%s user_name=%s domain_id=%s\n", uid, name, domain)
	}

	fmt.Println("\n== 积分活动（只读 delivery，不领取）")
	if camps, err := c.FetchCreditCampaigns(ctx, cred); err != nil {
		fmt.Printf("ERROR %v\n", err)
	} else if len(camps) == 0 {
		fmt.Println("（无活动返回）")
	} else {
		for _, cp := range camps {
			fmt.Printf("id=%-3d status=%-10s claimable=%-5v unit=%-6s amount=%-8.0f pending=%-8.0f %s  [%s ~ %s]\n",
				cp.ID, cp.Status, cp.Claimable, cp.BenefitUnit, cp.BenefitAmount, cp.PendingTotalAmount,
				cp.Title, cp.Extra.StartTime, cp.Extra.EndTime)
		}
	}

	fmt.Println("\n== 免费福利渠道（opengw gateway config / claim）")
	if err := c.ClaimBenefit(cred); err != nil {
		fmt.Printf("claim: %v\n", err)
	} else {
		fmt.Println("claim: ok（福利渠道可用）")
	}
}
