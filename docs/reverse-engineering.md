# CodeArts Agent 逆向记录（2026-08-03，版本 26.7.0）

> 素材：`D:\CodeArts Agent\resources\app`（VS Code 系，未打包目录），
> 扩展 `vscode-codebot`（agent + snap API 客户端）、`huaweicloud.authentication`
> （登录）、`huaweicloud.codearts-agent-support`，以及 `product.json` 域名表。

## 1. 架构

CodeArts Agent 是 VS Code 系 Electron 应用（vscode 1.109.5 定制）。AI 对话走
**华为云 snap-access 盘古引擎**：

- 聊天：`POST https://snap-access.cn-north-4.myhuaweicloud.com/v1/chat/chat`（SSE）
- 登录：华为云 CodeArts OAuth2（PKCE），`snap-manager/v1/oauth2/tokens` 换 STS 临时凭证
- 鉴权：聊天用 `x-auth-token: <security_token>`；部分账号 API 用临时 AK/SK 签名

客户端里还带一个 Cline 系 agent 内核（`agentkernelServer-*.exe`，Bun 打包的本地
HTTP 服务），负责本地 agent 循环（会话/工具/文件），云端对话仍是上面的 chat 接口。

## 2. 关键端点（商业版 cn-north-4）

| 用途 | 方法/路径 | 鉴权 |
| --- | --- | --- |
| 聊天 | POST /v1/chat/chat | x-auth-token: security_token |
| 开始聊天 | POST /v1/chat/start-chat | 同上 |
| Agent 列表 | GET /v1/chat/agents | 同上 |
| 反馈事件 | POST /v1/chat/management/event | 同上 |
| 记录请求 | POST /v1/chat/record-request | 同上 |
| 换 token | POST /v1/oauth2/tokens（authorization_code / refresh_token） | client_id + code/refresh_token |
| ticket 轮询 | GET /v1/login/ticket?ticket_id=&secret= | plugin-name/version 头 |
| 当前用户 | GET /snap-manager/v1/current/user | AK/SK 签名 + X-Security-Token |
| 账号信息 | GET /v5/caller-identity（sts.cn-north-4） | AK/SK 签名 + X-Security-Token |

身份接口实测修正（2026-09-15，两个都踩过坑）：
- `current/user` 必须带 `snap-manager` 前缀：漏掉返回 `APIG.0101 The API does not exist`。
- `caller-identity` 只在 `sts.cn-north-4` 域可用：`iam.myhuaweicloud.com/v5/caller-identity`
  返回 `APIGW.0101`。
- 返回字段：caller-identity → `{account_id, principal_id, principal_urn}`；current/user →
  `{user_id, user_name, domain_id, domain_name}`。

域名表（product.json commercialVersionDomain.newFramework.productDomain）：
`chatDomain = codeGenDomain = snapEngineDomain = snap-access.cn-north-4.myhuaweicloud.com`；
`toolkitDomain = iam.myhuaweicloud.com`；`iamStsOpenDomain = sts.cn-north-4.myhuaweicloud.com`。

## 3. 登录流程（huaweicloud.authentication）

1. 生成 `ticket_id`（32 hex）、`secret`（32 hex）、PKCE `code_verifier/code_challenge`。
2. 本地监听 `127.0.0.1:{port}/oauth/callback`。
3. 构造 `https://codearts.huaweicloud.com/authorize?client_id=...&port=...&code_challenge=...&code_challenge_method=S256&ticket_id=...&plugin-name=huaweicloud.authentication&plugin-version=...`。
4. 双通道拿结果：
   - 回调带 `code` → `POST snap-manager/v1/oauth2/tokens`
     `{client_id, code, code_verifier, grant_type:"authorization_code", redirect_uri:"http://127.0.0.1:{port}/oauth/callback"}`
   - 轮询 `GET snap-manager/v1/login/ticket?ticket_id=&secret=`（插件头）
