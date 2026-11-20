package telemetry

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRequestLogLifecycle(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "observability.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cost := 0.16
	firstOutput := int64(573)
	if err := store.RecordRequest(RequestLog{
		RequestID:               "req-1",
		CreatedAt:               time.Now().Add(-time.Hour).UnixMilli(),
		Endpoint:                "/v1/chat/completions",
		Method:                  "POST",
		ClientIP:                "127.0.0.1",
		StatusCode:              200,
		Stream:                  true,
		Model:                   "GLM-5.2",
		AccountUID:              "acct-1",
		AccountName:             "acct-1",
		Attempts:                2,
		QueueWaitMs:             12,
		FirstOutputMs:           &firstOutput,
		TotalMs:                 987,
		PromptTokens:            10,
		CompletionTokens:        20,
		TotalTokens:             30,
		UsageEstimated:          true,
		CreditCost:              &cost,
		BillingPromptTokens:     30,
		BillingCompletionTokens: 7,
		BillingCachedTokens:     29,
		BillingTotalTokens:      37,
		BillingRequestCount:     2,
		FinishReason:            "stop",
		ErrorType:               "",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}

	items, total, err := store.ListRequests(RequestFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("total=%d items=%d", total, len(items))
	}
	item := items[0]
	if item.CreditCost == nil || *item.CreditCost != cost || item.FirstOutputMs == nil || *item.FirstOutputMs != firstOutput {
		t.Fatalf("unexpected row: %#v", item)
	}
	if item.BillingRequestCount != 2 || item.BillingTotalTokens != 37 {
		t.Fatalf("billing fields lost: %#v", item)
	}

	overview, err := store.MetricsOverview(0, time.Now().UnixMilli()+1)
	if err != nil {
		t.Fatal(err)
	}
	if overview.TotalRequests != 1 || overview.TotalTokens != 30 || overview.TTFTP50Ms != firstOutput {
		t.Fatalf("overview=%#v", overview)
	}

	if _, err := store.Purge(0, 1, 128<<20); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	_, total, err = store.ListRequests(RequestFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("purge left %d rows", total)
	}
}
