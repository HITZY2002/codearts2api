// 模型目录与限时福利路由。
//
// 上游可用模型来自三路（对齐官方客户端 ModelService）：
//  1. agent-center：useragents 找 CodeAgent（alias 智能体 + show_in_ide）→ detail 的 gpts.models
//  2. 内置：GET /v1/model/builtin（Agent-Type: PromptCenter）
//  3. 限时福利（免费套餐）：GET {opengw}/api/v1/gateway/config
//
// 福利模型不在 agent-center 注册，聊天必须追加请求头 maas_type: benefit，
// 否则上游返回 InferHub.002002009.404 model is not registered。
//
// 目录按账号隔离：福利是按账号授予的，池里 A 账号有、B 账号没有时不允许互相污染，
// 因此 IsBenefitModel 必须带账号身份查询（见 SetAccountModels）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"codearts2api/internal/auth"
)

// CatalogTTL 账号模型目录的有效期（超过则建议重新发现）。
const CatalogTTL = time.Hour

// 福利路由头：官方 renderer 检测到 isFreeBenefit 即添加，经 kernel 转发到上游聊天请求。
const (
	HeaderMaasType = "maas_type"
	MaasBenefit    = "benefit"
)

// ModelInfo 模型信息（与 workbuddy/trae 一致）。
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int64  `json:"contextWindow,omitempty"`
	MaxTokens     int64  `json:"maxTokens,omitempty"`
	// Benefit 限时福利（免费套餐）模型：聊天需带 maas_type: benefit 头。
	Benefit bool   `json:"benefit,omitempty"`
	Desc    string `json:"desc,omitempty"`
}

// 冷启动种子：福利套餐轮换后新增的模型在首次发现前也要走福利路由，
// 少带一次头就是一次 404，宁可多带。发现结果只会在此基础上增补。
var seedBenefitModels = []string{
	"deepseek-v4-flash-0731", "deepseek-v4-pro-0813", "glm-5.3-flash",
}

// 内置模型种子（大小写归一用，不标记福利）。
var seedKnownModels = []string{
	"GLM-5.2", "GLM-5.2-ArkTS-SPARK", "GLM-5.1", "GLM-4.7",
	"OpenPangu-2.0-Pro", "OpenPangu-2.0-Flash",
	"Qwen3-VL-235B", "Qwen3.5-397B-A17B-VL", "Qwen3.6-27B-VL",
}

var seedBenefit = func() map[string]bool {
	m := make(map[string]bool, len(seedBenefitModels))
	for _, id := range seedBenefitModels {
		m[strings.ToLower(id)] = true
	}
	return m
}()

// accountCatalog 一个账号的模型目录快照。
type accountCatalog struct {
	infos   []ModelInfo
	benefit map[string]bool // lower(id) -> true（本轮发现确认的福利）
	knows   map[string]bool // lower(id) -> true（该账号目录里登记过的全部模型）
	// lastBenefit 上一次福利来源成功时的福利条目。福利网关临时故障时用它兜底：
	// 只在内置来源明确说「它不是福利」时才摘标记，否则保留，避免把
	// maas_type: benefit 头弄丢（丢了就是 InferHub.002002009.404）。
	lastBenefit map[string]ModelInfo
	fetched     time.Time
}

var catalog = struct {
	sync.RWMutex
	accounts map[string]*accountCatalog // accountID -> 目录
	known    map[string]string          // lower(id) -> 精确 ID（跨账号，大小写归一）
}{accounts: map[string]*accountCatalog{}, known: map[string]string{}}

func init() {
	catalog.known = make(map[string]string, len(seedKnownModels)+len(seedBenefitModels))
	for _, id := range append(append([]string{}, seedKnownModels...), seedBenefitModels...) {
		if _, ok := catalog.known[strings.ToLower(id)]; !ok {
			catalog.known[strings.ToLower(id)] = id
		}
	}
}

// SetAccountModels 整体替换某账号的模型目录（不是增补：套餐轮换后旧模型不得残留）。
// 等价于「本轮福利来源也成功」，因此不保留上一轮福利记忆。
func SetAccountModels(accountID string, infos []ModelInfo) {
	setAccountModels(accountID, infos, false)
}

