# 变更说明（相对已发布线上版本）

> 基线：GitHub 已推送的 `origin/main`（`ee754d8` "fix: recover model ID casing mismatches
> and preserve account availability (#11)"，即当前线上版本）。
> 范围：7 个未推送提交（`ec5bf27`…`107ebe4`）+ 当前工作区未提交变更。
> 整理日期：2026-11-22。

## 一、变更总览

| 提交 / 变更 | 类型 | 摘要 |
| --- | --- | --- |
| `ec5bf27` feat: observability dashboard and credit status | 新增 | SQLite 观测存储 + 数据看板；WebUI 由静态单页 panel.html 换成 Vue 3 工程（Naive UI + ECharts）；轮次积分消耗上报 |
| `536cf88` wip: official ID alignment, … settings page | 新增+修复 | WebUI 设置页与 `data/settings.json` 热更新；出站代理支持；账号池利用率选号；官方 ID/报文对齐（docs/official-id-and-flow.md）；OAuth 回调两阶段加固 |
| `f84d530` fix: review round 3 | 修复 | cfg/scheduler 数据竞争、轮次结束积分查询、401 token 匹配、状态异步落盘、SSE 增量合并 O(n) |
| `cd54761` chore | 清理 | README 过期会话描述、放宽测试 HTTP 超时、删除死代码 CheckAndRefreshToken |
| `18627df` deploy | 部署 | update-server.sh 兼容宝塔面板 Go 项目守护进程 |
| `45d6f3f` ui | 界面 | 日志/账号/事件表格自适应列宽（数字不换行、文本列省略号） |
| `107ebe4` fix: review round 4 | 修复 | 客户端取消不再处罚账号；SSE 错误保真（上游失败不再伪装成空内容成功）；OpenAI 多模态透传与规范对齐；向上游转发 `stream_options.include_usage` |
| 工作区（未提交） | 重构+新增 | WebUI 零第三方依赖重写（1.26MB → 161KB）；静态资源运行时 gzip + immutable 缓存；`/v1/models` 返回内容面板设置；`/v1/models` 统一全小写输出 |

## 二、依赖与构建产物

| 项 | 线上版本 | 当前 |
| --- | --- | --- |
| Go 依赖 | 零第三方依赖 | `modernc.org/sqlite v1.34.5`（纯 Go 实现，无 CGO，用于观测存储） |
| 前端依赖 | echarts、vue-echarts、naive-ui、lucide-vue-next、vue | 仅 vue（其余全部移除） |
| 前端产物体积 | 约 1.26MB（gzip 约 387KB） | 约 161KB（gzip 传输约 56KB） |
| WebUI 实现 | Vue 3 + Naive UI + ECharts（单文件 App.vue 约 1300 行） | 零依赖自绘组件库（components/ui 15 个基础组件 + pages 7 个页面 + store/api/utils 分层），SVG 手写图表 |
| Dockerfile / CI | — | 随观测存储同步更新（构建缓存、go vet/race 流水线） |

前端工程结构（新）：

```
web/ui/src/
  App.vue            壳（顶栏 + 侧边栏 + 路由切换，164 行）
  store.js           共享状态（一次 loadAll 并行拉取看板/日志/事件）
  api.js             鉴权与 fetch 封装（API Key 持久化 localStorage）
  utils.js           格式化工具
  styles/main.css    主题变量（亮/暗/跟随系统）
  components/        SideNav、DonutChart、TrendChart + components/ui 15 个基础组件
  pages/             Dashboard / Accounts / Logs / Events / Models / Settings / DetailDrawer
```

## 三、新增功能

### 1. SQLite 观测存储与看板（ec5bf27）

- 默认启用，落盘 `./data/observability.db`；只记录请求元数据（模型、状态、耗时、Token、账号 UID），**不记录 prompt、completion、Authorization、API Key**。
- 保留策略：按天数 / 每表行数 / 数据库体积三重上限，启动与轮询时自动清理。
- 环境变量：`CA2A_OBSERVABILITY_ENABLED`、`CA2A_OBSERVABILITY_SQLITE_PATH`、`CA2A_OBSERVABILITY_RETENTION_DAYS`、`CA2A_OBSERVABILITY_MAX_ROWS`、`CA2A_OBSERVABILITY_MAX_DB_BYTES`、`CA2A_OBSERVABILITY_SNAPSHOT_INTERVAL_SECONDS`。
- 面板新增看板页：请求量、成功率、首字延迟 p50/p95/p99、总耗时、Token 用量、账号健康、系统事件流。

### 2. 积分消耗上报与每日签到（ec5bf27 / f84d530）

- 聊天轮次结束从上游 `done.credit_cost_for_web` 读取积分消耗（抓包确认字段，不做二次推算）。
- 启动与 watchdog 轮询时自动领取每日签到积分与新用户积分；面板可单账号/全员签到；`credits_auto_claim: false` 或 `CA2A_CREDITS_AUTO_CLAIM=0` 关闭。
- 领取失败不影响推理账号（不冷却、不禁用）。

### 3. WebUI 设置页与 settings.json 热更新（536cf88 + 工作区）

- 可热更新：默认模型、单账号并发、排队重试、watchdog 开关与周期、保活参数、福利/签到自动领取，保存即生效（调度器周期重建），持久化到 `data/settings.json`。
- 优先级：`settings.json` > 环境变量 > `config.json`（改 config/env 不生效时先检查或删除该文件）。
- `/v1/models` 返回内容控制（工作区新增，写入同一 settings.json）：
  - `models_return_benefit`：是否列出限时福利模型（关闭后解决"列出了但调用必然失败"的误导；自动领取独立开关）；
  - `models_hidden`：逗号分隔隐藏指定模型 ID，连带隐藏其小写形式；
  - `models_return_alias`：是否返回小写别名条目（**注意：见"已知问题"第 1 条，当前实现下该开关实际不生效**）。
- 升级兼容：旧版 settings.json 缺少 `models_*` 字段时回退默认值，不会因布尔零值静默关闭。

### 4. 出站代理支持（536cf88）

config.json 支持为上游请求配置 HTTP(S) 代理（登录、刷新、聊天共用），适配服务器侧无法直连华为云的场景。

### 5. 账号池利用率选号（536cf88）

选号策略在原有的健康/冷却过滤基础上加入利用率因子，减少把并发都压到同一账号。

### 6. 静态资源 gzip 与长缓存（工作区）

- 服务端对 `/assets/*` 首次请求压缩一次后常驻内存回放（产物带内容哈希文件名，换版自动失效）。
- `Cache-Control: public, max-age=31536000, immutable`；`index.html` 不缓存，保证发版即生效。

## 四、行为变更（兼容性影响）

| 变更 | 影响 |
| --- | --- |
| `/v1/models` 只返回**全小写、去重后**的模型 ID（原：精确 ID + 小写别名双条目） | **破坏性**：依赖列表里精确大小写 ID（如 `GLM-5.2`）做展示/匹配的客户端看到的 ID 变为 `glm-5.2`。聊天不受影响——请求链路经 `CanonicalModel`/动态目录做大小写不敏感归一后再发上游 |
| 客户端断开/取消不再处罚账号 | 原：用户连点 3 次"停止"= 健康账号冷却 10 分钟；现：识别客户端取消，不计错误、不轮换 |
| 上游失败错误保真 | 原部分上游错误在流式下表现为"空内容成功"；现统一为 SSE error 事件（扁平 error_code、嵌套 `{"error":{...}}`、字符串错误均覆盖） |
| 多模态透传 | user 消息 content 数组中的 `text`/`image_url` 分片原样透传（`image_url` 对象整体保留、含 `detail`）；不支持的分片类型（如 `input_audio`）返回 400（原静默丢弃） |
| 流式 usage | 网关向上游转发 `stream_options.include_usage`（官方同款），流式/非流式优先透传真实 usage，上游未返回时按文本长度估算兜底 |
| 代码围栏剥离收窄 | 仅剥离 ```` ```markdown ```` / ```` ```json ```` 包裹（v1 文档形态）；其他语言围栏是模型内容，原样保留 |
| 计费头对齐 | 计费/追踪头族扩展 `x-ot-trace-id`、`x-ot-span-id`、`created-time`，对齐官方 agent-kernel |
| 剥离内容围栏/参数忽略 | `stop`/`frequency_penalty`/`presence_penalty`/`response_format`/`seed`/`logprobs`/`n` 静默忽略（恒单 choice）——README 已注明 |

## 五、缺陷修复（review round 3/4 及杂项）

- **数据竞争**：config/scheduler 读写竞争修复（`-race` 全绿）；
- **积分查询时机**：改为轮次结束查询，避免中途查询拿到 0；
- **401 token 匹配**：刷新后 token 与账号匹配逻辑修正；
- **状态落盘**：池状态异步批量落盘，降低每请求 IO；
- **SSE 增量合并**：delta 拼接从 O(n²) 降为 O(n)；
- **队列设置回退**：config.json 缺省的队列字段不再以零值进设置页（原表现：设置页显示 0，保存被范围校验拒绝）；
- **CanonicalModel 大小写兜底**：switch 归一改为大小写不敏感匹配；
- **测试稳定性**：放宽本地测试 HTTP 超时（Windows 回环偶发超时，见"已知问题"第 3 条）。

## 六、部署与运维

- `deploy/update-server.sh` 兼容宝塔面板 Go 项目守护进程（重启探测逻辑适配）；
- Docker 以非 root 用户 `app`（uid/gid 10001）运行，bind mount 需 `chown -R 10001:10001 auths data`；
- 观测库体积超限自动清理（retention/rows/db_bytes 三重上限）；
- API Key 必填：缺失时拒绝启动（不再回退公开占位 key）。

## 七、已知问题（本次梳理发现，建议推送前处理）

### 后端 / 文档

1. **`models_return_alias` 是死开关（P1）**
   工作区把 `modelEntries` 改为只输出全小写 ID 后，`filterModelEntries` 的别名判定
   （依赖"存在对应精确 ID 条目"）永远不会命中——`precise` 集合恒为空，
   关闭该开关不会有任何过滤效果。但 settings 结构、README 表格、设置页 UI 仍暴露该开关。
   `models_filter_test.go` 用混合大小写条目直接构造输入测纯函数，掩盖了与 `modelEntries`
   实际输出的脱节。建议：二选一——要么删除该设置项（推荐，"全小写"已是明确产品决策），
   要么恢复双条目输出。

2. **README 未同步 `/v1/models` 行为变更（P1，文档矛盾）**
   README「模型列表与限时福利」一节仍写"返回上游**精确模型 ID**（区分大小写，如
   `GLM-5.2`、`Qwen3-VL-235B`），同时为含大写的 ID 补一条小写别名"，与全小写新行为
   直接矛盾；同一节表格中 `models_return_alias` 行随问题 1 一并处理。

3. **`internal/server` 测试在 Windows 本地偶发失败（P2，非本次引入）**
   用例直连 `127.0.0.1` 真实 HTTP，整包并发运行时偶发
   `TestChatCompletionsParamInvalidDoesNotRotateOrCoolAccounts`、
   `TestChatCompletionsServerErrorsStillCoolAccounts`、`TestChatCompletionsUsage`
   因连接超时/时序失败（单跑 `-run` 稳定通过）。`cd54761` 已放宽过一轮超时，
   如需彻底解决可改 httptest.Client 超时注入或放慢断言等待。

4. **webui.go gzip 细节（P3）**
   `acceptsGzip` 不处理 `Accept-Encoding: gzip;q=0` 与 `*` 通配；非压缩响应未回
   `Vary: Accept-Encoding`，若用户在服务前再加一层缓存代理可能出现压缩态被缓存给
   不支持 gzip 的客户端。自用/内网无影响。

### 前端（本次重构引入）

5. **顶栏"账号在线"恒为 0/N（P1）** — `web/ui/src/App.vue:83` 按 `a.healthy` 过滤账号，
   但后端 `Pool.List()` 输出的账号对象没有 `healthy` 字段（聚合值在 `overview.stats.healthy`），
   导致只要池里有账号，顶栏永远显示"0/N 账号在线"并套警告样式。改读 `overview.stats.healthy`
   或按 `!disabled && !cooling` 计算。

6. **OAuth 轮询循环组件卸载后不停止（P1，泄漏）** — `pages/AccountsPage.vue` 的
   `pollAccountLogin` 用 while + sleep(2000) 轮询 `/admin/api/oauth/poll`，无
   onBeforeUnmount 清理、无 AbortController；授权等待期间切走页面后循环继续每 2 秒
   打后端，直到会话过期（404）才停。

7. **翻页竞态：页码与数据可能错位（P2）** — `store.js` 的 `loadAll` 无请求序号/
   AbortController，快速连点翻页时后发先至会把第 3 页数据显示在"页码 2"下；
   `UiDataTable` 的 loading 状态也未禁用分页按钮。

8. **看板"调用分布"口径被日志页筛选污染（P2）** — `DashboardPage` 的模型占比基于
   `store.logs`（日志页第 1 页、带日志筛选、每页条数），用户在日志页筛选/改条数后，
   看板占比的样本随之静默变化（可能只剩 20 条）。

9. **全员签到按钮无 loading、无结果反馈、可重复提交（P2）** — 批量签到在后端分批执行
   可能耗时数分钟，期间按钮仍可点击；`runAction` 丢弃响应，完成后无提示。

10. **批量签到抽屉"每页条数"选择器是死控件（P2）** — local 分页未接
    `onUpdatePageSize`，选择 50/100 无任何效果且显示与实际不一致。

11. **`ttft_p99_ms ?? '—'` 兜底永不生效（P3）** — 后端 int64 恒输出数字（无数据为 0），
    `??` 拦不到；无数据时显示"0 ms"而非占位符。同理延迟曲线对无首包的桶画成 0ms。

12. **零碎（P3）** — 甜甜圈弧长按 top-6 重算与列表百分比口径不一致；TrendChart tooltip
    对 null 输出 "NaN"；`UiInput` 同时开 clearable + showPassword 时两个尾随按钮重叠；
    ModelsPage 隐藏/恢复模型整份 POST settings 与设置页并发编辑会互相覆盖；
    `reloadDashboard` 切时间范围会顺带重置日志/事件页码。

## 附：提交清单（origin/main..HEAD）

```
107ebe4 fix: review round 4 - client-cancel accounting, SSE error fidelity, official & OpenAI alignment
45d6f3f ui: adaptive column widths for log/account/event tables (nowrap numbers, flexible ellipsis text columns)
18627df deploy: make update-server.sh compatible with BT panel Go project daemon
cd54761 chore: fix stale README session description, relax test HTTP timeouts, drop dead CheckAndRefreshToken
f84d530 fix: review round 3 - cfg/scheduler races, turn-end credit query, 401 token matching, async state flush, O(n) SSE deltas
536cf88 wip: official ID alignment, header cleanup, pool utilization picking, proxy support, settings page
ec5bf27 feat: add observability dashboard and credit status
```
