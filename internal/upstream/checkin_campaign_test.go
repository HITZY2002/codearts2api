package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 官方活动金额改版（150 → 每日 1000 积分）后标题不再包含 "150"：
// 签到匹配不得锁金额，否则活动改版后签到静默失效（"无可领取的签到或新手活动"）。
func TestCheckinMatchesDailyCampaignRegardlessOfAmount(t *testing.T) {
	claims, confirms := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCreditDelivery:
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"items":[`+
				`{"campaignId":77,"title":"每日签到领1000积分","status":"ELIGIBLE","claimable":true,`+
				`"benefitUnit":"CREDIT","benefitAmount":1000,"pendingTotalAmount":1000}]}}`)
		case EpCreditClaim:
			claims++
			_, _ = fmt.Fprint(w, `{"code":0}`)
		case EpCreditConfirm:
			confirms++
			_, _ = fmt.Fprint(w, `{"code":0}`)
		}
	}))
	defer srv.Close()

	c := New(5 * time.Second)
	c.snapBase = srv.URL
	res, err := c.Checkin(context.Background(), SignCredential{})
	if err != nil || !res.OK || !res.Wrote || res.Credits != 1000 || claims != 1 || confirms != 1 {
		t.Fatalf("改版后的每日签到活动应被领取：result=%+v err=%v claims=%d confirms=%d", res, err, claims, confirms)
	}
}

// 无关活动不得误匹配：非签到活动不应触发领取。
func TestCheckinSkipsUnrelatedCampaigns(t *testing.T) {
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCreditDelivery:
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"items":[`+
				`{"campaignId":88,"title":"限时福利活动","status":"ELIGIBLE","claimable":true,`+
				`"benefitUnit":"CREDIT","benefitAmount":500}]}}`)
		default:
			writes++
			_, _ = fmt.Fprint(w, `{"code":0}`)
		}
	}))
	defer srv.Close()

	c := New(5 * time.Second)
	c.snapBase = srv.URL
	res, err := c.Checkin(context.Background(), SignCredential{})
	if err != nil {
		t.Fatalf("无关活动不应报错: %v", err)
	}
	if !res.Skipped || res.Message != "无可领取的签到或新手活动" {
		t.Fatalf("无关活动应被跳过，result=%+v", res)
	}
	if writes != 0 {
		t.Fatalf("无关活动不应产生领取写入，writes=%d", writes)
	}
}
