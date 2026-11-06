# 模型 ID 写法（TM.000010051「无权调用该模型」）排查记录

> 实测环境：2026-11-06，cn-north-4，单账号（个人账号 OAuth 授权产生的 IAM 用户）。

## 现象

`/v1/chat/completions` 直接返回：

```
codearts api code=502 http=502 path=/api/v2/chat/completions
msg=TM.000010051 您当前无权调用该模型，请联系企业管理员开通。
```

但**同一个账号在码道 CodeArts 官方客户端里能正常用同一批模型**。很容易据此判断成
「账号没开通模型/套餐」，实际不是。

## 根因：同一个账号下，模型注册写法不统一

| 上游认的写法 | 模型（实测） |
|---|---|
| 目录显示名（含大写，如 `GLM-5.2`） | GLM-5.2、Qwen3-VL-235B |
| 内部小写 ID（如 `deepseek-v4.1-flash`） | DeepSeek-V4.1-Flash、DeepSeek-V4.1-Flash-1017、GLM-5.3、GLM-5.3-Flash、Kimi-K3、Kimi-K3.1-Pro、OpenPangu-2.0-Pro、OpenPangu-2.0-Flash |

写法不匹配时上游返回的就是 `TM.000010051`（不是 404/参数错误），文案与真正的
「无权限」完全一致。码道客户端发的是内部小写 ID，所以它能用；代理原先按模型发现
返回的显示名原样透传，于是被拒。

对照实验（同账号、同一时刻，只改大小写）：

```
DeepSeek-V4.1-Flash  → TM.000010051        deepseek-v4.1-flash  → OK
GLM-5.3              → TM.000010051        glm-5.3              → OK
Kimi-K3              → TM.000010051        kimi-k3              → OK
OpenPangu-2.0-Pro    → TM.000010051        openpangu-2.0-pro    → OK
GLM-5.2              → OK                 glm-5.2              → TM.000010051（反向同理）
```

结论：**只能按模型逐个试，不能全局统一大小写**。`GLM-5.2` 转小写同样报错，
`deepseek-v4.1-flash` 转大写同样报错。

## 修复

1. `internal/upstream/client.go`
   - 新增`modelAliases` 别名表与 `SetModelAlias` / `ClearModelAlias` / `lookupModelAlias`；
   - `CanonicalModel` 先走原有的目录归一（`canonicalBase`），再查学习到的实际写法；
   - 新增 `ModelVariant(model)`：给出同一模型的另一种大小写写法。

2. `internal/server/handler.go`
   - 聊天请求遇到 `TM.000010051` 或 `InferHub.002002009 not registered` 时，
     自动换另一种写法**重试一次**，成功即记住映射（后续请求直接命中，无额外开销）；
   - 换写法仍失败则撤销学习（`ClearModelAlias`），再按「模型对该账号不可用」处理，
     不会把错误写法固化成永久别名；
   - 日志：`model X: 上游注册写法是 "xxx"，改用该写法重试 account=...`。

3. `internal/server/availability.go`
   - 后台可用性探测复用同一套兜底（`probeSend`），否则探测会把这类模型误判为
     「不可用」并从 `/v1/models` 摘掉。

4. 关联改动（同批）
   - `TM.000010051` 归类为「该模型对该账号不可用」：不冷却账号（此前落到
     `status>=500` 分支，一次试探就把账号冷却 10 分钟，整池变 `no_healthy_account`）；
   - 可用性结论落盘 `data/state.json.availability.json`，重启后不必重新逐模型探测
     （探测 20s/个且要求账号空闲 10 分钟）；
   - 模型发现「没有可探测账号」时不再写 5 分钟负缓存（原来会导致启动瞬间
     `/v1/models` 退化成静态兜底表）；
   - `智速模式` → `speed` 别名：目录把它当成独立 ID 返回，但它与 `speed` 是同一个
     路由模型（官方控制台展示名）。

## 验证

```bash
# 1) 逐个写法试打（只读）
go run ./cmd/modeltest -auth-dir ./auths -gap 12s "DeepSeek-V4.1-Flash" "deepseek-v4.1-flash"

# 2) 身份 / 积分活动 / 福利渠道 只读诊断
go run ./cmd/diag -auth-dir ./auths

# 3) 走服务
curl -s http://127.0.0.1:7866/v1/models -H "Authorization: Bearer $CA2A_API_KEY"
curl -s -X POST http://127.0.0.1:7866/v1/chat/completions \
  -H "Authorization: Bearer $CA2A_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"DeepSeek-V4.1-Flash","messages":[{"role":"user","content":"hi"}]}'
```

## 仍未开通（与写法无关）

- `deepseek-v4-flash-0731` / `deepseek-v4-pro-0813`：免费福利渠道未给该账号分组
  （`POST /api/v1/benefit/claim` → `error_code=4000 invalid channel, group not found`）；
- `GLM-5.2-ArkTS-SPARK`：两种写法均被拒，需要鸿蒙 ArkTS 专项授权。
