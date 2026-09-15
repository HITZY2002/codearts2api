package server

import (
	"errors"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// 上游的「这个模型对这个账号不可用」必须被识别出来，而且不能被当成账号故障
// （否则健康账号会被记错误、累计到阈值冷却 10 分钟）。
func TestUpstreamUnavailableReason(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{`codearts error code=InferHub.002002009.404 msg=The model is not registered, please request other model`, "注册"},
		{`codearts error code=InferHub.4004.200 msg=benefit not found`, "福利"},
		{`codearts error code=TM.00001041 msg=并发会话数已达上限`, ""},
		{`dial tcp: i/o timeout`, ""},
	}
	for _, c := range cases {
		got := upstreamUnavailableReason(errors.New(c.msg))
		if c.want == "" && got != "" {
			t.Errorf("%q 不应判为模型不可用，got=%q", c.msg, got)
		}
		if c.want != "" && got == "" {
			t.Errorf("%q 应判为模型不可用", c.msg)
		}
	}
}

// 可用性缓存：过期视为未知；写入后按账号+模型隔离。
func TestAvailabilityCacheScoping(t *testing.T) {
	markUnusable("acct-A", "GLM-5.2", "未注册")
	if st, _ := modelAvailability("acct-A", "GLM-5.2"); st != availUnusable {
		t.Fatal("应记录为不可用")
	}
	if st, _ := modelAvailability("acct-A", "glm-5.2"); st != availUnusable {
		t.Error("大小写不敏感")
	}
	if st, _ := modelAvailability("acct-B", "GLM-5.2"); st != availUnknown {
		t.Error("可用性必须按账号隔离，不能跨账号污染")
	}
	markUsable("acct-A", "GLM-5.2")
	if st, _ := modelAvailability("acct-A", "GLM-5.2"); st != availUsable {
		t.Error("成功调用后应覆盖为可用")
	}
}

// 过期的结论不再生效，避免套餐变化后一直按旧结论办事。
func TestAvailabilityExpiry(t *testing.T) {
	markUsable("acct-exp", "m1")
	availability.Lock()
	e := availability.byKey[availKey("acct-exp", "m1")]
	e.at = time.Now().Add(-availabilityTTL - time.Minute)
	availability.byKey[availKey("acct-exp", "m1")] = e
	availability.Unlock()
	if st, _ := modelAvailability("acct-exp", "m1"); st != availUnknown {
		t.Error("过期结论应视为未知")
	}
}

// 过滤语义：只有「所有健康账号都判定不可用」才下架；只要还有账号没结论，
// 或者有一个账号可用，就必须保留（不能让探测没跑完把可用模型藏起来）。
func TestModelKnownUnusable(t *testing.T) {
	p, err := pool.New([]*auth.Auth{
		{UserID: "u1", UserName: "u1"}, {UserID: "u2", UserName: "u2"},
	}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(time.Second)})

	if h.modelKnownUnusable("m-keep") {
		t.Error("无结论时不应下架")
	}
	markUnusable("u1", "m-keep", "未注册")
	if h.modelKnownUnusable("m-keep") {
		t.Error("还有账号没结论时不应下架")
	}
	markUnusable("u2", "m-keep", "未注册")
	if !h.modelKnownUnusable("m-keep") {
		t.Error("全部账号都不可用时应下架")
	}
	markUsable("u2", "m-keep")
	if h.modelKnownUnusable("m-keep") {
		t.Error("有账号可用时不应下架")
	}
}

// 探测必须让位于真实请求：账号一忙就整体跳过，绝不能让用户请求排队等锁。
func TestProbeSkipsBusyAccount(t *testing.T) {
	a := &auth.Auth{UserID: "u-busy", UserName: "u-busy", AccessKeyID: "AK", SecretAccessKey: "SK", CloudDragonTok: "ST"}
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(time.Second)})
	upstream.SetAccountModels("u-busy", []upstream.ModelInfo{{ID: "m-busy"}})

	// 占用并发槽位，模拟真实请求在跑
	if !p.AcquireLock("u-busy") {
		t.Fatal("应能获取锁")
	}
	defer p.ReleaseLock("u-busy")
	if !p.Busy("u-busy") {
		t.Fatal("Busy 应为 true")
	}
	h.probeModel(p.Get("u-busy"), "m-busy")
	if st, _ := modelAvailability("u-busy", "m-busy"); st != availUnknown {
		t.Error("账号忙时探测应直接跳过，不得留下结论")
	}
}

// 探针占用的上游会话释放很慢（实测 >15s），连续探测会把单账号的 3 个并发会话
// 占满，紧随其后的真实请求被上游判为 TM.00001041。两次探测之间必须留释放窗口。
func TestProbeGapExists(t *testing.T) {
	if probeGap < 15*time.Second {
		t.Fatalf("probeGap=%s 太短，上游会话释放实测 >15s", probeGap)
	}
	if probeConcurrency != 1 {
		t.Fatalf("probeConcurrency=%d，探测必须串行以免占满会话槽位", probeConcurrency)
	}
}

// 排队重试不能无限等：默认值应远小于原来的 180×10s=30min。
func TestQueueRetryDefaultBounded(t *testing.T) {
	h := NewHandler(Config{})
	total := time.Duration(h.cfg.QueueMaxAttempts) * h.cfg.QueueRetryDelay
	if total > 10*time.Minute {
		t.Fatalf("排队重试上限 %s 过长，交互式请求会像挂死", total)
	}
	if total < time.Minute {
		t.Fatalf("排队重试上限 %s 过短，瞬时的上游排队会直接失败", total)
	}
}

// 模型级错误（not registered / benefit not found）不得被当成账号故障：
// 实测线上：调用未注册模型 → 错误被嵌入包装成 5xx → 健康账号被冷却 10 分钟，
// 之后所有正常请求全部失败。
func TestModelErrorDoesNotCoolAccount(t *testing.T) {
	a := &auth.Auth{UserID: "u-cool", UserName: "u-cool", AccessKeyID: "AK", SecretAccessKey: "SK", CloudDragonTok: "ST"}
	p, err := pool.New([]*auth.Auth{a}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(time.Second), ErrCooldown: time.Minute, ErrThreshold: 1})

	err404 := &upstream.ApiError{
		Code: 502, Status: 502, Path: "/api/v2/chat/completions",
		Message: "InferHub.002002009.404 The model is not registered, please request other model",
	}
	if !h.handleUpstreamError(p.Get("u-cool"), "GLM-5.2-ArkTS-SPARK", err404) {
		t.Error("模型级错误应返回 true，让调用方立即结束轮转")
	}

	if !p.Healthy("u-cool") {
		t.Error("模型级错误不得冷却账号")
	}
	st, reason := modelAvailability("u-cool", "GLM-5.2-ArkTS-SPARK")
	if st != availUnusable || reason == "" {
		t.Errorf("应记为模型不可用，got state=%v reason=%q", st, reason)
	}
}