// setAccountModels 写入账号目录。
//
// keepBenefit=true 表示本轮福利来源失败（内置来源成功）：此时保留上一次成功的
// 福利条目，否则非种子的福利模型会瞬间丢掉 maas_type: benefit 标记，直到下一次
// 刷新成功为止——这期间它们在上游按未注册模型 404。
func setAccountModels(accountID string, infos []ModelInfo, keepBenefit bool) {
	if accountID == "" {
		return
	}
	ac := &accountCatalog{
		infos:       append([]ModelInfo(nil), infos...),
		benefit:     make(map[string]bool),
		knows:       make(map[string]bool),
		lastBenefit: map[string]ModelInfo{},
		fetched:     time.Now(),
	}
	catalog.Lock()
	defer catalog.Unlock()
	if catalog.known == nil {
		catalog.known = map[string]string{}
	}
	if prev := catalog.accounts[accountID]; prev != nil {
		for k, v := range prev.lastBenefit {
			ac.lastBenefit[k] = v
		}
	}
	for _, mi := range ac.infos {
		if mi.ID == "" {
			continue
		}
		key := strings.ToLower(mi.ID)
		ac.knows[key] = true
		if mi.Benefit {
			ac.benefit[key] = true
			ac.lastBenefit[key] = mi
		}
		if _, ok := catalog.known[key]; !ok || mi.ID != key {
			catalog.known[key] = mi.ID
		}
	}
	if keepBenefit {
		// 本轮内置来源没有提到的福利模型：继续按福利路由（宁可多带，不可漏带）。
		for key, mi := range ac.lastBenefit {
			if ac.knows[key] {
				continue // 内置来源明确登记了它 → 已转正，不保留标记
			}
			ac.benefit[key] = true
			ac.knows[key] = true
			ac.infos = append(ac.infos, mi)
			if _, ok := catalog.known[key]; !ok || mi.ID != key {
				catalog.known[key] = mi.ID
			}
		}
		sort.Slice(ac.infos, func(i, j int) bool {
			li, lj := strings.ToLower(ac.infos[i].ID), strings.ToLower(ac.infos[j].ID)
			if li != lj {
				return li < lj
			}
			return ac.infos[i].ID < ac.infos[j].ID
		})
	}
	catalog.accounts[accountID] = ac
}

// AccountModels 返回某账号的模型目录快照（未发现时 ok=false）。
func AccountModels(accountID string) ([]ModelInfo, bool) {
	catalog.RLock()
	defer catalog.RUnlock()
	ac, ok := catalog.accounts[accountID]
	if !ok {
		return nil, false
	}
	return append([]ModelInfo(nil), ac.infos...), true
}

// AccountCatalogStale 报告某账号目录是否需重新发现（未发现或超过 CatalogTTL）。
func AccountCatalogStale(accountID string) bool {
	catalog.RLock()
	defer catalog.RUnlock()
	ac, ok := catalog.accounts[accountID]
	return !ok || time.Since(ac.fetched) >= CatalogTTL
}

// IsBenefitModel 报告该账号下该模型是否走限时福利路由（大小写不敏感）。
//
// 判定顺序：
//  1. 该账号目录里标了福利 → true（发现结果优先于种子）
//  2. 该账号目录里有但没有标福利 → false：它已经以内置/agent-center 身份注册，
//     套餐轮换后福利模型转正就靠这条摘掉 maas_type 头
//  3. 目录里没有这个模型（尚未发现、福利来源失败或未领取）→ 回退冷启动种子，
//     少带一次头就是一次 InferHub.002002009.404，宁可多带
func IsBenefitModel(accountID, model string) bool {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return false
	}
	catalog.RLock()
	defer catalog.RUnlock()
	if ac, ok := catalog.accounts[accountID]; ok {
		if ac.benefit[key] {
			return true
		}
		if ac.knows[key] {
			return false
		}
	}
	return seedBenefit[key]
}

// lookupKnownModel 按小写 ID 查精确模型 ID（种子 + 已发现模型）。
func lookupKnownModel(id string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(id))
	if key == "" {
		return "", false
	}
	catalog.RLock()
	defer catalog.RUnlock()
	exact, ok := catalog.known[key]
	return exact, ok
}

