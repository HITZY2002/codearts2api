// 常量：CodeArts Agent 云端 API（来自 resources/app/extensions/vscode-codebot 逆向）。
package upstream

const (
	// SnapEngineApiHost 盘古引擎/聊天网关（snap-access，product.json snapEngineDomain）。
	SnapEngineApiHost = "https://snap-access.cn-north-4.myhuaweicloud.com"
	// SnapManagerHost 登录/令牌服务（snap-manager）。
	SnapManagerHost = SnapEngineApiHost + "/snap-manager"
	// PortalHost 华为云 CodeArts 门户（OAuth authorize 入口，注意带 /portal）。
	PortalHost = "https://codearts.huaweicloud.com/portal"
	// IAMHost IAM 令牌/STS（备用）。
	IAMHost = "https://iam.myhuaweicloud.com"
	// STSHost 临时 AK/SK。
	STSHost = "https://sts.cn-north-4.myhuaweicloud.com"

	// CLIENT_ID OAuth client（官方 CodeArts Agent 插件的 uri_scheme）。
	// refresh_token 与该值绑定：换 client_id 刷新会被 STS 拒
	// （invalid refresh token: 'invalid client id: xxx'）。实测 2026-09-15。
	CLIENT_ID = "codearts-agent"

	// 聊天端点（chatDomain 商业版 = snap-access.cn-north-4）。
	EpChatV2         = "/api/v2/chat/completions"
	EpChat           = "/v1/chat/chat"
	EpStartChat      = "/v1/chat/start-chat"
	EpChatAgents     = "/v1/chat/agents"
	EpChatManagement = "/v1/chat/management/event"
	EpChatRecord     = "/v1/chat/record-request"
	EpAgentList      = "/v1/agent-center/agents/useragents"
	EpAgentDetail    = "/v1/agent-center/agents/detail"
	// 内置模型（与官方 ModelService.listBuiltinModels 一致，Agent-Type: PromptCenter）。
	EpModelBuiltin = "/v1/model/builtin"

	// 限时福利（免费套餐）网关：模型发现 + 领取（opengw.developer 域）。
	BenefitHost     = "https://opengw.developer.huaweicloud.com"
	EpBenefitConfig = "/api/v1/gateway/config"
	EpBenefitClaim  = "/api/v1/benefit/claim"

	// 登录/令牌
	EpLoginTicket    = "/v1/login/ticket"
	EpOAuthTokens    = "/v1/oauth2/tokens"
	EpCurrentUser    = "/v1/current/user"
	EpCallerIdentity = "/v5/caller-identity"
)
