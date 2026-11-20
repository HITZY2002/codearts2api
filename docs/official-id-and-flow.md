# 官方 CodeArts 桌面客户端：请求流程与消息 ID 算法（逆向确认）

分析对象：`C:\Users\LinZX\AppData\Local\Programs\CodeArts\resources\bin\agent-kernel.exe`
（102 MB，Rust，AMD64）。工具：IDA Professional 9.5 + Hex-Rays，分析副本位于
`%TEMP%\codearts-official-inspect`。以下函数地址均为该副本的镜像基址 0x140000000 下的 EA。

## 1. 桌面端整体流程

```
Electron UI (renderer)
  └─ POST /session/{sessionID}/prompt_async   ← 本地 agent-kernel HTTP
       headers: x-snap-traceid(每次随机 UUID), x-ot-client-type=desktop,
                x-ot-client-version, plugin-name=codearts_space, maas_type(福利模型) …
agent-kernel.exe
  ├─ 会话存储: session(ses_…) → message(msg_…) → part(prt_…)
  │    user message 一条/轮；assistant message 的 parentID 指向 user message
  ├─ Agent 循环: 一轮内多次调用上游推理（工具调用续请求不换用户消息）
  └─ POST https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions
       headers: user-msg-id(=本轮 user message ID), created-time, model-id,
                model-name, x-ot-session-id, x-ot-parent-session-id,
                x-ot-function, maas_type, client-ip, x-ot-client-type/version
```

- 上游 header 组装函数：`sub_141717F40`。`user-msg-id` 直接取请求上下文结构体
  偏移 +24 处的字符串字段（为空则发空值），不做逐请求生成。
- 积分查询（renderer `CreditUsageService`）：
  `GET {snapEngine}/v1/ops/credit/usage?type=msg&id={userMessageId}`，
  进行中会话取 assistant 消息的 `parentID`，历史会话取配对 user 消息 ID；
  另有 `type=session` 按会话查询。AK/SK 签名，`Agent-Type: PromptCenter`。
- **积分查询时机（renderer 源码确认，`out/renderer/assets/index-*.js`）**：
  `recordTurnStats(sessionId, finalStatus)` 只在 session 进入终态
  （`COMPLETED`/`CANCELED`/`FAILED`，由 `session.status`/`session.idle` 事件触发）
  时执行，对最后一条 assistant 消息调一次 `fetchCreditUsage`——即**整轮
  Agent 循环（含全部工具续请求）结束后查一次**，不是每次上游推理都查。
  仅积分计费用户（`isCreditPackage`）触发；历史会话加载时
  `fetchHistoryCreditUsage` 按每轮配对的 user 消息 ID 批量补查。
  上游按 `msg_id` 聚合计费（`requestCount` 累计），所以轮末一次查询即整轮总消耗。
  网关对齐：`finish_reason=tool_calls` 的请求跳过查询，最终 `stop` 的请求
  用该轮复用的 `msg_id` 查一次（`internal/server/handler.go`，响应写出后执行）。
- 官方实际记录可见同一 `msg_…` 出现在相隔十几分钟的两次请求中（同一轮的重试/续跑），
  证实 ID 绑定“用户消息/轮次”，与时间无关。

## 2. 消息 ID 生成算法（已反编译确认）

生成器：`sub_1421ECEA1`（核心），包装 thunk：

| 函数 | 前缀参数 | 用途 |
| --- | --- | --- |
| `sub_1421ECE95` | `"msg"` (0x1443B6A8B) | 消息 ID |
| `sub_1421ECE89` | `"prt"` (0x1443B6A88) | 消息部分 ID |

算法等价伪代码：

```text
ms = unix_millis(now)                     # 系统时间换算，减去 0001-01-01 纪元常数
state = 全局 { last_ms, seq12 }            # .data 0x145DC5EF0，进程内共享
if ms <= last_ms:
    seq += 1
    if seq >= 0xFFF:                      # 4095 用尽
        last_ms += 1; seq = 1             # 毫秒进位，保证单调
else:
    last_ms = ms; seq = 1
stamp = ((last_ms << 12) & 0xFFFFFFFFF000) | seq   # 48 bit = 36 bit 毫秒 + 12 bit 序号

uuid = uuidv4()                           # sub_142E6620C：16 字节，version/variant 位固定
suffix = [ ALPHABET[b % 62] for b in uuid[0:14] ]  # sub_1421EBE4A，逐字节 div 62 取余
id = prefix + "_" + hex12(stamp) + suffix # 12 位小写 hex + 14 位 base62
```

