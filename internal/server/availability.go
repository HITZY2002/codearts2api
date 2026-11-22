package server

import (
	"bufio"
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// 模型可用性探测。
//
// 背景（2026-09-15 实测）：上游 /v1/models 的发现结果 ≠ 账号真能调用的模型。
// agent-center 与 /v1/model/builtin 会列出未在当前账号注册的模型（实测
// GLM-5.2-ArkTS-SPARK / OpenPangu-2.0-Pro / OpenPangu-2.0-Flash 全部返回
// InferHub.002002009.404），福利模型在未领取时返回 InferHub.4004.200
// benefit not found。只按发现结果列模型，客户端会拿到一堆必然失败的条目。
//
// 因此这里做一次轻量真实探测（一条最小 chat 请求），把结果缓存起来：
//   - /v1/models 过滤掉已知不可用的模型
//   - 聊天路由避开不可用模型
//   - 上游报 not registered / benefit not found 时立即记为不可用（从真实流量学习）

const (
	modelAccessDeniedReason = "当前账号无权调用该模型"

	// availabilityTTL 探测结果有效期。套餐/福利变化后最多滞后这么久。
	availabilityTTL = 30 * time.Minute
	// probeTimeout 单次探测超时。
	probeTimeout = 45 * time.Second
	// probeInterval 后台批量探测间隔。
	probeInterval = 15 * time.Minute
	// probeStartDelay 启动后多久开始首轮探测（避开接入初期的真实请求）。
	probeStartDelay = 2 * time.Minute
	// probeIdleBefore 账号至少空闲这么久才探测。真实请求本身就会写可用性结论
	// （成功 markUsable / 失败 markUnusable），探测只是空闲时补齐，避免与用户
	// 抢上游那 3 个并发会话槽位（槽位释放实测 >15s）。
	probeIdleBefore = 10 * time.Minute
	// probeConcurrency 并发探测数。探测本身会占上游会话槽位（单账号 3 个并发），
	// 且账号并发锁默认 max_concurrent=1，所以这里也用 1：慢一点，但不能干扰真实请求。
	probeConcurrency = 1
)

// probeGap 两次探测之间的间隔（var 仅为测试可注入缩短）。探针本身是串行的，
// 但它占用的上游会话槽位释放很慢（实测 >15s）：连续探 3 个就占满单账号的
// 3 个并发会话，紧随其后的真实请求会被上游判为 TM.00001041 并开始排队。
// 留出释放窗口再探下一个；生产值必须 >= 15s（TestProbeGapExists 钳制）。
var probeGap = 20 * time.Second

type availState int

const (
	availUnknown availState = iota
	availUsable
	availUnusable
)

type availEntry struct {
	state  availState
	reason string
	at     time.Time
}

// probesStop 在服务退出时关闭，让带 sleep 的探测循环尽快结束。
var (
	probesStop     = make(chan struct{})
	probesStopOnce sync.Once
)

var availability = struct {
	sync.Mutex
	byKey     map[string]availEntry // accountID|modelLower -> 状态
	probing   map[string]bool       // 正在探测的 key（去重）
	sweeping  bool                  // 是否已有一轮后台批量探测在跑
	lastSweep time.Time
	// lastTraffic accountID -> 最后一次真实请求时间。探测只为「补齐结论」，
	// 有真实流量的账号不探测（真实请求本身就会写结论，且探测占用的上游会话
	// 槽位释放很慢，会挤掉用户请求）。
	lastTraffic map[string]time.Time
}{byKey: map[string]availEntry{}, probing: map[string]bool{}, lastTraffic: map[string]time.Time{}}

func availKey(accountID, model string) string {
	return accountID + "|" + strings.ToLower(model)
}

// modelAvailability 返回缓存的可用性；过期视为未知。
func modelAvailability(accountID, model string) (availState, string) {
	availability.Lock()
	defer availability.Unlock()
	e, ok := availability.byKey[availKey(accountID, model)]
	if !ok || time.Since(e.at) > availabilityTTL {
		return availUnknown, ""
	}
	return e.state, e.reason
}

// markUnusable 记录某账号下某模型不可用（来自探测或真实请求失败）。
func markUnusable(accountID, model, reason string) {
	if accountID == "" || model == "" {
		return
	}
	availability.Lock()
	defer availability.Unlock()
	availability.byKey[availKey(accountID, model)] = availEntry{state: availUnusable, reason: reason, at: time.Now()}
}

// markUsable 记录某账号下某模型可用。
func markUsable(accountID, model string) {
	if accountID == "" || model == "" {
		return
	}
	availability.Lock()
	defer availability.Unlock()
	availability.byKey[availKey(accountID, model)] = availEntry{state: availUsable, at: time.Now()}
}

// noteTraffic 记录账号上一次真实请求时间（探测据此避让）。
func noteTraffic(accountID string) {
	if accountID == "" {
		return
	}
	availability.Lock()
	defer availability.Unlock()
	availability.lastTraffic[accountID] = time.Now()
}

// trafficIdleFor 返回账号距上次真实请求的空闲时长；从未有流量返回极大值。
func trafficIdleFor(accountID string) time.Duration {
	availability.Lock()
	defer availability.Unlock()
	t, ok := availability.lastTraffic[accountID]
	if !ok {
		return time.Duration(1<<62 - 1)
	}
	return time.Since(t)
}

// upstreamUnavailableReason 把上游错误归类为「该模型对这个账号不可用」，否则返回空串。
func upstreamUnavailableReason(err error) string {
	if err == nil {
		return ""
	}
	return upstreamUnavailableReasonText(err.Error())
}

// probeModel 用一条最小请求真实试一次该模型，并记录结果。
//
// 探测会占用上游会话槽位（单账号只有 3 个并发会话），因此这里必须走账号的并发锁：
// 拿不到锁就让位给真实请求，等下一轮再试——否则用户请求会被探测挤成
// TM.00001041「并发会话数已达上限」。
func (h *Handler) probeModel(acct *pool.Account, model string) {
	h.probeModelCtx(context.Background(), acct, model)
}

func (h *Handler) probeModelCtx(parentCtx context.Context, acct *pool.Account, model string) {
	if acct == nil || acct.AuthSnapshot() == nil {
		return
	}
	// 账号一旦有真实请求在跑（或刚跑过），本轮探测整体让位：探测只是为了让
	// /v1/models 说真话，绝不能因此让用户请求排队等锁。下一轮再试。
	if h.cfg.Pool.Busy(acct.Name) || trafficIdleFor(acct.UID) < probeIdleBefore {
		return
	}
	if !h.cfg.Pool.AcquireLock(acct.Name) {
		return
	}
	defer h.cfg.Pool.ReleaseLock(acct.Name)
	key := availKey(acct.UID, model)
	availability.Lock()
	if availability.probing[key] {
		availability.Unlock()
		return
	}
	availability.probing[key] = true
	availability.Unlock()
	defer func() {
		availability.Lock()
		delete(availability.probing, key)
		availability.Unlock()
	}()

	authz := acct.AuthSnapshot()
	token, ak, sk := authz.Credentials()
	cred := upstream.SignCredential{AccessKeyID: ak, SecretAccessKey: sk, SecurityToken: token}
	ctx, cancel := context.WithTimeout(parentCtx, probeTimeout)
	defer cancel()
	rc, err := acct.Client.ProbeModel(ctx, model, cred, upstream.IsBenefitModel(acct.UID, model))
	if err != nil {
		// 并发上限是暂时的，不代表模型不可用。
		if strings.Contains(err.Error(), "TM.00001041") {
			return
		}
		if reason := upstreamUnavailableReason(err); reason != "" {
			markUnusable(acct.UID, model, reason)
			if h.cfg.Telemetry != nil {
				_ = h.cfg.Telemetry.RecordEvent("model_probe_failed", "warn", acct.UID, model, reason, nil)
			}
			log.Printf("probe %s/%s: 不可用：%s", acct.Name, model, reason)
			return
		}
		log.Printf("probe %s/%s: 未判定 %v", acct.Name, model, err)
		if h.cfg.Telemetry != nil {
			_ = h.cfg.Telemetry.RecordEvent("model_probe_failed", "error", acct.UID, model, "probe inconclusive", nil)
		}
		return
	}
	defer rc.Close()
	// 读到首个 data 事件（或非 SSE 首行）为止，上限 64KB / 128 行：
	// 200 也要看内容，上游可能把错误塞在心跳/注释行之后的 SSE data 事件里。
	const (
		maxProbeHeadBytes = 64 * 1024
		maxProbeHeadLines = 128
	)
	br := bufio.NewReaderSize(io.LimitReader(rc, maxProbeHeadBytes+1), maxProbeHeadBytes)
	var head strings.Builder
	for i := 0; i < maxProbeHeadLines && head.Len() < maxProbeHeadBytes; i++ {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			remain := maxProbeHeadBytes - head.Len()
			if len(line) > remain {
				line = line[:remain]
			}
			head.WriteString(line)
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			if payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")); payload != "" {
				break
			}
		} else if trimmed != "" && !strings.HasPrefix(trimmed, ":") &&
			!strings.HasPrefix(trimmed, "event:") &&
			!strings.HasPrefix(trimmed, "id:") &&
			!strings.HasPrefix(trimmed, "retry:") {
			// 非 SSE 响应体（如纯 JSON 错误）读到首条有效内容即可判定。
			break
		}
		if err != nil {
			break
		}
	}
	headStr := head.String()
	if reason := upstreamUnavailableReasonText(headStr); reason != "" {
		markUnusable(acct.UID, model, reason)
		if h.cfg.Telemetry != nil {
			_ = h.cfg.Telemetry.RecordEvent("model_probe_failed", "warn", acct.UID, model, reason, nil)
		}
		log.Printf("probe %s/%s: 不可用：%s", acct.Name, model, reason)
		return
	}
	if headStr == "" {
		return
	}
	markUsable(acct.UID, model)
	if h.cfg.Telemetry != nil {
		_ = h.cfg.Telemetry.RecordEvent("model_probe_succeeded", "info", acct.UID, model, "model available", nil)
	}
	log.Printf("probe %s/%s: 可用", acct.Name, model)
}

