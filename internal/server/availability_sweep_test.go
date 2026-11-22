package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// 周期 sweep 必须与流量学习的异步探测同走 runProbes：探测之间保留 probeGap
// 释放窗口。此前 sweep 是背靠背的裸循环——待探测组合多时连续占满上游会话
// 槽位（释放实测 >15s），紧随其后的真实请求被上游判为 TM.00001041 排队。
// 通过注入缩短 probeGap，用耗时断言 sweep 确实经过了释放窗口。
func TestSweepAvailabilityUsesProbeGap(t *testing.T) {
	oldGap := probeGap
	probeGap = 120 * time.Millisecond
	t.Cleanup(func() { probeGap = oldGap })

	// 重置全局 sweep 节流与该账号的可用性残留，确保两个模型都处于待探测状态。
	availability.Lock()
	availability.lastSweep = time.Time{}
	delete(availability.byKey, availKey("u-sweep", "m-s1"))
	delete(availability.byKey, availKey("u-sweep", "m-s2"))
	availability.Unlock()

	var hits atomic.Int64
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer chatSrv.Close()

	upstream.SetAccountModels("u-sweep", []upstream.ModelInfo{{ID: "m-s1"}, {ID: "m-s2"}})
	a := &auth.Auth{UserID: "u-sweep", UserName: "u-sweep", AccessKeyID: "AK", SecretAccessKey: "SK", CloudDragonTok: "ST"}
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, chatSrv.URL)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(5 * time.Second)})

	start := time.Now()
	h.sweepAvailability()
	elapsed := time.Since(start)

	if got := hits.Load(); got != 2 {
		t.Fatalf("两个待探测模型都应被探测，got=%d", got)
	}
	if elapsed < 120*time.Millisecond {
		t.Fatalf("sweep 应经过 runProbes 的释放窗口（>=120ms），实测 %s——探测背靠背了", elapsed)
	}
	for _, model := range []string{"m-s1", "m-s2"} {
		if st, _ := modelAvailability("u-sweep", model); st != availUsable {
			t.Fatalf("探测成功后 %s 应记为可用", model)
		}
	}
}

// sweep 的节流必须生效：半个探测间隔内重复调用不重复探测。
func TestSweepAvailabilityThrottled(t *testing.T) {
	availability.Lock()
	availability.lastSweep = time.Now() // 刚 sweep 过
	availability.Unlock()

	var hits atomic.Int64
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer chatSrv.Close()

	upstream.SetAccountModels("u-sweep-throttle", []upstream.ModelInfo{{ID: "m-t1"}})
	a := &auth.Auth{UserID: "u-sweep-throttle", UserName: "u-sweep-throttle", AccessKeyID: "AK", SecretAccessKey: "SK", CloudDragonTok: "ST"}
	p, err := pool.New([]*auth.Auth{a}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, chatSrv.URL)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(5 * time.Second)})

	h.sweepAvailability()
	h.sweepAvailability()
	if got := hits.Load(); got != 0 {
		t.Fatalf("节流窗口内不应触发探测，got=%d", got)
	}
}
