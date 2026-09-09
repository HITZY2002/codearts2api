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
| 当前用户 | GET /v1/current/user | AK/SK 签名 + X-Security-Token |
| 账号信息 | GET /v5/caller-identity（sts.cn-north-4） | AK/SK 签名 + X-Security-Token |

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

`CLIENT_ID` = 应用 `uri_scheme`（默认 `codearts`，实测以登录链接为准，可在 login 工具
用 `-client-id` 覆盖）。

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

## 8. 限时福利模型（免费套餐，2026-09-06 逆向自 CodeArtsSpace 0.1.18）

官方客户端模型 = 内置 + 限时福利两路合并（`ModelService.fetchAgentModels/listBuiltinModels` + `getFreeBenefitModels`）：

- 福利发现：`GET https://opengw.developer.huaweicloud.com/api/v1/gateway/config`
  （AK/SK 签名 + `X-Security-Token`，无 Agent-Type），返回
  `{error_code:"0000", result:{base_url, models:[{model_id, model_name, context_window, max_tokens}]}}`。
  实测模型 ID 全小写：`deepseek-v4-flash-0731`、`deepseek-v4-pro-0813`、`glm-5.3-flash`。
- 领取/余额（幂等，打开模型菜单即调用）：
  `POST /api/v1/benefit/claim {}`、`GET /api/v1/user/tokens/balance`（同 host 同签名）。
- 网关总开关：`GET {snap}/v1/benefit-gateway-config`（`Agent-Type: PromptCenter`）→ `{enabled}`。
- 聊天路由：**同一** `POST {snap}/api/v2/chat/completions`，福利模型需追加请求头
  `maas_type: benefit`（renderer 检测 `isFreeBenefit` 添加，经 kernel `KERNEL_LLM_FORWARD_HEADERS`
  转发）。无此头报 `InferHub.002002009.404 model is not registered`；有头正常流式。
- agent 选择：`useragents?offset=0&limit=100&is_primary_agent=true` 后取
  `agent_name=="CodeAgent" && alias.alias_zh_cn=="智能体" && show_in_ide`（退化 primary/首个）。
- 内置补充接口：`GET {snap}/v1/model/builtin`（`Agent-Type: PromptCenter`）→ `{builtinModels:[...]}`。
- 模型 ID 区分大小写；福利网关返回小写，`CanonicalModel` 做大小写不敏感归一。

## 9. 脱敏

本仓库不包含任何真实 token。`auths/`、`data/`、`config.json`、`.env` 均 gitignore。
