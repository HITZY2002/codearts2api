package upstream

import "testing"

// 福利来源临时失败时，不能让已发现的非种子福利模型丢掉 maas_type: benefit
// （丢了上游就按未注册模型 404）；模型真正转正后仍要能摘掉标记。
func TestPartialDiscoveryKeepsBenefit(t *testing.T) {
	acct := "acct-partial"
	SetAccountModels(acct, []ModelInfo{
		{ID: "brand-new-benefit-0915", Benefit: true},
		{ID: "GLM-5.2", ContextWindow: 202752},
	})
	if !IsBenefitModel(acct, "brand-new-benefit-0915") {
		t.Fatal("首次发现后应走福利路由")
	}
	// 福利来源失败 + 内置来源成功（keepBenefit=true）
	setAccountModels(acct, []ModelInfo{{ID: "GLM-5.2", ContextWindow: 202752}}, true)
	if !IsBenefitModel(acct, "brand-new-benefit-0915") {
		t.Error("福利来源失败后应保留福利标记")
	}
	// 福利来源恢复且模型被内置来源登记 → 转正，摘掉标记
	setAccountModels(acct, []ModelInfo{{ID: "brand-new-benefit-0915"}, {ID: "GLM-5.2"}}, false)
	if IsBenefitModel(acct, "brand-new-benefit-0915") {
		t.Error("模型转正后应摘掉福利标记")
	}
}