// upstreamUnavailableReasonText 同 upstreamUnavailableReason，作用于响应正文。
func upstreamUnavailableReasonText(s string) string {
	low := strings.ToLower(s)
	switch {
	case upstream.IsModelAccessErrorMessage(s):
		return modelAccessDeniedReason
	case strings.Contains(low, "002002009") || strings.Contains(low, "not registered"):
		return "模型未在当前账号注册"
	case strings.Contains(low, "4004.200") || strings.Contains(low, "benefit not found"):
		return "限时福利未领取或已过期（可执行 go run ./cmd/models -claim，或打开 benefit_auto_claim）"
	}
	return ""
}

// StartAvailabilityProber 供 main 启动后台探测器。
func (h *Handler) StartAvailabilityProber(ctx context.Context) { h.startAvailabilityProber(ctx) }

// startAvailabilityProber 后台周期性探测：把「发现到但还没结论」的模型试一遍。
// 只在有账号且拿到目录后才干活，避免空转打上游。
func (h *Handler) startAvailabilityProber(ctx context.Context) {
	go func() {
		<-ctx.Done()
		probesStopOnce.Do(func() { close(probesStop) })
	}()
	go func() {
		// 启动后先等服务预热：立刻探测会和刚接入的客户端抢账号槽位。
		select {
		case <-ctx.Done():
			return
		case <-time.After(probeStartDelay):
		}
		ticker := time.NewTicker(probeInterval)
		defer ticker.Stop()
		h.sweepAvailabilityCtx(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.sweepAvailabilityCtx(ctx)
			}
		}
	}()
}