5. 响应含 `{user_id, user_name, domain_id, refresh_token, credentials:{access_key_id, secret_access_key, security_token, expiration}}`。
6. 刷新：`POST https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens`
   `{client_id, code_verifier, grant_type:"refresh_token", refresh_token}`（同样带 DPoP）。

`CLIENT_ID` = 应用 `uri_scheme`，实测为 **`codearts-agent`**（官方 CodeArts Agent 插件）。

**refresh_token 与 client_id、DPoP 公钥三者绑定**，实测两种失败：
- client_id 不匹配 → `400 STS5.1806 invalid refresh token: 'invalid client id: codearts-agent'`
- DPoP 私钥换新 → `400 STS5.1806 invalid refresh token: 'InvalidDPoPHeader'`
- refresh_token 是一次性的，用旧值再刷 → `invalid refresh token: 'the refresh token has been used'`

因此登录时的 client_id 与 DPoP 私钥必须随 refresh_token 一起落盘（`auths/*.json` 的
`client_id` / `dpop_private_key` 字段），刷新时原样复用，并把响应里轮转后的新
refresh_token 写回。

## 4. 聊天请求

Header：
```
Content-Type: application/json
Accept: text/event-stream
X-Sdk-Date: <YYYYMMDDTHHMMSSZ>
X-Security-Token: <STS security_token>（临时凭证时）
X-Sdk-Content-Sha256: <payload sha256>
Authorization: SDK-HMAC-SHA256 Access=<AK>, SignedHeaders=..., Signature=...
x-snap-traceid: <随机>
```

**鉴权不是 x-auth-token**：实测 x-auth-token 传 STS security_token 会被 APIG 拒
（APIG.0301 decrypt token fail）。桌面端走 `GlobalCredentialClient`，即华为云
AK/SK `SDK-HMAC-SHA256` 签名（signed headers = 请求全部头，CanonicalURI 带尾斜杠，
payload hash 取 X-Sdk-Content-Sha256）。已按该算法实现并实测通过。

Body：
```json
{"chat_id":"<32位hex>","client":"IDE","task":"chat",
 "messages":[{"type":"text","text":"..."}],
 "task_parameters":{"ide":"CodeArts Agent"},
 "batch_task_parameters":[],"attempt":1,"not_allow_external_model":true,
 "user_id":"<用户名>"}
```

实测要点：
- `chat_id` 必须是 **32 位十六进制**（UUID 去连字符），否则报「请求参数错误：chat_id」
- `messages` 是 **内容块数组（无 role）** `[{type:"text",text}]`，
  不是 OpenAI 的 {role,content}，否则报「请求参数错误：messages」
- 携带 `user_id`（用户名）更稳妥

## 5. SSE 格式

实测为**逐行 `data:` JSON（无空行分隔，部分行也无 event 前缀）**：
- 起始：`{"id":...,"model":"glm-4.7","type":"answer","chat_id":...,"response_message_id":...}`
- 全文快照：`{"text":"<当前完整文本>","output":[],"prompt_tokens":...,"completion_tokens":...}`
  （text 是**累计全文**，非增量，解析时用替换语义）
- 增量：`{"delta":{"content":"...","reasoning_content":"..."}}`（is_delta_response 场景）
- 结束：`{"text":"[DONE]","error_code":"0",...}`；错误形如
  `{"text":"[DONE]","error_code":"ChatAgent.00001001","error_msg":"..."}`
- `output` 数组：终态 `[{type:"output_text",text}]`

## 6. 额度/签到核对结论

- **没有每日签到接口**。
- **没有「申请额度」按钮**（免费额度按月重置，见 codearts 个人用量页）。
- 因此本项目用「token 自动 refresh 续期」作为对应自动签到的能力，
  并保留 `credit.sh`/`apply.sh` 运维脚本。

## 7. 限时福利模型（免费套餐，2026-09-06 逆向自 CodeArtsSpace 0.1.18）

> 该节由 PR #2（@threegod3）的逆向结论整理而来。

