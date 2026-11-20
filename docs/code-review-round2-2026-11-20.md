# codearts2api 复检报告（第二轮）

- **复检日期**：2026-11-20
- **复检对象**：工作区未提交修改（36 个文件变更 / 新增 8 个文件），相对上一轮报告后应用的全部修复
- **复检方式**：全量重读关键模块 + `go build ./...` + `go vet ./...` + `go test ./...` + `go test -race ./...` + `gofmt -l` + 针对新可疑点编写临时 race 测试复现（验证后已删除，未残留改动）
- **上一轮报告**：`docs/code-review-2026-11-19.md`

---

## 一、总体结论

| 项 | 结论 |
|---|---|
| 原报告高严重度 5 项 | **全部修复**，且有针对性回归测试 |
| 原报告中严重度 6 项 | **全部修复** |
| 原报告低严重度 6 项 | 5 项已修复，1 项部分修复（死代码清理残留 1 个方法） |
| 构建 / vet | 通过 |
| `go test ./...` | 多数通过，存在偶发失败（测试时序敏感，非逻辑错误） |
| `go test -race ./...` | **全部通过，无 DATA RACE** |
| 修复是否引入新问题 | **是：1 个高严重度并发缺陷（已用 `-race` 实证）+ 若干中低问题**，见第四节 |

> 一句话结论：上一轮报告的问题修复质量高、覆盖到位；但**新增的设置热更新机制引入了 `h.cfg` 无同步读写的数据竞争**，属于必须处理的回归。

---

## 二、原报告问题逐项复核

### 高严重度（全部已修复）

| # | 原问题 | 状态 | 复核依据 |
|---|---|---|---|
| 1 | `saveChats` 无锁序列化 map，可致进程崩溃 | ✅ 已修复 | `handler.go:1410 storeConversation` 在 `convMu` 内完成「更新 + 序列化 + 写盘」（`saveConversationsLocked`，`handler.go:1494`）；新增 `stability_test.go:17 TestConversationStateConcurrentSaveAndReload`；`go test -race ./...` 通过 |
| 2 | `pickAccount` 未接入聊天主路径 | ✅ 已修复 | `handler.go` 两处 `Pool.PickExcluding` 已替换为 `h.pickAccount(tried, model)`；新增 `TestChatCompletionsUsesModelCapabilityRouting`（4 账号、仅 `/d` 有目标模型，断言只请求 `/d`） |
| 3 | 非流式错误一刀切冷却 10 分钟 | ✅ 已修复 | 删除 `if readCompletion { Cooldown }` 分支，统一走 `handleUpstreamError`；新增 `UpstreamError.APIError()`（`sse.go`）复用 `embeddedSSEError` 分类；新增 `TestLateSSEErrorsUseConsistentAccountClassification`（401→disable、429→soft cooldown） |
| 4 | `convAcct` 不持久化、`chats` 是死数据 | ✅ 已修复 | 持久化结构改为 `conversationState{Conversations, Turns}`，带 7 天 TTL 与 4096 条上限（`pruneConversationsLocked`）；`chats` 已删除；重启重载由 `TestConversationStateConcurrentSaveAndReload` 覆盖 |
| 5 | 队列重试后并发锁双重释放 | ✅ 已修复 | 引入 `lockHeld` + `releaseLock` 幂等闭包（`handler.go:819-829`），重试重新获取失败时不再误放他人槽位；新增 `TestQueueReacquireFailureDoesNotReleaseAnotherRequest` 精确复现该场景并断言 `active_concurrent` |

### 中严重度（全部已修复）

| # | 原问题 | 状态 | 复核依据 |
|---|---|---|---|
| 6 | 配置项无效 / 面板硬编码 | ✅ 已修复 | `upstream.timeout_seconds` 经 `ToPoolConfig().UpstreamTimeout` 传入 `upstream.New` 与 pool 三处构造；scheduler 新增真实 `KeepaliveTick`（只读 `FetchCreditCampaigns`，成功后才推进心跳）；面板 `keepalive_window` 改为 `poolConfig.KeepaliveWindow.String()`；`keepalive_window` 增加启动期 `ParseDuration` 校验 |
| 7 | `Account.Auth` 指针替换竞争 | ✅ 已修复 | 新增 `AuthSnapshot()`（`pool.go:158`，持 `a.mu` 返回指针），聊天/校验/刷新/探测/签到全部改走它；`List()` 中残留的 `a.Auth` 读取位于 `a.mu` 保护区内，安全 |
| 8 | OAuth 会话状态非同步 + 双轨状态 | ✅ 已修复 | store 读写统一返回 `cloneOAuthSession`；`Done/Err` 改由 `complete/fail` 方法在 `s.mu` 内写；`h.logins/pendingLogin` 双轨已删除 |
| 9 | `/oauth/callback` 开放重定向 | ✅ 已修复 | `handler.go:259` 现在校验 `https`、`next.User==nil`、host 等于 `PortalHost`、`ticket_id` 存在且能命中年活跃会话，否则 400；新增 `TestOAuthDirectCallbackRejectsOpenRedirect` 并断言会话未被篡改 |
| 10 | `saveState` 无锁读切片 + tmp 竞争 | ✅ 已修复 | 新增 `stateMu` 串行化写盘，`saveState` 内部持 `p.mu` 取账号快照；`Accounts()` 改为返回切片副本；`Enable/ClearCooldown/Cooldown/NoteError/Disable` 已调整为「先解锁再落盘」避免自锁 |
| 11 | 单个凭证损坏导致服务无法启动 | ✅ 已修复 | `auth.LoadDir` 对不可读/解析失败的文件跳过并 `log.Printf` 告警；`loadState` 解析失败也会打日志而非静默丢弃 |

