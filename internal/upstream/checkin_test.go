package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCheckinTransitionsAndEligibility(t *testing.T) {
	tests := []struct {
		name, status string
		claimable    bool
		end          string
		start        string
		wantOK       bool
		wantWrote    bool
		wantWrites   []string
		wantCredits  float64
	}{
		{name: "fresh", status: "ELIGIBLE", claimable: true, wantOK: true, wantWrote: true, wantWrites: []string{EpCreditClaim, EpCreditConfirm}, wantCredits: 150},
		{name: "unconfirmed", status: "CLAIMED", wantOK: true, wantWrote: true, wantWrites: []string{EpCreditConfirm}, wantCredits: 150},
		{name: "confirmed", status: "CONFIRMED", wantOK: true},
		{name: "consumed", status: "CONSUMED", wantOK: true},
		{name: "not eligible", wantOK: true},
		{name: "not yet claimable", status: "ELIGIBLE"},
		{name: "unknown status", status: "CHANGED"},
		{name: "expired", status: "ELIGIBLE", claimable: true, end: "2020-01-01T00:00:00Z", wantOK: true},
		{name: "not started", status: "ELIGIBLE", claimable: true, start: "2100-01-01T00:00:00+08:00", wantOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var writes []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Agent-Type") != "PromptCenter" || r.Header.Get("X-Security-Token") != "token" || !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 ") {
					t.Error("missing signed PromptCenter credentials")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					if r.URL.Path != EpCreditDelivery || r.URL.Query().Get("channel") != "IDE" {
						t.Errorf("wrong delivery URL: %s", r.URL)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": []any{
						map[string]any{"campaignId": 34, "title": "每日签到领150积分", "status": tc.status, "claimable": tc.claimable, "benefitUnit": "CREDIT", "benefitAmount": 150, "pendingTotalAmount": 150, "extra": map[string]string{"endTime": tc.end, "startTime": tc.start}},
						map[string]any{"campaignId": 2, "claimable": true, "status": "ELIGIBLE"},
						map[string]any{"campaignId": 3, "claimable": true, "status": "ELIGIBLE"},
					}}})
					return
				}
				writes = append(writes, r.URL.Path)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["campaignId"] != float64(34) {
					t.Errorf("unexpected campaign: %v", body)
				}
				if r.URL.Path == EpCreditClaim {
					key, _ := body["idempotentKey"].(string)
					if body["channel"] != "IDE" || !strings.HasPrefix(key, "claim_34_") {
						t.Errorf("wrong claim body: %v", body)
					}
				}
				_, _ = fmt.Fprint(w, `{"code":0,"message":"ok"}`)
			}))
			defer srv.Close()
			c := New(5 * time.Second)
			c.snapBase = srv.URL
			res, err := c.Checkin(context.Background(), SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "token"})
			if err != nil || res.OK != tc.wantOK || res.Wrote != tc.wantWrote || res.Credits != tc.wantCredits || !reflect.DeepEqual(writes, tc.wantWrites) {
				t.Fatalf("result=%+v err=%v writes=%v, want ok=%t wrote=%t credits=%g writes=%v", res, err, writes, tc.wantOK, tc.wantWrote, tc.wantCredits, tc.wantWrites)
			}
		})
	}
}

