package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"codearts2api/internal/auth"
)

func TestCanonicalModelKnownIndex(t *testing.T) {
	SetAccountModels("t-canon", []ModelInfo{{ID: "Qwen3.9-Test-VL", ContextWindow: 4096}})
	tests := []struct{ in, want string }{
		{"Qwen3.9-Test-VL", "Qwen3.9-Test-VL"},           // 精确命中
		{"qwen3.9-test-vl", "Qwen3.9-Test-VL"},           // 小写别名归一
		{"glm-5.2", "GLM-5.2"},                           // 种子 + 静态别名
		{"snap-chat", "GLM-5.2"},                         // 旧版别名
		{"deepseek-v4-pro-0813", "deepseek-v4-pro-0813"}, // 福利种子（全小写）
		{"unknown-model-xyz", "unknown-model-xyz"},       // 未知原样透传
	}
	for _, tc := range tests {
		if got := CanonicalModel(tc.in); got != tc.want {
			t.Errorf("CanonicalModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBenefitIsPerAccount(t *testing.T) {
	SetAccountModels("acct-a", []ModelInfo{
		{ID: "custom-benefit-0717", Benefit: true},
		{ID: "GLM-5.2", ContextWindow: 202752},
	})
	SetAccountModels("acct-b", []ModelInfo{{ID: "GLM-5.2", ContextWindow: 202752}})

	if !IsBenefitModel("acct-a", "CUSTOM-BENEFIT-0717") {
		t.Error("账号 A 的福利模型应走福利路由（大小写不敏感）")
	}
	if IsBenefitModel("acct-b", "custom-benefit-0717") {
		t.Error("账号 B 未授予该福利模型，不得跟着账号 A 走福利路由")
	}
	if IsBenefitModel("acct-b", "glm-5.2") {
		t.Error("内置模型不应走福利路由")
	}
	// 目录未知的账号：回退冷启动种子（宁可带头，避免未注册模型 404）。
	if !IsBenefitModel("acct-unknown", "glm-5.3-flash") {
		t.Error("目录未发现时应回退种子判定福利模型")
	}
	if IsBenefitModel("acct-unknown", "glm-5.2") {
		t.Error("种子之外的模型不得误判为福利")
	}
}

func TestSetAccountModelsReplacesCatalog(t *testing.T) {
	SetAccountModels("acct-rot", []ModelInfo{{ID: "rotated-benefit-0813", Benefit: true}})
	if !IsBenefitModel("acct-rot", "rotated-benefit-0813") {
		t.Fatal("首次写入后应为福利模型")
	}
	// 套餐轮换：该模型不再是福利模型 —— 目录必须整体替换，不能残留旧标记。
	SetAccountModels("acct-rot", []ModelInfo{{ID: "rotated-benefit-0813", ContextWindow: 1024}})
	if IsBenefitModel("acct-rot", "rotated-benefit-0813") {
		t.Error("轮换后不得残留福利标记")
	}

	if _, ok := AccountModels("acct-rot"); !ok {
		t.Error("目录应可读回")
	}
	if !AccountCatalogStale("acct-never-seen") {
		t.Error("未发现的账号目录应视为过期")
	}
	if AccountCatalogStale("acct-rot") {
		t.Error("刚写入的目录不应过期")
	}
}

// 福利模型转正（或套餐变化）后，发现结果必须压过冷启动种子，否则会一直带错头。
func TestDiscoveredCatalogOverridesSeed(t *testing.T) {
	// glm-5.3-flash 在种子里是福利模型，但该账号发现到的是内置模型。
	SetAccountModels("acct-promoted", []ModelInfo{
		{ID: "glm-5.3-flash", ContextWindow: 131072},
		{ID: "GLM-5.2", ContextWindow: 202752},
	})
	if IsBenefitModel("acct-promoted", "GLM-5.3-flash") {
		t.Error("发现结果说它是内置模型时不得再走福利路由")
	}
	if IsBenefitModel("acct-promoted", "GLM-5.2") {
		t.Error("内置模型不应走福利路由")
	}
	// 目录里查不到的种子模型（福利来源失败/未领取）仍要带头，否则上游按未注册模型拒。
	if !IsBenefitModel("acct-promoted", "deepseek-v4-pro-0813") {
		t.Error("目录里没有的种子模型应回退种子判定")
	}
	// 目录明确标了福利 → 优先级最高。
	SetAccountModels("acct-promoted2", []ModelInfo{{ID: "glm-5.3-flash", Benefit: true}})
	if !IsBenefitModel("acct-promoted2", "GLM-5.3-FLASH") {
		t.Error("目录标了福利就应走福利路由（大小写不敏感）")
	}
}

func TestMergeModelsBenefitAndDeterminism(t *testing.T) {
	agent := []ModelInfo{{ID: "deepseek-v4-pro-0813", ContextWindow: 1048576}}
	builtin := []ModelInfo{
		{ID: "GLM-5.2", ContextWindow: 202752},
		{ID: "deepseek-v4-pro-0813"},
	}
	benefit := []ModelInfo{{ID: "deepseek-v4-pro-0813", Benefit: true, MaxTokens: 8192}}

	got := MergeModels(agent, builtin, benefit)
	if len(got) != 2 {
		t.Fatalf("合并后应去重为 2 条，got=%+v", got)
	}
	if got[0].ID != "deepseek-v4-pro-0813" || got[1].ID != "GLM-5.2" {
		t.Fatalf("输出顺序应按小写 ID 稳定排序，got=%+v", got)
	}
	// 福利标记取或（少标一次就是一次 404），参数取更全的一条。
	if !got[0].Benefit {
		t.Error("三路合并后福利标记不得丢失")
	}
	if got[0].ContextWindow != 1048576 || got[0].MaxTokens != 8192 {
		t.Errorf("应保留最全的参数，got=%+v", got[0])
	}
	if got[1].Benefit {
		t.Error("内置模型不应带福利标记")
	}
	// 结果稳定：重复合并顺序一致。
	again := MergeModels(benefit, builtin, agent)
	for i := range got {
		if got[i].ID != again[i].ID {
			t.Fatalf("合并结果不稳定：%v vs %v", got, again)
		}
	}
}

func TestSendChatV2BenefitHeaderSigned(t *testing.T) {
	var mu sync.Mutex
	var header http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		header = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {}\n\n")
	}))
	defer srv.Close()

	c := New(5 * time.Second)
	c.snapBase = srv.URL
	cred := SignCredential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "ST"}
	body := map[string]any{"model": CanonicalModel("glm-5.3-flash"), "stream": true}

	rc, err := c.SendChatV2(context.Background(), body, "trace-1", cred, "tok", true)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	mu.Lock()
	got := header.Clone()
	mu.Unlock()
	if got.Get(HeaderMaasType) != MaasBenefit {
		t.Fatalf("福利模型必须带 %s: %s，headers=%v", HeaderMaasType, MaasBenefit, got)
	}
	authz := got.Get("Authorization")
	if !strings.Contains(authz, HeaderMaasType) {
		t.Fatalf("SignedHeaders 必须覆盖 %s（否则上游按未签名头拒），authz=%s", HeaderMaasType, authz)
	}

	// 非福利模型不得带该头（否则会被路由到福利网关）。
	if _, err := c.SendChatV2(context.Background(), body, "trace-2", cred, "tok", false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got = header.Clone()
	mu.Unlock()
	if got.Get(HeaderMaasType) != "" || strings.Contains(got.Get("Authorization"), HeaderMaasType) {
		t.Fatalf("非福利请求不应出现 %s，headers=%v", HeaderMaasType, got)
	}
}