// MergeModels 合并多路模型列表：按小写 ID 去重，福利标记取或，参数更全的优先。
// 结果按小写 ID 排序，保证 /v1/models 输出稳定（缓存刷新前后顺序一致）。
func MergeModels(sets ...[]ModelInfo) []ModelInfo {
	merged := map[string]ModelInfo{}
	for _, set := range sets {
		for _, mi := range set {
			if mi.ID == "" {
				continue
			}
			key := strings.ToLower(mi.ID)
			cur, ok := merged[key]
			if !ok {
				merged[key] = mi
				continue
			}
			merged[key] = richerModel(cur, mi)
		}
	}
	out := make([]ModelInfo, 0, len(merged))
	for _, mi := range merged {
		out = append(out, mi)
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].ID), strings.ToLower(out[j].ID)
		if li != lj {
			return li < lj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// richerModel 合并同一模型的两条记录：福利标记取或（漏标即漏头），其余取更全的一条。
func richerModel(cur, next ModelInfo) ModelInfo {
	benefit := cur.Benefit || next.Benefit
	keep, drop := cur, next
	if betterModel(next, cur) {
		keep, drop = next, cur
	}
	keep.Benefit = benefit
	if keep.Name == "" {
		keep.Name = drop.Name
	}
	if keep.ContextWindow == 0 {
		keep.ContextWindow = drop.ContextWindow
	}
	if keep.MaxTokens == 0 {
		keep.MaxTokens = drop.MaxTokens
	}
	if keep.Desc == "" {
		keep.Desc = drop.Desc
	}
	return keep
}

// betterModel 报告 a 是否比 b 更值得保留：优先带参数，其次保留精确大小写。
func betterModel(a, b ModelInfo) bool {
	if (a.ContextWindow > 0) != (b.ContextWindow > 0) {
		return a.ContextWindow > 0
	}
	return a.ID != strings.ToLower(a.ID) && b.ID == strings.ToLower(b.ID)
}

// FetchModels 拉取指定账号当前可用模型（三路合并），并写入该账号的模型目录。
//
// 任一路失败只记日志不阻断：福利接口不可用时仍能拿到内置模型。
func (c *Client) FetchModels(acct *auth.Auth) ([]ModelInfo, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for model fetch")
	}
	cred := SignCredential{
		AccessKeyID:     acct.AccessKeyID,
		SecretAccessKey: acct.SecretAccessKey,
		SecurityToken:   acct.CloudDragonTok,
	}
	accountID := acct.UserID
	var sets [][]ModelInfo
	if infos, err := c.fetchAgentModels(cred); err != nil {
		Log("agent models: %v", err)
	} else {
		sets = append(sets, infos)
	}
	builtinOK := false
	if infos, err := c.fetchBuiltinModels(accountID, cred); err != nil {
		Log("builtin models: %v", err)
	} else {
		builtinOK = true
		sets = append(sets, infos)
	}
	benefitOK := false
	if infos, err := c.fetchBenefitModels(accountID, cred); err != nil {
		Log("benefit models: %v", err)
	} else {
		benefitOK = true
		sets = append(sets, infos)
	}
	out := MergeModels(sets...)
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// 福利来源失败但内置来源成功：保留上一轮已确认的福利条目，别把标记弄丢。
	setAccountModels(accountID, out, builtinOK && !benefitOK)
	return out, nil
}

// fetchAgentModels 官方 fetchAgentModels 等价实现：agent-center detail 的 gpts.models。
func (c *Client) fetchAgentModels(cred SignCredential) ([]ModelInfo, error) {
	agentID, err := c.defaultAgentID(cred)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return nil, fmt.Errorf("no default agent found")
	}
	raw, err := c.getSigned(context.Background(), c.snapURL(EpAgentDetail)+"?agent_id="+url.QueryEscape(agentID), cred, true)
	if err != nil {
		return nil, err
	}
	var detail struct {
		Gpts struct {
			Models []struct {
				ModelAlias string `json:"model_alias"`
				ModelName  string `json:"model_name"`
				ModelID    string `json:"model_id"`
				Params     struct {
					ContextWindow int64  `json:"context_window"`
					MaxTokens     int64  `json:"max_tokens"`
					ModelID       string `json:"model_id"`
					ModelDesc     string `json:"model_desc"`
				} `json:"model_parameters"`
			} `json:"models"`
		} `json:"gpts"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	out := make([]ModelInfo, 0, len(detail.Gpts.Models))
	for _, m := range detail.Gpts.Models {
		id := firstNonEmpty(m.ModelName, m.ModelID, m.ModelAlias, m.Params.ModelID)
		if id == "" {
			continue
		}
		out = append(out, ModelInfo{
			ID:            id,
			Name:          id,
			ContextWindow: m.Params.ContextWindow,
			MaxTokens:     m.Params.MaxTokens,
			Desc:          m.Params.ModelDesc,
		})
	}
	return out, nil
}

// defaultAgentID 拉取用户 agent 列表，返回 CodeAgent 的 agent_id。
// 过滤条件与官方客户端一致：agent_name=="CodeAgent" && alias.alias_zh_cn=="智能体" && show_in_ide，
// 退化为 is_primary_agent，再退化为首个。
func (c *Client) defaultAgentID(cred SignCredential) (string, error) {
	raw, err := c.getSigned(context.Background(), c.snapURL(EpAgentList)+"?offset=0&limit=100&is_primary_agent=true", cred, true)
	if err != nil {
		return "", err
	}
	var out struct {
		Agents []struct {
			AgentID   string `json:"agent_id"`
			AgentName string `json:"agent_name"`
			Primary   bool   `json:"is_primary_agent"`
			ShowInIDE bool   `json:"show_in_ide"`
			Alias     struct {
				ZhCN string `json:"alias_zh_cn"`
			} `json:"alias"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parse agents: %w", err)
	}
	for _, a := range out.Agents {
		if a.AgentName == "CodeAgent" && a.Alias.ZhCN == "智能体" && a.ShowInIDE {
			return a.AgentID, nil
		}
	}
	for _, a := range out.Agents {
		if a.Primary {
			return a.AgentID, nil
		}
	}
	if len(out.Agents) > 0 {
		return out.Agents[0].AgentID, nil
	}
	return "", nil
}

