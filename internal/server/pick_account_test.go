package server

import (
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// 客户端只调 /v1/chat/completions（没调过 /v1/models）时目录为空。
// pickAccount 必须先补齐目录再挑号，否则 MaxRotate 用尽也轮不到有能力的账号。
func TestPickAccountDiscoversCatalogLazily(t *testing.T) {
	var as []*auth.Auth
	for _, n := range []string{"a", "b", "c", "d"} {
		as = append(as, &auth.Auth{UserID: n, UserName: n})
	}
	p, err := pool.New(as, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(time.Second)})

	// 模拟「惰性发现后」d 账号的目录里有该模型
	upstream.SetAccountModels("d", []upstream.ModelInfo{{ID: "brand-new-benefit-0915", Benefit: true}})

	got := h.pickAccount(map[string]bool{}, "brand-new-benefit-0915")
	if got == nil || got.Name != "d" {
		t.Fatalf("应挑到目录里有该模型的账号 d，got=%v", got)
	}
}

// 目录未知时乐观放行，不凭缺失目录拒请求。
func TestAccountCanServeOptimisticWhenUnknown(t *testing.T) {
	p, err := pool.New([]*auth.Auth{{UserID: "x", UserName: "x"}}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(time.Second)})
	if !h.accountCanServe(p.Get("x"), "whatever-model") {
		t.Error("目录未知应乐观放行")
	}
}