官方客户端可用模型 = 内置 + 限时福利两路合并（`ModelService.listBuiltinModels` + `getFreeBenefitModels`）：

- 福利发现：`GET https://opengw.developer.huaweicloud.com/api/v1/gateway/config`
  （AK/SK 签名 + `X-Security-Token`，无 Agent-Type），返回
  `{error_code:"0000", result:{base_url, models:[{model_id, model_name, context_window, max_tokens}]}}`。
  实测模型 ID 全小写：`deepseek-v4-flash-0731`、`deepseek-v4-pro-0813`、`glm-5.3-flash`。
- 领取/余额（幂等，官方客户端打开模型菜单即调用）：
  `POST /api/v1/benefit/claim {}`、`GET /api/v1/user/tokens/balance`（同 host 同签名）。
- 网关总开关：`GET {snap}/v1/benefit-gateway-config`（`Agent-Type: PromptCenter`）→ `{enabled}`。
- 聊天路由：**同一** `POST {snap}/api/v2/chat/completions`，福利模型需追加请求头
  `maas_type: benefit`（renderer 检测 `isFreeBenefit` 添加，经 kernel `KERNEL_LLM_FORWARD_HEADERS`
  转发）。无此头报 `InferHub.002002009.404 model is not registered`；有头正常流式。
- agent 选择：`useragents?offset=0&limit=100&is_primary_agent=true` 后取
  `agent_name=="CodeAgent" && alias.alias_zh_cn=="智能体" && show_in_ide`（退化 primary/首个）。
- 内置补充接口：`GET {snap}/v1/model/builtin`（`Agent-Type: PromptCenter`）→ `{builtinModels:[...]}`。
- 模型 ID 区分大小写；福利网关返回小写，`CanonicalModel` 做大小写不敏感归一。

### 本仓库的实现取舍

- **福利目录按账号存放**（`internal/upstream/models.go`）：福利是按账号授予的，
  池里 A 账号有、B 账号没有时，判定依据必须是**发起请求的那个账号**，不能全局共享；
  目录**整体替换**而非增补，套餐轮换后不留旧标记。
- **发现结果优先于冷启动种子**：目录里标了福利 → 带 `maas_type`；目录里有但没标
  （说明它以内置/agent-center 身份注册）→ 不带，福利模型转正后能自动摘掉该头；
  目录里查不到（未发现、福利来源失败或未领取）→ 回退种子带头，避免未注册模型 404。
- **按账号能力路由**（`internal/server/handler.go` `pickAccount`）：`/v1/models`
  展示的是各账号目录的并集，聊天会优先挑目录里真有该模型的账号，没有才退回普通轮转。
  否则混合池里没有该模型的账号会先吃一次 400，白耗一次 `MaxRotate`（只有 3 次，
  账号多于 3 个时可能永远轮不到有能力的那台），还会给健康账号记错误、到阈值冷却 10 分钟；
  会话黏性锁定的账号若不能服务当前模型，同样换号而不是硬发。
- **`maas_type` 由服务端注入并计入签名**：`SendChatV2` 在 `signRequest` 之前设置该头，
  SignedHeaders 覆盖它（与官方客户端行为一致）。
- **领取默认关闭**：`POST benefit/claim` 是对账号的写操作，因此默认只读不领取；
  需要领取时显式执行 `go run ./cmd/models -claim` 或打开 `benefit_auto_claim`。
- **福利来源失败时保留上一轮福利条目**：三路发现里福利网关可能临时不可用，此时若整体
  替换目录，非种子的福利模型会丢掉 `maas_type: benefit`，上游按未注册模型 404。因此
  `setAccountModels(keepBenefit=true)` 会保留上一轮成功发现的福利条目，只有内置来源
  明确登记了该模型（视为转正）才摘掉标记。
- 冷启动种子（`seedBenefitModels`）：首次发现前也要带福利头，少带一次就是一次 404。

## 8. 脱敏

本仓库不包含任何真实 token。`auths/`、`data/`、`config.json`、`.env` 均 gitignore。