- 字母表：`0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz`
  （.rdata 0x1443B66F8）。映射为 `byte % 62`，存在模偏差，与官方一致。
- 随机源是 UUIDv4：第 6 字节固定 `0x4?`、第 8 字节固定 `10??????`，
  因此后缀第 6/8 位字符分布略有偏置——这是官方实现的固有特征。
- **尾部 `001`/`002` 是同一毫秒内的生成序号，不是客户端类型标记。**
  desktop 样本常见 `002`：kernel 在同一毫秒先生成另一个 ID（序号 1），
  user message 排第二。web HAR 样本为 `001`。序号动态变化，不应硬编码。

格式校验器：`sub_141FCB6E0`（session-bundle/src/rewrite.rs 引用）：

```text
strip_prefix("msg_") 后长度必须 == 26
前 12 字符 ∈ [0-9a-f]（小写 hex，大写 A-F 不合法）
后 14 字符 ∈ [0-9A-Za-z]
```

完整 ID 共 30 字符。

## 3. 复用粒度（网关实现依据）

官方 kernel 有持久会话存储，一轮 prompt 只创建一条 user message，
Agent 循环内所有推理请求共享其 ID。本网关是无状态 OpenAI 兼容层，
没有 kernel 的会话存储，因此用“会话 ID + 截至最近一条 user 消息的指纹”
近似轮次，并按账号隔离：

- 同账号 + 同轮次（工具续请求、排队重试、401 刷新、模型大小写重试）→ 复用 `user-msg-id`
- 换账号 → 该账号自己的新 ID（积分按账号身份归属，不能跨账号复用）
- 新用户提问 → 新 ID
- 客户端未传会话标识时，仅靠消息指纹区分；同账号完全相同的首轮消息可能被视为同一轮

实现位置：

- `internal/upstream/credit_usage.go` `NewUpstreamMsgID`：雪花时钟 + UUIDv4 后缀
- `internal/server/request.go` `upstreamTurnKey`：轮次指纹
- `internal/server/handler.go` `upstreamMsgIDFor`：账号×轮次缓存，随 ConvStateFile 持久化

## 4. 与 Web HAR 的区别

Web 端（`/chat/codebaseservice/v1/cloudagent/sessions/{id}/messages`）是另一套模型：
`X-Turn-Id: {sessionId}-N`、数字 `message_id`，积分字段 `credit_cost_for_web.id`
才是 `msg_…`。桌面 kernel 的 ses/msg/prt 模型不能与 Web turn 模型混用。

## 5. 逆向工具备注

- IDA 9.5 的 Python `decode_insn`/`get_operand_value` 对 rip-relative 与 call
  目标存在系统性 -0x10 偏移；扫描引用时需按原始字节（E8/E9 位移、lea ModRM）
  校正，或匹配 `val+0x10`。字符串列表不含 Rust 非空终止字符串，需按字节搜索。
- 分析脚本与结果：`%TEMP%\codearts-official-inspect\ida_msg*.py / *_result.json`。

## 6. 上游请求字段对比（官方 kernel vs 本网关）

依据：header 组装函数 `sub_141717F40` 与 LLM 发送函数 `sub_141702B95`
（crates\llm\adapters，InferHub/CustomOpenAI）引用的全部字符串常量，
以及对整个二进制的字节级搜索。

### 官方有、我们没有

| 字段 | 位置 | 说明 |
| --- | --- | --- |
| `created-time` | header | 请求创建时间，格式化当前时间 |
| `model-id` | header | 模型 ID（body 里也有 model） |
| `model-name` | header | 模型显示名 |
| `x-ot-client-version` | header | 客户端版本号 |
| `x-ot-session-id` | header | 会话 ID（我们发的是自造的 `Session-Id`） |
| `x-ot-trace-id` / `x-ot-span-id` | header | OTel 链路追踪，逐请求 |
| `x-ot-parent-session-id` / `x-ot-function` | header | 子 agent 场景，可选 |
| `user-session-id` | header | 可选 |
| `client-ip` | header | 可选 |
| `enable_thinking` | body | InferHub adapter 与 `tool_stream` 并列写入；fast-model 请求会被重写 |
| `<system-reminder>` 处理 | 行为 | header 函数内置正则 `(?s)<\?system-reminder>.*?</\?system-reminder>`，官方会处理消息中的 system-reminder 标签 |

