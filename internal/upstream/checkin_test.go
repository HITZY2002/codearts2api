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
		wantWrites   []string
		wantCredits  float64
	}{
		{name: "fresh", status: "ELIGIBLE", claimable: true, wantOK: true, wantWrites: []string{EpCreditClaim, EpCreditConfirm}, wantCredits: 1000},
		{name: "unconfirmed", status: "CLAIMED", wantOK: true, wantWrites: []string{EpCreditConfirm}},
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
						map[string]any{"campaignId": 1, "title": "daily", "status": tc.status, "claimable": tc.claimable, "benefitUnit": "CREDIT", "benefitAmount": 1000, "pendingTotalAmount": 1000, "extra": map[string]string{"endTime": tc.end, "startTime": tc.start}},
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
				if body["campaignId"] != float64(1) {
					t.Errorf("unexpected campaign: %v", body)
				}
				if r.URL.Path == EpCreditClaim {
					key, _ := body["idempotentKey"].(string)
					if body["channel"] != "IDE" || !strings.HasPrefix(key, "claim_1_") {
						t.Errorf("wrong claim body: %v", body)
					}
				}
				_, _ = fmt.Fprint(w, `{"code":0,"message":"ok"}`)
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase = srv.URL
			res, err := c.Checkin(context.Background(), SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "token"})
			if err != nil || res.OK != tc.wantOK || res.Credits != tc.wantCredits || !reflect.DeepEqual(writes, tc.wantWrites) {
				t.Fatalf("result=%+v err=%v writes=%v, want ok=%t credits=%g writes=%v", res, err, writes, tc.wantOK, tc.wantCredits, tc.wantWrites)
			}
		})
	}
}

func TestCheckinRecoversConfirmationAndDailyReset(t *testing.T) {
	status := "ELIGIBLE"
	claimCount, confirmCount := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCreditDelivery:
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"items":[{"campaignId":1,"status":%q,"claimable":true,"benefitAmount":1000}]}}`, status)
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
	c := New(time.Second)
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
	if err != nil || !res.OK || res.Credits != 1000 || claimCount != 2 || confirmCount != 3 {
		t.Fatalf("daily reset not handled: result=%+v err=%v claim=%d confirm=%d", res, err, claimCount, confirmCount)
	}
}

func TestCheckinNewUserAndMalformedResponses(t *testing.T) {
	tests := []struct {
		name, delivery, claim string
		wantErr, wantOK       bool
		wantConfirm           int
		wantCredits           float64
	}{
		{name: "new user", claim: `{"code":0}`, wantOK: true, wantConfirm: 1, wantCredits: 4000},
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
						body = `{"code":0,"data":{"items":[{"campaignId":4,"title":"new user","status":"ELIGIBLE","claimable":true,"benefitAmount":4000}]}}`
					}
					_, _ = fmt.Fprint(w, body)
				case EpCreditClaim:
					var body struct {
						ID int `json:"campaignId"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body.ID != 4 {
						t.Errorf("claimed other campaign: %d", body.ID)
					}
					_, _ = fmt.Fprint(w, tc.claim)
				case EpCreditConfirm:
					confirms++
					_, _ = fmt.Fprint(w, `{"code":0}`)
				}
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase = srv.URL
			res, err := c.Checkin(context.Background(), SignCredential{})
			if (err != nil) != tc.wantErr || res.OK != tc.wantOK || confirms != tc.wantConfirm || res.Credits != tc.wantCredits {
				t.Fatalf("result=%+v err=%v confirms=%d", res, err, confirms)
			}
		})
	}
}
