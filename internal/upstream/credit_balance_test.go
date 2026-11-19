package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchCreditBalance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpPackageOverview {
			t.Fatalf("path=%s, want %s", r.URL.Path, EpPackageOverview)
		}
		_, _ = fmt.Fprint(w, `{
			"all_credit_package": {
				"package_credit_amount": 28970.99,
				"package_credit_used": 426.78,
				"package_credit_remain": 28544.21,
				"expiring_credit_amount": 12.5
			},
			"basic_package": {"package_credit_remain": 1870.99},
			"ondemand_credit_package": {"package_credit_remain": 0},
			"bonus_credit_package": {"package_credit_remain": 26673.22}
		}`)
	}))
	defer server.Close()

	client := New(5 * time.Second)
	client.snapBase = server.URL
	balance, err := client.FetchCreditBalance(context.Background(), SignCredential{})
	if err != nil {
		t.Fatal(err)
	}
	if balance.RemainingAmount != 28544.21 || balance.UsedAmount != 426.78 ||
		balance.BasicRemaining != 1870.99 || balance.BonusRemaining != 26673.22 ||
		balance.ExpiringAmount != 12.5 {
		t.Fatalf("unexpected balance: %+v", balance)
	}
}