### 我们有、官方没有（已删除，整个二进制搜不到对应字符串）

以下字段已于 2026-11-20 从上游请求中移除（`ChatHeadersV2` / `chatBodyV2` /
`sendChatV2`），测试改为断言它们不再出现。网关侧的会话亲和、
`X-Codearts-Chat-Id` 响应头与日志 `chat_id` 保留，只影响本网关，不上游。

| 字段 | 位置 | 证据 |
| --- | --- | --- |
| `Chat-Id` | header（已删） | 二进制无此字符串 |
| `Session-Id` | header（已删） | 仅出现在 `Mcp-Session-Id`（MCP 协议） |
| `is_confidential: false` | header（已删） | 二进制无此字符串 |
| `app-id: CodeAgent3.0` | header（已删） | `CodeAgent` 子串零命中 |
| `X-Language` | header（聊天，已删） | 仅 RagAgent 组件用 `x-language`，聊天 adapter 不带；积分/模型目录接口仍按官方保留 |
| `chat_id` | body（已删） | 全部命中都在本地消息/IM 模型，LLM adapter 不引用 |
| `prompt_cache_key` | body（已删） | 零命中；官方只有响应侧 `prompt_cache_hit_tokens`/`prompt_cache_miss_tokens` |

### 双方一致

`x-auth-token`、SDK-HMAC-SHA256 `Authorization`、`Content-Type: application/json`、
`Accept: text/event-stream`、`x-snap-traceid`、`x-ot-client-type: desktop`、
`user-msg-id`、`maas_type: benefit`（福利模型）、body 的
`model`/`stream`/`messages`/`tools`/`tool_choice`/`temperature`/`top_p`/`max_tokens`/`reasoning_effort`/`tool_stream`。

注：字符串零命中是强证据但非绝对（理论上可运行时拼接）；上表“官方没有”各项
连子串都不存在，运行时构造的可能性极低。

### 对齐进度（2026-11-20）

已补齐：

- `x-ot-session-id`：`upstreamSessionID(accountUID, chatID)` =
  `sha256("codearts-session:" + 账号UID + ":" + chatID)` 前 16 字节 hex，
  签名前写入。同账号+同会话稳定，换账号即换 ID（官方会话↔账号 1:1）。
- `model-id` / `model-name`：取 body 的 canonical model；官方 model-name
  缺省即回退 model-id，网关无显示名目录，同值就是官方回退行为。
- `x-ot-client-version`：`0.2.9`（官方 app.asar 内 package.json version）。

明确不做（用户决定）：`created-time`、body `enable_thinking`。
其余可选头（`x-ot-trace-id`/`x-ot-span-id`/`user-session-id`/`client-ip`/
`x-ot-parent-session-id`/`x-ot-function`）属遥测/子 agent 场景，暂不补。

## 7. 多账号 × 多会话的分配与匹配

原项目（修改前基线）：黏性 `convAcct[chatID]→账号`（裸 map 无 TTL）+
`PickExcluding` 配置序第一个健康账号 + 每账号槽位排队 180s 溢出 + 错误轮转。
新会话全部挤向第一个账号（填满-溢出），且 `chat_id`/`prompt_cache_key` 不区分账号。

现在：

- **挑号**：`PickExcluding` 改为健康账号中并发利用率
  （`activeConcurrent/maxConcurrent`）最低优先，同利用率保持配置序。
  同时进来的新会话摊开到整个池；槽位排队与错误轮转保留兜底。
- **黏性**：保留（轮次语义与缓存亲和依赖它），绑定带 TTL 与上限。
- **上游身份**：`user-msg-id` 按账号×轮次复用；`x-ot-session-id` 按账号×会话
  派生；换账号 = 新会话新消息 ID，历史靠完整 messages 重放。
- **单网关 key**（用户决定保持）：没有来源维度，`conversation_id` 即会话命名
  空间。客户端必须保证会话 ID 唯一，或回传响应头 `X-Codearts-Chat-Id`；
  两个客户端用同一个 `conversation_id` 会共享黏性与轮次 ID（积分合并），
  这是单 key 部署的固有约束，文档化而非代码隔离。
- 服务端匹配依据不变：计费 `(签名账号, user-msg-id)`，亲和 `x-ot-session-id`，
  缓存按内容前缀（官方请求侧无任何缓存键字段）。
