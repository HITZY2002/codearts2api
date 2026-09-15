package server

import (
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// 混合账号池：/v1/models 展示的是并集，聊天必须先挑目录里真有该模型的账号。
// 否则没这个模型的账号会先吃一次 400，白耗一次 MaxRotate（只有 3 次），
// 还会给健康账号记错误、累计到阈值冷却 10 分钟。
func TestPickAccountPrefersAccountWithModel(t *testing.T) {
	p, err := pool.New([]*auth.Auth{
		{UserID: "acct-a", UserName: "a"},
		{UserID: "acct-b", UserName: "b"},
		{UserID: "acct-c", UserName: "c"},
		{UserID: "acct-d", UserName: "d"}, // 未发现目录
	}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(time.Second)})

	upstream.SetAccountModels("acct-a", []upstream.ModelInfo{{ID: "GLM-5.2"}})
	upstream.SetAccountModels("acct-b", []upstream.ModelInfo{{ID: "GLM-5.2"}})
	upstream.SetAccountModels("acct-c", []upstream.ModelInfo{
		{ID: "GLM-5.2"}, {ID: "deepseek-v4-pro-0813", Benefit: true},
	})

	got := h.pickAccount(map[string]bool{}, "deepseek-v4-pro-0813")
	if got == nil || got.Name != "acct-c" {
		t.Fatalf("应挑目录里有该模型的账号，got=%v", got)
	}
	// 有能力小写别名也要命中。
	got = h.pickAccount(map[string]bool{}, "glm-5.2")
	if got == nil || got.Name != "acct-a" {
		t.Fatalf("小写别名应命中第一个有能力账号，got=%v", got)
	}
	// 有能力但已被 tried 排除 → 退回普通轮转，不能返回同一账号。
	got = h.pickAccount(map[string]bool{"acct-c": true}, "deepseek-v4-pro-0813")
	if got == nil || got.Name == "acct-c" {
		t.Fatalf("被排除的账号不应再被选中，got=%v", got)
	}
	// 谁都没有这个模型 → 退回普通轮转（目录只是参考，不能凭它拒请求）。
	if got = h.pickAccount(map[string]bool{}, "unknown-model-xyz"); got == nil {
		t.Fatal("应退回普通轮转而不是拒绝请求")
	}

	// 粘性账号不能服务该模型时必须换号。
	if h.accountCanServe(p.Get("acct-a"), "deepseek-v4-pro-0813") {
		t.Error("acct-a 目录里没有该福利模型")
	}
	if !h.accountCanServe(p.Get("acct-c"), "DEEPSEEK-V4-PRO-0813") {
		t.Error("有该模型的账号应通过能力检查（大小写不敏感）")
	}
	// 目录未知的账号乐观放行，交给上游判定；nil 也不能崩。
	if !h.accountCanServe(p.Get("acct-d"), "deepseek-v4-pro-0813") {
		t.Error("目录未知时应乐观放行")
	}
	if !h.accountCanServe(nil, "deepseek-v4-pro-0813") {
		t.Error("账号不存在时应乐观放行")
	}
}

func TestModelEntriesAliasesStaticAndBenefit(t *testing.T) {
	entries := modelEntries([]upstream.ModelInfo{
		{ID: "GLM-5.2", ContextWindow: 202752, MaxTokens: 8192},
		{ID: "deepseek-v4-pro-0813", ContextWindow: 1048576, Benefit: true},
	})
	byID := map[string]map[string]any{}
	for _, e := range entries {
		id, _ := e["id"].(string)
		if byID[id] != nil {
			t.Fatalf("/v1/models 出现重复 ID: %s", id)
		}
		byID[id] = e
	}

	// 精确 ID 与小写别名都要在（老客户端习惯用小写）。
	for _, want := range []string{"GLM-5.2", "glm-5.2", "deepseek-v4-pro-0813"} {
		if byID[want] == nil {
			t.Errorf("缺少模型条目 %q，got=%v", want, keysOf(byID))
		}
	}
	if byID["GLM-5.2"]["context_length"] != int64(202752) {
		t.Errorf("context_length 应取自上游目录，got=%v", byID["GLM-5.2"])
	}
	if byID["GLM-5.2"]["max_output_tokens"] != int64(8192) {
		t.Errorf("max_output_tokens 应取自上游目录，got=%v", byID["GLM-5.2"])
	}
	if byID["deepseek-v4-pro-0813"]["benefit"] != true {
		t.Error("福利模型应在 /v1/models 标记 benefit")
	}

	// 动态目录缺失的模型由静态表补位（精确 ID + 小写别名），福利标记跟随静态表。
	for _, want := range []string{"Qwen3-VL-235B", "qwen3-vl-235b", "glm-5.3-flash", "deepseek-v4-flash-0731"} {
		if byID[want] == nil {
			t.Errorf("静态兜底缺少 %q", want)
		}
	}
	if byID["qwen3-vl-235b"]["benefit"] != nil {
		t.Error("内置模型不应标记 benefit")
	}
	if byID["glm-5.3-flash"]["benefit"] != true {
		t.Error("静态兜底里的福利模型应保留 benefit 标记")
	}

	// 动态目录整体失败（无账号可用）时仍返回完整静态表 + 别名。
	fallback := modelEntries(nil)
	if len(fallback) < len(staticModels) {
		t.Fatalf("静态兜底条目过少: %d", len(fallback))
	}
	seen := map[string]bool{}
	for _, e := range fallback {
		id, _ := e["id"].(string)
		if seen[id] {
			t.Fatalf("静态兜底出现重复 ID: %s", id)
		}
		seen[id] = true
	}
	if !seen["GLM-5.2"] || !seen["glm-5.2"] {
		t.Errorf("静态兜底也要给大写 ID 补小写别名，got=%v", keysOf(seen))
	}
}

func keysOf[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