// fetchBuiltinModels GET /v1/model/builtin（Agent-Type: PromptCenter）。
func (c *Client) fetchBuiltinModels(accountID string, cred SignCredential) ([]ModelInfo, error) {
	key := accountID + "|builtin"
	if c.srcBlocked(key) {
		return nil, fmt.Errorf("builtin source backoff")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.snapURL(EpModelBuiltin), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, []byte{}, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		c.noteSrcFail(key)
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		c.noteSrcFail(key)
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: EpModelBuiltin}
	}
	var out struct {
		BuiltinModels []struct {
			ModelID       string `json:"model_id"`
			ModelName     string `json:"model_name"`
			ModelDesc     string `json:"model_desc"`
			ContextWindow int64  `json:"context_window"`
			MaxTokens     int64  `json:"max_tokens"`
		} `json:"builtinModels"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		c.noteSrcFail(key)
		return nil, fmt.Errorf("parse builtin models: %w", err)
	}
	c.clearSrcFail(key)
	infos := make([]ModelInfo, 0, len(out.BuiltinModels))
	for _, m := range out.BuiltinModels {
		id := firstNonEmpty(m.ModelID, m.ModelName)
		if id == "" {
			continue
		}
		infos = append(infos, ModelInfo{ID: id, Name: id, ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, Desc: m.ModelDesc})
	}
	return infos, nil
}

// ClaimBenefit 领取限时福利（幂等：已领取返回成功）。官方客户端打开模型菜单即调用。
//
// 这是对账号的写操作，默认不随模型发现自动执行（见 Client.SetBenefitAutoClaim）：
// 需要领取时显式调用（`codearts2api models -claim` 或打开自动领取开关）。
func (c *Client) ClaimBenefit(cred SignCredential) error {
	body, _ := json.Marshal(map[string]any{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.benefitURL(EpBenefitClaim), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, body, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: EpBenefitClaim}
	}
	var out struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("parse claim: %w", err)
	}
	if out.ErrorCode != "0000" {
		return fmt.Errorf("claim failed: error_code=%s msg=%s", out.ErrorCode, out.ErrorMsg)
	}
	return nil
}

// fetchBenefitModels 拉取限时福利模型（gateway/config），返回结果整体标记 Benefit。
func (c *Client) fetchBenefitModels(accountID string, cred SignCredential) ([]ModelInfo, error) {
	key := accountID + "|benefit"
	if c.srcBlocked(key) {
		return nil, fmt.Errorf("benefit source backoff")
	}
	if c.claimAuto.Load() {
		if err := c.ClaimBenefit(cred); err != nil {
			Log("benefit claim: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.benefitURL(EpBenefitConfig), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, []byte{}, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		c.noteSrcFail(key)
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		c.noteSrcFail(key)
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: EpBenefitConfig}
	}
	var out struct {
		ErrorCode string `json:"error_code"`
		Result    struct {
			BaseURL string `json:"base_url"`
			Models  []struct {
				ModelID       string `json:"model_id"`
				ModelName     string `json:"model_name"`
				ModelDesc     string `json:"model_desc"`
				ContextWindow int64  `json:"context_window"`
				MaxTokens     int64  `json:"max_tokens"`
			} `json:"models"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		c.noteSrcFail(key)
		return nil, fmt.Errorf("parse benefit config: %w", err)
	}
	if out.ErrorCode != "0000" {
		c.noteSrcFail(key)
		return nil, fmt.Errorf("gateway/config error_code=%s", out.ErrorCode)
	}
	c.clearSrcFail(key)
	infos := make([]ModelInfo, 0, len(out.Result.Models))
	for _, m := range out.Result.Models {
		id := firstNonEmpty(m.ModelID, m.ModelName)
		if id == "" {
			continue
		}
		infos = append(infos, ModelInfo{
			ID: id, Name: id,
			ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, Desc: m.ModelDesc,
			Benefit: true,
		})
	}
	return infos, nil
}