func TestCheckinRecognizesCurrentDailyCampaignAndSkipsCompleted(t *testing.T) {
	writes := 0
	status := "CONFIRMED"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"items":[{"campaignId":34,"title":"每日签到领150积分","status":%q,"claimable":false,"benefitUnit":"CREDIT"}]}}`, status)
			return
		}
		writes++
		_, _ = fmt.Fprint(w, `{"code":0}`)
	}))
	defer server.Close()

	client := New(5 * time.Second)
	client.snapBase = server.URL
	result, err := client.Checkin(context.Background(), SignCredential{})
	if err != nil || !result.OK || result.Wrote || !result.Skipped || result.Message != "今日已签到" {
		t.Fatalf("completed daily campaign result=%+v err=%v", result, err)
	}
	if writes != 0 {
		t.Fatalf("completed campaign issued %d writes", writes)
	}
}

func TestCheckinRecoversConfirmationAndDailyReset(t *testing.T) {
	status := "ELIGIBLE"
	claimCount, confirmCount := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCreditDelivery:
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"items":[{"campaignId":34,"title":"每日签到领150积分","status":%q,"claimable":true,"benefitAmount":150}]}}`, status)
		case EpCreditClaim:
			claimCount++
			status = "CLAIMED"
			_, _ = fmt.Fprint(w, `{"code":0}`)
		case EpCreditConfirm:
			confirmCount++
			if confirmCount == 1 {
				// HTTP 200 的业务失败必须重试，不能当作确认成功。
				_, _ = fmt.Fprint(w, `{"code":40001,"message":"try again"}`)
				return
			}
			status = "CONFIRMED"
			_, _ = fmt.Fprint(w, `{"code":0}`)
		}
	}))
	defer srv.Close()
	c := New(5 * time.Second)
	c.snapBase = srv.URL
	for i, wantOK := range []bool{false, true, true} {
		res, err := c.Checkin(context.Background(), SignCredential{})
		if err != nil || res.OK != wantOK {
			t.Fatalf("attempt %d result=%+v err=%v", i, res, err)
		}
	}
	if claimCount != 1 || confirmCount != 2 {
		t.Fatalf("duplicate or missing write: claim=%d confirm=%d", claimCount, confirmCount)
	}
	// 上游在次日重新变为 ELIGIBLE；本地不得永久缓存「已签」。
	status = "ELIGIBLE"
	res, err := c.Checkin(context.Background(), SignCredential{})
	if err != nil || !res.OK || res.Credits != 150 || claimCount != 2 || confirmCount != 3 {
		t.Fatalf("daily reset not handled: result=%+v err=%v claim=%d confirm=%d", res, err, claimCount, confirmCount)
	}
}

func TestCheckinDailyCampaignAndMalformedResponses(t *testing.T) {
	tests := []struct {
		name, delivery, claim string
		wantErr, wantOK       bool
		wantConfirm           int
		wantCredits           float64
	}{
		{name: "daily campaign", claim: `{"code":0}`, wantOK: true, wantConfirm: 1, wantCredits: 150},
		{name: "claim business failure", claim: `{"code":40001,"message":"not ready"}`},
		{name: "claim missing code", claim: `{"message":"ok"}`},
		{name: "empty campaigns", delivery: `{"code":0,"data":{"items":[]}}`, wantOK: true},
		{name: "missing items", delivery: `{"code":0,"data":{}}`, wantErr: true},
		{name: "missing business code", delivery: `{"data":{"items":[]}}`, wantErr: true},
		{name: "delivery business failure", delivery: `{"code":40001,"message":"not ready"}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			confirms := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case EpCreditDelivery:
					body := tc.delivery
					if body == "" {
						body = `{"code":0,"data":{"items":[{"campaignId":34,"title":"每日签到领150积分","status":"ELIGIBLE","claimable":true,"benefitAmount":150}]}}`
					}
					_, _ = fmt.Fprint(w, body)
				case EpCreditClaim:
					var body struct {
						ID int `json:"campaignId"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body.ID != 34 {
						t.Errorf("claimed other campaign: %d", body.ID)
					}
					_, _ = fmt.Fprint(w, tc.claim)
				case EpCreditConfirm:
					confirms++
					_, _ = fmt.Fprint(w, `{"code":0}`)
				}
			}))
			defer srv.Close()
			c := New(5 * time.Second)
			c.snapBase = srv.URL
			res, err := c.Checkin(context.Background(), SignCredential{})
			if (err != nil) != tc.wantErr || res.OK != tc.wantOK || confirms != tc.wantConfirm || res.Credits != tc.wantCredits {
				t.Fatalf("result=%+v err=%v confirms=%d", res, err, confirms)
			}
		})
	}
}
