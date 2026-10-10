package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

type adminCheckinClient struct{ calls int }

func (c *adminCheckinClient) Checkin(context.Context, upstream.SignCredential) (upstream.CheckinResult, error) {
	c.calls++
	return upstream.CheckinResult{OK: true, Credits: 1000, Message: "领取成功（1000 积分）"}, nil
}

func TestAdminCheckinClaimsInsteadOfKeepalive(t *testing.T) {
	c := &adminCheckinClient{}
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{CheckinClient: c}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, APIKey: "test-key"})
	for _, tc := range []struct {
		body, key     string
		status, calls int
	}{
		{`{}`, "", http.StatusUnauthorized, 0},
		{`{"uid":"missing"}`, "test-key", http.StatusNotFound, 0},
		{`{"uid":"u1"}`, "test-key", http.StatusOK, 1},
		{`{}`, "test-key", http.StatusOK, 2},
	} {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/checkin", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+tc.key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.status || c.calls != tc.calls {
			t.Fatalf("status=%d calls=%d body=%s", rec.Code, c.calls, rec.Body.String())
		}
		if rec.Code == http.StatusOK {
			var response struct {
				OK      bool           `json:"ok"`
				Results []actionResult `json:"results"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || !response.OK || len(response.Results) != 1 || response.Results[0].Credits != 1000 {
				t.Fatalf("invalid claim response: %s", rec.Body.String())
			}
		}
	}
}
