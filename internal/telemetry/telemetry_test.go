package telemetry

import (
	"fmt"
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
		CreatedAt:               time.Now().Add(-48 * time.Hour).UnixMilli(),
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

	// retentionDays=0 must not wipe rows.
	if _, err := store.Purge(0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, total, err = store.ListRequests(RequestFilter{Limit: 10}); err != nil || total != 1 {
		t.Fatalf("Purge(0,0,0) unexpectedly deleted rows: total=%d err=%v", total, err)
	}

	if _, err := store.Purge(1, 1, 128<<20); err != nil {
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

func TestStoreCloseDrainsQueue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "drain.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const n = 20
	now := time.Now().UnixMilli()
	for i := 0; i < n; i++ {
		if err := store.RecordRequest(RequestLog{
			RequestID:  fmt.Sprintf("req-drain-%d", i),
			CreatedAt:  now + int64(i),
			Endpoint:   "/v1/chat/completions",
			Method:     "POST",
			StatusCode: 200,
			Model:      "glm-5.2",
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	// Close without prior Flush must still drain all queued rows.
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	_, total, err := reopened.ListRequests(RequestFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != n {
		t.Fatalf("Close dropped queued writes: got %d rows, want %d", total, n)
	}
}