### 低严重度

| # | 原问题 | 状态 | 说明 |
|---|---|---|---|
| 12 | pool 约 250 行死代码 | ⚠️ 部分修复 | 已删除 `RefreshAllTokens/Acquire/Release/CheckKeepalive/SetKeepalive/GetConcurrentStats/TryAcquire/NeedsKeepalive/UpdateKeepalive/SetMaxConcurrent`；**`CheckAndRefreshToken`（`pool.go:691`）仍无调用者** |
| 13 | `Remaining()` 未知过期返回 24h | ✅ 已修复 | 改为返回 `0`，未知即视为过期，会触发刷新或禁用 |
| 14 | 首条 user 消息指纹碰撞 | ✅ 已修复 | 无显式会话 ID 时 `conversationIDFor` 返回空，handler 每轮生成随机 chatID；新增 `upstreamTurnKey`（最后一条 user 之前的前缀指纹）用于消息 ID 复用；`turn_id_test.go` 覆盖跨工具续接稳定性与跨会话隔离 |
| 15 | 流式快照与增量不一致 | ⚠️ 已改但需实测 | 现在非前缀扩展的快照会直接报 `invalid_snapshot` 并中断流（`sse.go:614`）；一致性问题解决，但若上游确实会重写已发内容，会从「静默不一致」变成「用户可见中断」，需真实流量验证；另见新问题 N7（O(n²)） |
| 16 | 测试盲区 / 脆弱性 | ⚠️ 部分修复 | 新增 5 个关键回归测试 + `settings/auth/turn_id` 测试，`-race` 全绿；但 `go test ./...` 仍偶发失败（见 N13） |
| 17 | 杂项（Docker root、adminDisable、AcquireLockWait 空等、randHex 静默、withAuth 放行） | ✅ 已修复 | Dockerfile 增加 `USER app`；`Disable` 返回 bool 且 admin 按 404 处理；`AcquireLockWait` 账号不存在立即返回；`randHex` 失败打日志；`withAuth` 在空 Key 时返回 503 |

---

## 三、验证数据

```
go build ./...          PASS
go vet ./...            PASS（无输出）
go test ./...           多数 PASS；偶发 FAIL（见 N13）

go test -race ./...     ALL PASS
  ok cmd/login / cmd/server / internal/auth / internal/pool
  ok internal/scheduler / internal/server / internal/telemetry / internal/upstream
  无 DATA RACE

gofmt -l cmd internal   20 个文件未格式化（见 N12）
```

---

## 四、新发现的问题

### N1（高）设置热更新与请求读取 `h.cfg` 存在数据竞争，可致进程崩溃

**位置**：`internal/server/settings.go:241-272`（`applySettings` 写 `h.cfg`）；读取方为 `handler.go` 聊天热路径（`h.cfg.DefaultModel`、`h.cfg.QueueRetryDelay`、`h.cfg.QueueMaxAttempts`）与 `admin.go:38`（`"watch": h.cfg.WatchInfo` JSON 序列化）。

**问题**：`POST /admin/api/settings` 在请求 goroutine 中直接改写 `h.cfg` 的字段与 `WatchInfo` map，而其他请求无任何同步地读取。已用临时 race 测试实证：

```
WARNING: DATA RACE
Write at 0x... by goroutine 13:
  codearts2api/internal/server.(*Handler).applySettings()
      internal/server/settings.go:242   // h.cfg.DefaultModel = ...
Previous read at 0x... by goroutine 9:
  ... handler.go 请求路径读取 h.cfg.DefaultModel
```

- `string`/`Duration` 字段是数据竞争（可能读到撕裂值）；
- **`h.cfg.WatchInfo` 是 map，并发读（JSON 序列化）+ 写（`applySettings` 内 `WatchInfo[...] = ...`）会触发 `concurrent map read and map write` —— fatal error，进程直接退出**，与上一轮 #1 同类风险。