func TestFetchModelsMergesSourcesAndGatesClaim(t *testing.T) {
	var mu sync.Mutex
	var claims int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			claims++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent-center/agents/useragents":
			_, _ = io.WriteString(w, `{"agents":[{"agent_id":"a1","agent_name":"Other","is_primary_agent":true},`+
				`{"agent_id":"a2","agent_name":"CodeAgent","show_in_ide":true,"alias":{"alias_zh_cn":"智能体"}}]}`)
		case "/v1/agent-center/agents/detail":
			if r.URL.Query().Get("agent_id") != "a2" {
				t.Errorf("应选中 CodeAgent（alias 智能体 + show_in_ide），got=%s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"gpts":{"models":[{"model_name":"GLM-5.2","model_parameters":{"context_window":202752,"max_tokens":8192}}]}}`)
		case "/v1/model/builtin":
			if r.Header.Get("Agent-Type") != "PromptCenter" {
				t.Errorf("builtin 接口需带 Agent-Type: PromptCenter，got=%q", r.Header.Get("Agent-Type"))
			}
			_, _ = io.WriteString(w, `{"builtinModels":[{"model_id":"OpenPangu-2.0-Pro","context_window":524288}]}`)
		case "/api/v1/gateway/config":
			_, _ = io.WriteString(w, `{"error_code":"0000","result":{"base_url":"https://example.invalid","models":[`+
				`{"model_id":"glm-5.3-flash","context_window":1048576,"max_tokens":65536}]}}`)
		case "/api/v1/benefit/claim":
			_, _ = io.WriteString(w, `{"error_code":"0000","error_msg":""}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(5 * time.Second)
	c.snapBase = srv.URL
	c.benefitBase = srv.URL
	acct := &auth.Auth{UserID: "u-merge", UserName: "n", AccessKeyID: "AK", SecretAccessKey: "SK", CloudDragonTok: "ST"}

	infos, err := c.FetchModels(acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Fatalf("三路合并应得 3 个模型，got=%+v", infos)
	}
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[strings.ToLower(mi.ID)] = mi
	}
	if !byID["glm-5.3-flash"].Benefit {
		t.Error("福利网关返回的模型应标记 Benefit")
	}
	if byID["openpangu-2.0-pro"].Benefit || byID["glm-5.2"].Benefit {
		t.Error("内置模型不应标记 Benefit")
	}
	if byID["glm-5.2"].ContextWindow != 202752 || byID["glm-5.2"].MaxTokens != 8192 {
		t.Errorf("agent-center 参数未解析：%+v", byID["glm-5.2"])
	}
	mu.Lock()
	gotClaims := claims
	mu.Unlock()
	// 默认领取：不领取时福利模型在上游一律 benefit not found（实测 2026-09-15），
	// 所以发现福利模型时必须顺手领取（幂等，与官方客户端行为一致）。
	if gotClaims != 1 {
		t.Fatalf("默认应自动领取一次福利（幂等），claims=%d", gotClaims)
	}
	if !IsBenefitModel("u-merge", "GLM-5.3-FLASH") {
		t.Error("发现结果应写入该账号目录，供聊天路由判定")
	}
	if _, ok := AccountModels("u-merge"); !ok {
		t.Error("发现结果应可读回")
	}

	// 显式关闭后不得再 POST（该开关是给「不想让服务写账号」的部署留的）。
	c.SetBenefitAutoClaim(false)
	if _, err := c.FetchModels(acct); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotClaims = claims
	mu.Unlock()
	if gotClaims != 1 {
		t.Fatalf("关闭自动领取后不应再提交 claim，claims=%d", gotClaims)
	}
}