// sweepAvailability 对所有健康账号的已发现模型做一轮探测（跳过已有结论的）。
//
// 与流量学习触发的 probeUnknownAsync 同走 runProbes：探测之间保留 probeGap
// 释放窗口，并响应 probesStop。此前 sweep 是背靠背的裸循环——待探测组合多时
// 连续占满上游会话槽位，真实请求会被 TM.00001041 排队。
func (h *Handler) sweepAvailability() {
	h.sweepAvailabilityCtx(context.Background())
}

func (h *Handler) sweepAvailabilityCtx(ctx context.Context) {
	availability.Lock()
	if time.Since(availability.lastSweep) < probeInterval/2 {
		availability.Unlock()
		return
	}
	availability.lastSweep = time.Now()
	availability.Unlock()

	h.runProbesCtx(ctx, h.pendingProbes())
}

// pendingProbe 一个待探测的「账号 + 模型」。
type pendingProbe struct {
	acct  *pool.Account
	model string
}

// pendingProbes 列出所有「健康账号 × 已发现但还没有可用性结论」的组合。
func (h *Handler) pendingProbes() []pendingProbe {
	var out []pendingProbe
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil || acct.AuthSnapshot() == nil || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		models, ok := upstream.AccountModels(acct.UID)
		if !ok {
			continue
		}
		for _, mi := range models {
			if st, _ := modelAvailability(acct.UID, mi.ID); st != availUnknown {
				continue
			}
			out = append(out, pendingProbe{acct: acct, model: mi.ID})
		}
	}
	return out
}

// runProbes 并发执行待探测列表（上限 probeConcurrency）。
func (h *Handler) runProbes(items []pendingProbe) {
	h.runProbesCtx(context.Background(), items)
}

func (h *Handler) runProbesCtx(ctx context.Context, items []pendingProbe) {
	if len(items) == 0 {
		return
	}
	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	first := true
	for _, it := range items {
		// 跳过明确繁忙或刚有流量的账号，避免空等 probeGap 长时间占住 sweeping 标志。
		if it.acct == nil || it.acct.AuthSnapshot() == nil ||
			h.cfg.Pool.Busy(it.acct.Name) || trafficIdleFor(it.acct.UID) < probeIdleBefore {
			continue
		}
		// 让上一个探针占用的上游会话先释放，再探下一个。
		if !first {
			select {
			case <-time.After(probeGap):
			case <-ctx.Done():
				wg.Wait()
				return
			case <-probesStop:
				wg.Wait()
				return
			}
		}
		first = false
		wg.Add(1)
		sem <- struct{}{}
		go func(it pendingProbe) {
			defer wg.Done()
			defer func() { <-sem }()
			h.probeModelCtx(ctx, it.acct, it.model)
		}(it)
	}
	wg.Wait()
}