现有测试未覆盖「并发改设置 + 请求」，因此 `-race` 全绿并不代表安全。

**建议**：把所有可变运行时配置收敛到 `SettingsStore`（已有锁），请求路径通过 `h.settings()` 读取快照；或引入 `atomic.Pointer[RuntimeConfig]` / `Handler.cfgMu RWMutex`。`WatchInfo` 改为快照生成，不要共享同一个 map。

---

### N2（中）非流式响应在写回前同步查询积分，最坏阻塞 10 秒

**位置**：`handler.go:971-981`（`fetchCreditUsage`）、`handler.go:1034`（非流式，在 `writeJSON` 之前调用）、`handler.go:1022`（流式，在 handler 返回前调用）。

**问题**：

- 使用 `context.Background()` + 固定 10s 超时，**不受客户端断开/请求 context 控制**；
- 生产环境 `CreditUsageEnabled()` 恒为 true（仅测试端点跳过），即**每个请求都会多打一次上游**；
- 非流式路径在 `writeJSON` 之前执行 → 正常增加一次 RTT（~100-500ms），异常最长 10s，直接抬高非流式 P99。

**建议**：改为异步 goroutine 写入 telemetry；或加配置开关/更短超时；或仅在 `include_usage` / 客户端显式请求时查询。

### N3（中）SSE 错误分类使用裸子串 `"401"`，可能误禁健康账号

**位置**：`internal/upstream/client.go:723`

```go
case strings.Contains(low, "401") || strings.Contains(low, "unauthorized") || strings.Contains(low, "未授权"):
```

任意错误文本中出现 `401`（例如 `trace id ...401...`、token 计数、URL 参数）都会被分类为 401，随后 `handleUpstreamError` 会 **`Disable` 账号**（需人工恢复）。建议改为精确匹配 `code == "401"`、`IAM.401` 前缀、`http=401` 等形式。

### N4（中）`data/settings.json` 静默覆盖 config.json / 环境变量（含文档缺失）

**位置**：`cmd/server/main.go:32-39`、`internal/server/settings.go:61-82`

**问题**：`OpenSettings` 一旦找到 settings.json 就**全量替换** fallback（不字段级合并），随后 `applySettingsToConfig` 写回 `cfg`。即：

- 首次启动（无 settings.json）时 config.json / env 正常生效；
- WebUI 保存一次设置后，值被固化到 settings.json，之后**修改 config.json 或 `CA2A_*` 环境变量将静默失效**；
- README 未更新该优先级，也无启动日志提示。

**建议**：README 说明优先级；启动时打印「settings.json 覆盖了哪些字段」；或改为「env/config 优先、settings 仅补空」的合并策略。

### N5（中低）设置热更新不完整，但 API 声称「已保存并生效」

**位置**：`internal/scheduler/scheduler.go:46`（`UpdateConfig`）、`scheduler.go:72-81`（`time.NewTicker` 只在启动时创建一次）

- 修改 `poll_minutes` / `keepalive_interval_minutes` 后，`s.cfg` 立即更新，但 **ticker 周期仍是启动时的旧值**；
- `Run()` 在 `Enabled=false` 时直接 `return`，运行中把 `watch_enabled` 从 false 改为 true **不会启动循环**；
- `settings.go:236` 却返回「设置已保存并生效」。

**建议**：用 `time.Ticker.Reset` 或在每轮循环中按当前配置计算下一次触发；`Enabled` 每轮判断；或接口文案改为「已保存，将在重启后完全生效」。

### N6（中低）会话状态在请求热路径持全局锁同步写盘

**位置**：`handler.go:1410 storeConversation`、`handler.go:1436 upstreamMsgIDFor` → `saveConversationsLocked`（`handler.go:1494`）

每个成功的显式会话请求（以及每个新 turn 首次生成消息 ID）都会在 `convMu` 内序列化**整个** `conversations+turns` map（上限各 4096 条）并写文件。高频请求下 `convMu` 与磁盘 I/O 会成为串行瓶颈，且放大了写盘失败的日志量。

**建议**：内存中更新 + `dirty` 标记，后台定时（如 1-5s）批量落盘；进程退出时 flush；或分片/按会话文件存储。

### N7（中低）流式快照校验每次复制全部累积内容（O(n²)）

**位置**：`internal/upstream/sse.go:611`

```go
beforeContent, beforeReason := content.String(), reason.Len()
```

每个 SSE 事件都将已累积内容完整复制为字符串；长回复（数十万字符）下总体为 O(n²)，CPU/GC 压力明显。建议：维持「已发送长度」并只对新增片段做校验，或用 `bytes.Buffer` + 前缀比较（仅需比较首字符/长度即可判定替换）。

同时该逻辑把「全文快照重写」判为 `invalid_snapshot` 错误，需用真实流量确认上游不会在中途重写已发送内容（原代码注释称实测存在全文快照）。

### N8（中低）Docker 非 root 与 bind mount 权限冲突

**位置**：`Dockerfile:8-16`、`docker-compose.yml:11-13`

镜像内已 `adduser app` 并 `USER app`，但 compose 把宿主 `./auths`、`./data` 挂载到 `/app/*`。若宿主目录由 Docker/root 创建（默认 root:root），容器内 `app` 用户**无法写入**，导致凭证保存、state.json、settings.json、SQLite 全部失败。

**建议**：compose 增加 `user: "1000:1000"` 并在宿主机 `chown`，或在 entrypoint 内以 root 修正权限后降权；README 部署章节同步说明。

### N9（低）协议头/body 大改，README 未同步

- 移除：`chat_id`、`prompt_cache_key`、`Chat-Id`、`Session-Id`、`X-Language`、`app-id`、`is_confidential`；
- 新增：`x-ot-client-type/version`、`user-msg-id`、`x-ot-session-id`、`model-id`、`model-name`；
- 无显式 `conversation_id` 时 chatID 每轮随机 → 上游 session 每轮不同，prompt cache 复用下降；
- README 仍写「多轮上下文按账号自动续接（chat_id 分组）」，且未提及 `data/settings.json`、积分查询、新协议头。`docs/official-id-and-flow.md` 有完整逆向记录，可作为更新 README 的依据。

### N10（低）启用 `http.ProxyFromEnvironment` 的行为变化

`internal/upstream/client.go:5383` 的 Transport 新增环境代理支持，`HTTP_PROXY/HTTPS_PROXY/NO_PROXY` 现在会影响所有上游请求（`signedGet` 的 `http.DefaultClient` 本已受影响）。部署环境若存在 socks/http 代理变量，连通性可能变化，建议在 README 注明。

### N11（低）`settings.json` 损坏会导致启动失败

`OpenSettings` 解析或校验失败即返回 error，`main.go:34` `log.Fatalf`。这与本轮刚为 `auths/` 实现的「跳过损坏文件」策略不一致。建议备份损坏文件、回退到 config/env 并告警。

### N12（低）仓库未强制 gofmt，本次改动也含未格式化文件

`gofmt -l cmd internal` 列出 20 个文件，其中包括本次修改的 `cmd/probe/main.go`（`map[string]any` 字面量缩进错乱）。CI（`.github/workflows/go.yml`）只跑 build/test/race/vet，**没有 gofmt 检查**。建议加 `test -z "$(gofmt -l cmd internal)"` 步骤。

### N13（低）`go test ./...` 偶发失败（测试时序敏感）

复检中出现：

- `internal/upstream: TestFetchCreditUsage` —— `Client.Timeout` 为 1s，包并行执行时超时；
- `internal/server` 曾出现 httptest dial 超时（沙箱/负载相关）。

单独运行均通过。`-race` 模式下变慢会放大该问题，CI 可能随机红灯。建议测试专用 client 超时放宽到 5s，或 CI 加 `-p 1`，或对时序断言留更大余量。

### N14（trivial）残留与细节

- `pool.CheckAndRefreshToken`（`pool.go:691`）无调用者；
- `settingsSnapshot` 类型与 `SettingsStore.updated` 字段从未被使用；
- `fallbackSettings`（`settings.go:162`）不读取 `keepalive_window`，直接构造 `Handler` 的路径会退回默认 10m；
- `applySettingsToConfig` 把保活窗口按分钟取整（`int(d.Minutes())`），"90s" 一类配置会丢失精度；
- `AuthSnapshot()` 可能返回 nil，但 `chatCompletions`/`Validate` 两处直接调用其方法（当前调用路径不会出现 nil，属防御性缺口）。

---

## 五、建议的下一步（按优先级）

| 优先级 | 事项 |
|---|---|
| P0 | **N1**：消除 `h.cfg` 与 `WatchInfo` 的无同步读写（快照 + atomic，或统一由 `SettingsStore` 提供只读快照） |
| P1 | N2（积分查询异步化/开关）、N3（401 精确匹配）、N4（settings 优先级文档与日志） |
| P2 | N5（热更新 ticker）、N6（会话状态异步落盘）、N7（快照校验性能与行为确认）、N8（容器权限） |
| P3 | N9~N14（文档同步、代理说明、settings 容错、gofmt CI、测试超时、死代码清理） |

---

*本报告仅做验证与结论输出；除用于复现 N1 的临时 race 测试（已删除）外，未对项目代码做任何修改。*