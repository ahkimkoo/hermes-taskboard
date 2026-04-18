# Hermes Task Board 需求与架构设计文档

> 版本：v0.1 · 草案
> 作者：Claude Code
> 目标读者：后端 / 前端 / 运维

---

## 1. 项目概述

### 1.1 背景
Hermes Agent 是一个可以运行工具、读写文件、执行终端命令、对话式交互的 AI Agent。团队希望以看板（Kanban）形式统一编排要交给 Hermes 执行的"任务"，可视化整个执行过程，并允许人工在执行/验证阶段与 Hermes 交互。

### 1.2 目标
- 提供基于 Web 的 Trello 风格任务看板。
- 将"任务定义"与"任务执行（尝试）"解耦，一个任务可以派发多次尝试，多次尝试可并发。
- 支持自动 / 手动触发、依赖编排、优先级、标签、指定 Agent。
- 实时可视化 Hermes 的思考、工具调用与输出流；允许用户在执行中对话，在验证阶段复核。
- 系统轻量：单一 Go 二进制 + SQLite + 本地文件，能稳定支撑数百个并存任务。

### 1.3 非目标
- 不做多租户与复杂权限体系（v1 默认单用户 / 可信 LAN 环境）。
- 不实现 Hermes Agent 本身，仅对接其暴露的 API。
- 不构建分布式任务调度（Jobs 在单机内调度即可）。

---

## 2. 名词表（Glossary）

| 术语 | 含义 |
|---|---|
| Task（任务） | 用户定义的需求描述，生命周期贯穿 6 个状态列。 |
| Attempt（尝试） | 将某个 Task 指派给一个 Hermes Agent 的一次执行过程。同一 Task 可以有多次且可并发的 Attempt。 |
| Session（会话） | Hermes 侧的一个命名 `conversation`，即一条长对话的持久身份。**一个 Attempt 严格对应一个 Session = 一个 conversation_id**，生命周期完全一致；验证阶段用户追问仍在同一 Session 内继续，不另开 Session。注意：Session 内每一"轮"用户输入在 Hermes 实现上会产生一个新的 `run_id`（因 Hermes Runs API 不支持向进行中的 run 追加消息）；这些 run 都归属同一 conversation，由本系统聚合为单一 Session 视图。 |
| Board（看板） | 6 列视图：`Draft` / `Plan` / `Execute` / `Verify` / `Done` / `Archive`。 |
| Trigger（触发方式） | `Auto`（进入 Plan 即由调度器拉起）或 `Manual`（用户点击"开始"按钮）。 |
| Hermes Server | 注册到系统中的一个 Hermes API Server 实例：`base_url` + `API_SERVER_KEY` + `models[]`（多个 profile，默认 `hermes-agent`）。可注册多个，其中一个被标为默认。 |
| Agent | 逻辑上 = `(hermes_server_id, model)` 组合。任务的"指定 Agent"即在下拉里选一个具体的 server+model；不指定则使用默认 server 的默认 model。 |

---

## 3. 与 Hermes 集成方式的选择

Hermes 官方提供两种接入方式：**ACP（Agent Client Protocol）** 与 **API Server（`hermes gateway`）**。

| 维度 | ACP | API Server |
|---|---|---|
| 传输 | stdio / JSON-RPC | HTTP + SSE（OpenAI 兼容） |
| 定位 | 编辑器（VS Code / Zed / JetBrains）集成 | "dashboards 和 thick clients 的流式后端" |
| 多会话并发 | 与宿主进程绑定，受编辑器生命周期约束 | 原生支持，`POST /v1/runs` 创建独立 run |
| 状态保持 | 仅进程运行期 | `previous_response_id` / 命名 conversation 持久化 |
| 事件流 | JSON-RPC notification | SSE `chat.completion.chunk` / Runs events |
| 鉴权 | 无（本地 stdio） | Bearer Token |
| 远程部署 | 不适合 | 适合（Host/Port/CORS 可配） |

**结论：采用 API Server，且以命名 `conversation` 作为 Session 的唯一持久身份，Responses API 为主、Runs API 为辅**。关键原因：
1. 看板要同时追踪**多个并发 Attempt**，需要进程外的、可独立寻址的会话——Responses API 的命名 `conversation` 提供了这个持久身份（服务器端自动维护全部历史，包括 tool calls 与结果）。
2. Go 后端通过 HTTP/SSE 消费比 stdio JSON-RPC 更自然，也便于把 Hermes 部署在另一台机器上。
3. `conversation` 参数（或 `previous_response_id`）天然映射到"一个 Attempt = 一个 Session 的长对话"；验证阶段追问只是在**同一 conversation 上发起新一轮 response**。

**Hermes API 关键事实（已确认）**：
- `/v1/chat/completions` 无状态，每次需带全部历史——不适合本场景。
- `/v1/responses` 有状态，通过 `conversation: "<name>"`（或 `previous_response_id`）在服务端维护完整历史；**"追加消息"实际上是在同一 conversation 上创建一次新的 response/run**。
- `/v1/runs` 与 `/v1/runs/{id}/events` 只管单次执行（run）的事件订阅，**不支持往运行中的 run 追加 user 消息**。run 是"本轮执行"的一次性句柄，一个 Session 生命周期内可能产生**多个 run**。

因此本系统的**Session ↔ Run 关系**为：
```
1 Attempt  ==  1 Session  ==  1 named conversation
                                   │
                                   ├── run_1  (首轮 system+user prompt，可能 tool calls 多步)
                                   ├── run_2  (用户首次追问 → 新 run，延续 conversation)
                                   ├── run_3  (用户再追问 → 新 run)
                                   └── …
```
"1 Attempt = 1 Session" 的语义**不变**；在实现层，一个 Session 内会按用户轮次产生多个 `run_id`，但只有一个 `conversation_id`。

**主要接入点（本系统使用）**：
- `POST /v1/responses { conversation: "<attempt_id>", input: "<user msg>", ... }` → **起一次新 response/run**；服务端自动从 conversation 继承上下文。返回 `run_id` / `response_id`。
- `GET /v1/runs/{run_id}/events`（SSE）→ 订阅该 run 的工具调用 / token delta / 生命周期事件。
- 一次 Attempt 内每收到用户输入就调用一次上面这对组合：先 `POST /v1/responses` 起 run，再订阅其 events。
- `GET /v1/models` → 下拉选择模型。
- `GET /health/detailed` → Server 健康检查。
- Cancel：先用 `/v1/runs/{run_id}/cancel`（若 Hermes 提供）；否则关闭 SSE 并在客户端侧标记 cancelled。

> Runs API 的精确 schema 文档未公开完整字段，**后端封装一层 `HermesClient` 接口**，把请求 / 响应结构收敛在一个文件里，方便字段回填和未来升级。

---

## 4. 功能需求

### 4.1 看板视图
- 6 列固定顺序：Draft → Plan → Execute → Verify → Done → Archive。
- 每张卡片显示：标题、优先级徽章（P1–P5 色阶）、标签 Chip、指派的 Agent、依赖数/未就绪依赖数、Attempt 数量徽章。
- 支持拖拽改列（带状态机校验，见 §4.4）。
- 支持按标签 / 优先级 / Agent / 关键字过滤；支持搜索标题与描述。
- 列内按优先级升序、其次按更新时间降序排序。

### 4.2 任务（Task）属性
| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | UUID | 主键 |
| `title` | string | 必填 |
| `description` | markdown | 存储于 `data/task/{id}.json`，非基础字段 |
| `priority` | enum P1–P5 | P1 最高 |
| `dependencies` | []task_id | 前置任务，必须全部处于 `Done` 才可进入 Execute |
| `trigger_mode` | enum `auto` / `manual` | Plan → Execute 自动或手动 |
| `preferred_server` | server_id / null | 不填用默认 Server |
| `preferred_model` | string / null | 不填用该 Server 的默认 model（一般为 `hermes-agent`） |
| `tags` | []string | 多标签 |
| `status` | enum | 当前所在列 |
| `created_at` / `updated_at` | timestamp | |

### 4.3 尝试（Attempt）属性
| 字段 | 说明 |
|---|---|
| `id` | UUID |
| `task_id` | 归属任务 |
| `server_id` | 本次使用的 Hermes Server |
| `model` | 本次使用的 model（profile 名；手动触发时可在"开始执行"前更改 server + model） |
| `state` | `queued` / `running` / `needs_input` / `completed` / `failed` / `cancelled` |
| `session` | 唯一一个 Hermes Session 引用：{ `conversation_id`（Session 稳定身份，贯穿整个 Attempt）, `runs[]`（按时间顺序产生的 run_id 列表，每一轮用户输入对应一个）, `current_run_id`（当前活跃 run，若有）, `latest_response_id` } |
| `started_at` / `ended_at` | |
| `summary` | Hermes 完成时的最后一条 assistant 消息摘要 |

> **并发**：同一 Task 可以同时存在多个 `running` Attempt（例如并行试两个 Agent 或两种 Prompt 策略）。
> **Agent 切换**：仅对"手动触发"任务允许在 Attempt 进入 `running` 之前更换 `server` / `model`，进入 `running` 之后不再允许（Session 已与具体 server 绑定）。

### 4.4 状态机

**迁移分两类**：
- **自动迁移**（由后端触发）：仅 `Plan → Execute`、`Execute → Verify`、`Verify → Execute` 三条。
- **手动迁移**（用户拖拽卡片）：**其它所有转移都由用户手动拖拽完成**，包括 `Draft → Plan`、`Verify → Done`、`Done → Archive`、`Archive ↔ 任意`，以及任何回拉/跳过。

```
              ┌──────────┐  拖拽       ┌──────────┐
              │  Draft   │ ──────────▶ │  Plan    │
              └──────────┘             └────┬─────┘
                                            │ deps 全部 Done
                                            │ && (auto 触发 自动创建 Attempt ||
                                            │     manual 触发 用户点 Start)
                                            ▼
┌──────────┐  拖拽                    ┌──────────┐
│ Archive  │◀── Done 拖拽  ◀──── Done │ Execute  │
└──────────┘                           └────┬─────┘
      ▲                                     │ 所有 Attempt 进入终态
      │ 拖拽                                │ (completed / failed / cancelled)
      │                                     ▼
      │                                ┌──────────┐  用户追问（原 Attempt 同 Session 内继续）
      │           拖拽                  │  Verify  │──────────────────────────────▶ 回到 Execute
      └─────── ◀── Done ────────── ◀── └──────────┘
```

- **Draft → Plan**：用户拖拽。
- **Plan → Execute**（自动，仅此一条迁移会"自动创建 Attempt"）：
  - `auto` 触发：调度器每 N 秒扫描一次，发现 `Plan` 且 `dependencies_all_done`，自动创建 Attempt 并把卡片移入 Execute。
  - `manual` 触发：用户点击卡片上的"Start"（可选 server + model），系统创建 Attempt 并自动移入 Execute。手动拖拽到 Execute 等价于点 Start。
- **Execute → Verify**（自动）：**当且仅当一张卡上所有 Attempt 都进入终态**（`completed` / `failed` / `cancelled`）时，卡片自动进入 Verify。任意 Attempt 仍在 `queued` / `running` / `needs_input` 的，卡片保持在 Execute。
- **Verify → Done**：**用户拖拽**。没有 "Accept" 按钮。
- **Verify → Execute**（自动）：用户在 Verify 视图中对**某个已完成的 Attempt**发追问——后端对该 Attempt 的 `conversation_id` 调用 `POST /v1/responses` 起一个**新 run**（延续同一 conversation，故 Session 不变），该 Attempt 重新进入 `running`（追加新的 `run_id` 到 `session.runs[]`），卡片自动回到 Execute。Attempt 不新开、Session 不新开，仅新增一个 run 句柄。
- **Done → Archive** / **任意 → Archive**：用户拖拽。
- **Archive → 彻底删除**：用户显式"Delete"，级联清除数据（见 §7.3）。

> UI 上拖拽等价于调用 `POST /api/tasks/{id}/transition`。非法拖拽（例如 Draft 直接拖到 Done）由后端状态机拒绝并回弹。

### 4.5 与 Hermes 的交互
- **执行视图（点卡片打开的侧栏 / Modal）**：
  - 左侧：Attempt 列表，显示状态、server+model、时间线。
  - 右侧：当前选中的 Attempt 的对话输出（按 Hermes SSE 事件渲染，内容持久化存放于 `data/attempt/{attempt_id}/events.ndjson`，是完整对话历史）：
    - Thought / Token delta（淡色、折叠）。
    - Tool call 展开卡片（tool name / args / output）。
    - Assistant 文本（Markdown）。
    - 人工输入提示（当 Hermes 请求输入时高亮）。
  - **分页策略**：打开视图时**默认加载末尾 5 条"消息"**（1 条消息 = 1 个 user/assistant turn 或 1 个 tool_call 轮次；事件流聚合后的逻辑单元），滚动到顶部触发"向上翻页"再多加载一段历史；运行中的 Attempt 继续通过 SSE 实时追加到底部。
  - 底部输入框：用户可随时发消息，实际调用 `POST /v1/responses { conversation: attempt_id, input }` —— 后端先等当前 run 空闲（或主动 cancel），再起新 run，追加到本 Session 的 `runs[]`。
  - 操作按钮：Stop（取消当前 run）、New Attempt（并开一个新 Attempt）、Switch server/model（仅 manual 且 Attempt 未进入 running）。
- **Hermes 请求审批**：ACP 里有的"审批流"在 API Server 模式下不自动暴露，由本系统在 tool_call 事件里识别 `approval_required` 自定义约定并弹确认框（v1 可先透传不拦截）。

### 4.6 动效（UI 动画）
- **执行中的卡片**：绿色流光边框（`conic-gradient` + `@keyframes rotate` 或 `box-shadow` 脉冲）。
- **需要人工输入**（Attempt state = `needs_input`）：橙色边框闪烁（`@keyframes flash`）。
- **全部完成**：一张卡上所有 Attempt 进入终态后，执行态动画消失，系统**自动**把卡片移到 Verify 列并触发一次"移动"动画。
- 卡片拖拽：基于原生 HTML5 DnD + 自定义占位视觉。

### 4.7 标签
- 标签独立一张表，支持增删改颜色；任务以多对多关联。
- 搜索框支持 `tag:backend` 这种 DSL，也支持简单关键字模糊匹配标题/描述。

### 4.8 系统设置

#### 4.8.1 Hermes Server 管理（多 server、多 model）
- 设置页提供 Server 列表的增 / 改 / 删；每个 Server 字段：
  - `id`（稳定 slug，用户可编辑，作为任务里的 `preferred_server` 值；系统生成后一般不改）
  - `name`（展示名，唯一）
  - `base_url`（例如 `http://127.0.0.1:8642`）
  - `api_key`（对应 Hermes 的 `API_SERVER_KEY`，以 Bearer Token 发送；`config.yaml` 存储时用 `APP_SECRET` AES-GCM 加密成 `api_key_enc`）
  - `is_default`（整个系统仅一个 default server）
  - `max_concurrent`（本 Server 整体最大并发 Attempt 数，默认 10）
  - `models[]`（本 Server 的可用 profiles）：每项字段
    - `name`（profile 名；`hermes-agent` 为 Hermes 默认）
    - `is_default`（该 Server 内默认 model，未指定时用它）
    - `max_concurrent`（本 (server, model) 组合的最大并发，**默认 5**）
- 页面上"Test Connection"按钮：调用 `GET /health/detailed` + `GET /v1/models` 校验 key 与当前可用模型；返回结果用于提醒"配置里声明的 model 是否在 server 上真实存在"。
- Models 由用户在配置里声明（含并发上限等策略），**不**自动同步 `GET /v1/models`；`GET /v1/models` 只作参考展示，点"Import"可一键把 server 尚未声明的 model 追加到 `models[]`（默认并发 5）。
- 删除 Server：若仍有任务 / attempt 引用它，先弹确认；删除后任务里的 `preferred_server` 清为 null（回落到默认 server）。

#### 4.8.2 并发限制层级
调度器和 Attempt 启动路径需依次检查下述三级闸门（全部满足才放行）：
1. **全局**：`scheduler.global_max_concurrent` —— 所有活跃 Attempt 总数上限（默认 50）。
2. **Server 级**：`hermes_servers[x].max_concurrent` —— 目标 server 上活跃 Attempt 上限（默认 10）。
3. **Profile 级**：`hermes_servers[x].models[y].max_concurrent` —— 目标 (server, model) 组合上活跃 Attempt 上限（**默认 5**）。

任一闸门不满足时：
- `auto` 触发任务：留在 Plan 列，等待下轮扫描。
- `manual` 触发 Start：返回 409 `{code: "concurrency_limit", level: "global"|"server"|"profile"}`，前端 toast 提示具体受限层级。

#### 4.8.3 全局参数（全部在 `data/config.yaml`）
- `scheduler.scan_interval_seconds`（默认 5）
- `scheduler.global_max_concurrent`（默认 50）
- `archive.auto_purge_days`（默认 30，0 = 从不自动清理）

#### 4.8.4 账号 / 登录（可选开启）
- 默认**关闭**：任何人访问 board web 都无需登录（适合本机/可信 LAN）。
- 在"设置 → 访问控制"页面可切换"启用账号密码验证"：
  - 启用时要求当场设置 `username` + `password`（至少 8 位）；
  - 确认后，凭据（username + bcrypt 密码哈希 + 一个随机生成的 `session_secret`）写入 `data/config.yaml`；
  - 开启后立即踢下线当前会话，跳转登录页。
- 启用后：未登录的请求 401（API）/ 跳转 `/login`（页面）。
- 登录成功后服务端发 HttpOnly / SameSite=Lax 的 cookie（JWT 或签名 session id，TTL 可配，默认 7 天）。
- 支持"修改密码"、"关闭账号验证"（需输入当前密码）。
- 仅允许**单用户**（v1 不做多用户、不做角色权限），保持简单。

#### 4.8.5 配置文件与热加载（**重要**）
**所有"系统设置"统一存在 `data/config.yaml`**，SQLite **不**再承担配置职责，只存业务数据（tasks、attempts、tags、deps）。

包含内容：
- `auth.*`（账号密码、session secret、TTL）
- `server.*`（监听地址、CORS）
- `hermes_servers[].*`（多 server、每个含 models 与并发上限）
- `scheduler.*`、`archive.*`（全局参数、开关）
- `preferences.*`（语言、声音）

**生命周期**：
- **启动**：进程启动时读取 yaml，解码成内存 `*Config`（`sync/atomic.Pointer[Config]` 以便无锁读取）。
  - 凭据字段（`auth.password_hash`、`hermes_servers[].api_key_enc`）在内存中保留加密形态，需要时按需解密，不明文落日志。
- **修改**：
  - 通过 API / 设置页修改任何配置项时，后端先原地更新**内存 config**，然后 `write tmp → fsync → rename` 原子覆写 yaml。
  - 失败回滚（写文件失败 → 还原内存）。
- **加载（热刷新）**：
  - 设置页"**从文件加载**"按钮 → `POST /api/config/reload`；后端重新读取 yaml、校验、替换内存指针，并执行"副作用同步"（见下）。
  - 用户也可直接编辑 `data/config.yaml` 后点这个按钮，不必重启。
- **热刷新副作用同步**（必须幂等）：
  - `hermes_servers` 变化 → `HermesPool.Reload()` 重建受影响的 client；在跑的 Attempt 不中断（它们持有老 client 引用直到该 run 结束）。
  - `auth.enabled` 从 true 改为 false → 使现有 cookie 立即失效？按配置决定；默认**保留**现有会话直至过期，避免自打嘴。
  - `session_secret` 变化 → 所有 cookie 立即失效（副作用必然）。
  - `scheduler.*` / `archive.*` / `*.max_concurrent` 变化 → 下一轮扫描即生效；**不**重启 goroutine。
  - `preferences.*` 变化 → 通过 `/api/stream/board` 推一条 `preferences_updated` 事件，前端拉新值刷 UI。
  - `server.listen` 变化 → 后端优雅 `Shutdown` 旧 `http.Server`、以新地址重建 listener（复用同一 handler mux）；失败则回滚到旧地址并返回错误。浏览器若因端口变化断链，由前端检测 SSE 断线后弹提示"端口已变更，请更新书签为 `<new>` 后刷新"。`server.cors_origins` 变化即刻生效（只影响 CORS 中间件快照）。
- **校验**：任何写入或 reload 时，都先用 schema 校验（字段类型、`max_concurrent ≥ 1`、`is_default` 唯一性、id 非空等），校验失败拒绝加载并返回详细错误；内存和文件维持原状。

### 4.9 国际化（i18n）
- **支持语言**：`zh-CN`（简体中文）、`en`（English）。v1 以这两种为目标，架构上可扩展。
- **默认**：首次打开读浏览器 `navigator.language`，命中 `zh*` 则用中文，否则 English。
- **切换**：顶栏语言切换按钮（🌐 `中 / EN`），立即全局生效。
- **持久化**：登录状态下写入该用户的 `preferences.language`；未登录状态写 `localStorage`。
- **资源文件**：`web/locales/zh-CN.json`、`web/locales/en.json`，结构为扁平 key → string。Vue 侧用一个极小的 `$t(key, params?)` helper（不引入 vue-i18n 以避免额外构建步骤）。
- **覆盖范围**：所有 UI 文案、列名、状态标签、按钮、toast、表单校验错误；**Hermes 产生的内容不翻译**（保持原样）。
- **日期/数字格式化**：用浏览器原生 `Intl.DateTimeFormat` / `Intl.NumberFormat`，跟随 `language`。
- **后端错误**：API 错误响应统一返回 `{ code, message_key, params }` 结构；前端按 code/message_key 在本地字典里渲染。

### 4.10 响应式 & PWA
- **响应式断点**：
  - `≥1200px`：完整 6 列并排；
  - `768–1199px`：3 列横排 + 横向滚动；列头贴顶；
  - `<768px`（手机）：一次展示**单列**，顶部一行状态 tab（Draft/Plan/…），左右滑手势切换列；卡片全宽展示；执行面板改全屏 modal。
- **交互**：
  - 拖拽：桌面保留 HTML5 DnD；移动端改为"长按卡片 → 弹底部 Action Sheet → 选目标列"，避开 iOS/Android 对 HTML5 DnD 的不一致。
  - 输入：执行面板输入框在手机上固定底部、随软键盘升起。
- **PWA**：
  - 提供 `web/manifest.webmanifest`：name、short_name、start_url、display=`standalone`、theme_color、icons（192/512 PNG + maskable）。
  - 提供 `web/sw.js`（Service Worker）：`app-shell` 缓存静态资源（index.html、js、css、locales、vue.global.js、icons）；API / SSE 请求**不缓存**（始终 network-first，失败时返回离线提示页面）。
  - 支持"添加到主屏幕"，iOS 上配合 `apple-touch-icon` meta；安装后以独立 App 模式运行。
  - 登录 cookie 保持跨启动可用，手机端打开直接进看板。
  - 离线使用**不在 v1 范围**（看板本质实时）；离线只保证壳子不白屏。

### 4.11 声音提醒
- **触发事件**（三类）：
  1. Task 进入 Execute（第一个 Attempt 真正开始 running 时）。
  2. 任意 Attempt 进入 `needs_input`。
  3. 任意 Attempt 达到终态（completed / failed）→ 由状态机触发"完成"提示音；若整张卡从而自动移入 Verify，不重复鸣响。
- **素材**：从免版权库（如 freesound.org / mixkit.co / notificationsounds.com，遵守 CC0 / CC-BY）搜集 3 段短音（均 ≤1s、≤30 KB OGG 或 MP3），置于 `web/assets/sounds/`：
  - `start.ogg`（轻柔"叮"）
  - `input.ogg`（上扬"嘟嘟"）
  - `done.ogg`（成功"叮咚"）
  下载时记录来源与许可证于 `web/assets/sounds/CREDITS.md`。
- **播放**：前端通过一个 `sound.js` 模块，监听 SSE `board` 通道的 `attempt.state_changed` 事件并按规则播放。使用 `HTMLAudioElement` 预加载；首次用户手势后解锁浏览器自动播放策略。
- **默认开启**；设置页"偏好设置 → 声音提醒"总开关 + 每类事件细分开关 + 音量（0–100%）。
- **持久化**：登录状态下进 `preferences.sound`（user 粒度），否则 `localStorage`。
- **移动端**：PWA 窗口打开时正常鸣响；后台 / 锁屏不保证（浏览器限制）。**不**使用系统 Web Push（v1 不做 Notification API 推送）。

---

## 5. 非功能需求

| 维度 | 目标 |
|---|---|
| 规模 | 数百个任务并存；≤50 个同时 running Attempt。 |
| 延迟 | 看板列表 API < 50ms（内存缓存命中）；事件流 < 200ms 转发延迟。 |
| 资源 | 空闲内存 < 150MB；单二进制启动。 |
| 可用性 | 任意 task.json / attempt 目录缺失不得导致系统崩溃，仅显示占位并记录 warning。 |
| 可恢复 | 进程重启后，`running` Attempt 自动尝试重连 Hermes 的 run_id（若仍存活），否则标记 `failed` 且原因为 `detached`。 |
| 可观测 | 结构化日志（zerolog/slog）+ `/healthz` + `/metrics`（可选 prometheus）。 |
| 响应式 | 支持 360px～2560px 宽度；移动端 Lighthouse PWA 评分 ≥ 90。 |
| i18n | 中英文切换不刷新页面；未翻译的 key 回退 `en` 字符串；所有新文案必须同时进 `zh-CN` 与 `en` 字典。 |
| 声音 | 加载失败静默降级，不阻塞 UI；总静音时事件仍正常触发动画与状态更新。 |

---

## 6. 架构设计

### 6.1 总体拓扑

```
┌─────────────────────┐        ┌────────────────────────────────┐
│   Browser (Vue 3)   │◀──SSE──│      Go Backend (single bin)    │
│  - Kanban view      │        │  ┌─────────────────────────┐   │
│  - Execute modal    │──HTTP─▶│  │  HTTP API (net/http +   │   │
└─────────────────────┘        │  │  chi/router)            │   │
                               │  └──────────┬──────────────┘   │
                               │             ▼                   │
                               │  ┌─────────────────────────┐   │
                               │  │  Board Service (state   │   │
                               │  │  machine, transitions)  │   │
                               │  └──────────┬──────────────┘   │
                               │     ┌───────┼───────┐           │
                               │     ▼       ▼       ▼           │
                               │  Store  Scheduler  AttemptMgr   │
                               │    │               │            │
                               │    │               ▼            │
                               │    │        ┌────────────┐      │
                               │    │        │ HermesClient│─HTTP▶ Hermes API Server
                               │    │        └────────────┘      │
                               │    ▼                            │
                               │  SQLite + FS (data/*)           │
                               └────────────────────────────────┘
```

### 6.2 后端模块划分（Go）

```
cmd/taskboard/main.go          入口：装配依赖、启动 HTTP、优雅关停
internal/config                 配置加载：启动期 env + 运行期 data/config.yaml（热重载）
internal/server                 HTTP 路由、中间件、静态资源、SSE Hub
internal/board                  状态机、合法转移、领域服务
internal/store
  ├── sqlite                   *sql.DB 封装（modernc.org/sqlite 纯 Go 驱动）
  └── fsstore                   data/task、data/attempt 的文件读写
internal/hermes                 HermesClient 池（按 server_id 路由；封装 runs/responses/models/health）
internal/attempt                AttemptRunner（拉起 run、消费 SSE、落盘、广播）
internal/scheduler              auto-trigger 扫描、依赖解析、节流
internal/sse                    服务端→浏览器的事件总线（按 task/attempt 订阅）
internal/auth                   账号密码登录、bcrypt、session cookie、中间件守卫（可开关）
internal/security               API 内部 token 中间件、AES-GCM 加密 hermes api_key
web/                            前端静态资源（嵌入 go:embed）
```

### 6.3 数据存储策略

#### 6.3.1 SQLite（`data/db/taskboard.db`）——仅业务基础信息
SQLite 里**不再存任何"系统设置"**（Hermes Servers、账号、开关、偏好全部迁到 `data/config.yaml`）。
```sql
-- 任务
CREATE TABLE tasks (
  id              TEXT PRIMARY KEY,
  title           TEXT NOT NULL,
  status          TEXT NOT NULL,                 -- draft/plan/execute/verify/done/archive
  priority        INTEGER NOT NULL,              -- 1..5
  trigger_mode    TEXT NOT NULL,                 -- auto/manual
  preferred_server TEXT,                         -- 对应 config.yaml 里 hermes_servers[].id，nullable
  preferred_model  TEXT,                         -- model/profile name, nullable
  created_at      INTEGER NOT NULL,              -- unix ms
  updated_at      INTEGER NOT NULL,
  -- 便于过滤，冗余少量摘要字段；详细 description 存 data/task/{id}.json
  description_excerpt TEXT
);
CREATE INDEX idx_tasks_status_priority ON tasks(status, priority, updated_at DESC);

-- 依赖
CREATE TABLE task_deps (
  task_id      TEXT NOT NULL,
  depends_on   TEXT NOT NULL,
  PRIMARY KEY (task_id, depends_on)
);

-- 标签
CREATE TABLE tags (
  name  TEXT PRIMARY KEY,
  color TEXT
);
CREATE TABLE task_tags (
  task_id TEXT NOT NULL,
  tag     TEXT NOT NULL,
  PRIMARY KEY (task_id, tag)
);

-- 尝试（索引级，详情仍在文件）
CREATE TABLE attempts (
  id         TEXT PRIMARY KEY,
  task_id    TEXT NOT NULL,
  server_id  TEXT NOT NULL,         -- 对应 config.yaml 里 hermes_servers[].id
  model      TEXT NOT NULL,         -- 本次使用的 model/profile
  state      TEXT NOT NULL,
  started_at INTEGER,
  ended_at   INTEGER
);
CREATE INDEX idx_attempts_task_state ON attempts(task_id, state);
CREATE INDEX idx_attempts_server_model_state ON attempts(server_id, model, state); -- 并发计数查询
```
> `preferred_server` / `server_id` 是"软外键"——Hermes Server 的增删以 yaml 为准，SQLite 不做级联。若引用的 server 已从 yaml 删除，读取时视为"未指定"回落到默认 server。

#### 6.3.2 文件存储（避开 SQLite 膨胀）
```
data/
  config.yaml               # 访问控制 + 少量人读/人编全局项
  db/taskboard.db
  task/
    {task_id}.json          # { id, description(markdown), attachments_meta, custom_prompt_prefix, ... }
  attempt/
    {attempt_id}/
      meta.json             # { server_id, model, session:{ conversation_id, runs:[run_id...], current_run_id, latest_response_id }, summary }
      events.ndjson         # 每行一条 Hermes SSE 事件（追加写）
      transcript.md         # 人可读归档（可选，完成时生成）
```

`data/config.yaml` 完整示例（**文件权限 0600**，由后端首次写入时创建）：
```yaml
auth:
  enabled: false                    # 默认关闭
  username: ""                      # 启用后写入
  password_hash: ""                 # bcrypt($2a$...)
  session_secret: ""                # 32 字节随机值 hex；用于签名 cookie / JWT
  session_ttl_hours: 168            # 7 天

server:                             # 本进程自身的监听配置
  listen: "0.0.0.0:1900"            # 默认端口 1900；127.0.0.1:1900 仅本机、0.0.0.0:1900 局域网可访问
  cors_origins: []

hermes_servers:                     # 已注册的 Hermes API Server 列表
  - id: "default"                   # 任务里 preferred_server 引用的稳定 slug
    name: "Local Hermes"
    base_url: "http://127.0.0.1:8642"
    api_key_enc: "base64(AES-GCM(...))"   # 加密后的 API_SERVER_KEY
    is_default: true
    max_concurrent: 10              # 本 Server 总并发上限（默认 10）
    models:
      - name: "hermes-agent"
        is_default: true
        max_concurrent: 5           # 每 profile 并发上限默认 5
      - name: "hermes-agent-gpt"
        max_concurrent: 5
  # 可继续追加更多 server；仅一项 is_default: true

scheduler:
  scan_interval_seconds: 5
  global_max_concurrent: 50         # 全局 Attempt 并发上限（默认 50）

archive:
  auto_purge_days: 30               # Archive 列卡片超过该天数后后台物理删除；0=从不

preferences:                        # 单用户偏好
  language: ""                      # 空串=跟随浏览器；否则 "zh-CN" / "en"
  sound:
    enabled: true
    volume: 0.7                     # 0.0 ~ 1.0
    events:
      execute_start: true
      needs_input:   true
      done:          true
```

**`internal/config` 职责**：
- 启动时 `Load(path)`：读文件 → 校验 schema → AES-GCM 预解密校验 → 存入 `atomic.Pointer[Config]`。
- 运行时所有只读访问通过 `cfg := configStore.Snapshot()` 拿到一个不可变快照，避免并发锁。
- 写路径：`Mutate(fn)` —— 复制一份 → `fn(copy)` 修改 → 校验 → 原子替换指针 → `persist()`（`tmp + fsync + rename`）→ 广播 `config_updated` 事件 → 执行副作用 hook（`HermesPool.Reload()` 等）。
- 热加载：`Reload()` —— 重读文件并走相同 validate + swap + hook 路径；失败时保留旧快照并返回错误。
- `session_secret` / `api_key_enc` 的轮换都由 Mutate 路径自然触发副作用（cookie 失效 / HermesPool 重建）。

**选型理由**：
- 事件流天然是**追加写**，NDJSON 比 SQLite blob 更便宜，也便于 `tail -f` 调试与按需流式读取。
- 任务描述是长文本且不参与过滤查询，放文件避免 SQLite 页膨胀。
- SQLite 专职索引 + 事务（状态机迁移必须事务），大小稳定在 MB 级。

#### 6.3.3 缺失数据的容错
- 读 `tasks` 行但 `data/task/{id}.json` 丢失 → 返回 `description=""` + `warning: missing_description`，不 500。
- 读 Attempt 但 `meta.json` 丢失 → 返回 `state="unknown"` + warning；`events.ndjson` 丢失 → 事件流为空但卡片仍可展示。
- 启动时执行 `integrity_check`：比对 DB 与 FS，打印 warning；**不**自动删除，防止误删用户资料。

### 6.4 实时事件流设计 & 持久化 & 断线重连

#### 6.4.1 数据留痕
- `data/attempt/{attempt_id}/events.ndjson` 是**该 Attempt 对应 Session 完整对话历史**的唯一可信源；**跨多个 run 的事件都追加到同一个文件**，构成逻辑上连续的 Session 事件流。后端在收到每一条 Hermes 事件（SSE 或 reconnect 后补拉回来的历史）时，**先**追加写入该文件，**再**推给 SSE Hub 广播给前端。
- 文件里除了原始 Hermes 事件外，还插入系统级标记行：`{"kind":"system","event":"run_start"|"run_end"|"connect"|"disconnect"|"reconnect"|"backfill","run_id":...,"ts":...,"cursor":...}`——特别是 `run_start` / `run_end` 划分每一轮用户交互的边界，便于前端按 run 折叠或按 Session 连续渲染。
- 每行 NDJSON 都带一个单调递增 `seq`（后端注入，uint64，**跨 run 全局单调**），既是文件内偏移的逻辑游标，也是 SSE `Last-Event-ID`。

#### 6.4.2 Hermes → Backend（AttemptRunner）
`AttemptRunner` 为每个 **active Attempt** 维护一个 goroutine；active 定义：`state ∈ {queued, running, needs_input}`。

**一轮（run）的生命周期**：
1. 新 run 起始：
   - **首轮**（Attempt 刚创建）：`POST /v1/responses { conversation: attempt_id, input: <初始系统/用户 prompt 组合>, model, stream: true }`，拿到 `run_id` 与 `response_id` 写入 `meta.json`。
   - **后续轮**（用户追问）：`POST /v1/responses { conversation: attempt_id, input: <user msg> }`，同样返回新的 `run_id`。
2. 写入一行 `{"kind":"system","event":"run_start","run_id":...}`。
3. 订阅 `GET /v1/runs/{run_id}/events`（若该 run 产生 `response_id` 前已可订阅）或读取 response 流；在每个事件上：
   a. `seq := seq+1`，落盘 `events.ndjson`（`O_APPEND` + fsync 批量）；
   b. 做轻量解析（识别 tool_call、需要输入、completion、失败）；
   c. 广播到 `sse.Hub` 的 topic `attempt:{id}`；
   d. 命中 Attempt / Task 状态变更时，调用 Board Service 事务写库并广播 `board`。
4. run 结束 → 写 `run_end` 标记，落盘更新 `meta.session.current_run_id=null` 和 `latest_response_id`。若 Attempt 还需要（有排队的用户输入），立即起下一轮；否则 Attempt 进入终态（completed/failed/cancelled）。

**用户消息排队**：
- 同一 Attempt 在当前 run 未结束时不允许并行起新 run（Hermes conversation 内顺序语义）。
- 用户点"发送"时：若当前有 run 在跑，排入 Attempt 级 FIFO 队列；UI 给出"当前正在执行，消息已排队"的提示。也可选择"中断并立即发送"——后端 cancel 当前 run 后起新 run。
2. **断线自愈**：
   - 任意网络错误（EOF、超时、5xx）视为断线，记录 `disconnect` 系统事件。
   - 采用**指数退避 + 抖动**重连（1s、2s、4s、8s…上限 30s）。
   - 重连目标是**当前活跃 run 的 `run_id`**（`meta.session.current_run_id`）。尝试：
     a. `GET /v1/runs/{run_id}/events?after=<seq>`（若 Hermes 支持游标回放），把断线期间事件补写 `events.ndjson`（标记 `backfill`）；
     b. 若游标不支持，则通过 Responses API 按 `conversation` + `previous_response_id` 拉取当前 response 的完整结果，与已落盘 events 作 diff 补齐。
   - 再进入正常流式订阅。
   - 重连期间 Attempt 状态保持不变（前端仍显示 running），只在 UI 顶部显示 "reconnecting…" 灰条。
3. **进程重启恢复**：启动时扫描 `attempts` 表中 `state IN ('queued','running','needs_input')` 的行，对每一个：
   - 读取 `meta.json` 的 `session.conversation_id` 与 `current_run_id`；
   - 若 `current_run_id` 仍在 Hermes 侧存活，重新订阅事件（走与断线重连相同的补齐逻辑）；
   - 若该 run 已过期 / 404：尝试通过 `conversation` 用 Responses API 拉最后一个 response 的状态；能补齐就续跑，无法恢复则把 Attempt 标 `failed` / `reason='detached'`，已落盘的 events 保留。
   - 若 Attempt 还有待处理的用户输入（见 FIFO 队列），在旧 run 恢复完成后按顺序继续下一轮。
4. 进程退出时不清理中间状态；落盘即是进度。

#### 6.4.3 Backend → Browser
- **两条 SSE 通道**：
  - `GET /api/stream/board`：任务状态/卡片级事件（move、create、delete、attempt_summary_update）。
  - `GET /api/stream/attempt/{id}?since_seq=N`：某个 Attempt 的详细事件流；`since_seq` 缺省为"最新"。前端进入卡片详情时才订阅，避免风暴。
- 浏览器 SSE 断线自动重连，用 `Last-Event-ID: <seq>` 告诉后端从哪恢复；后端直接从 `events.ndjson` seek 到 `seq+1` 继续推，不依赖内存 ring buffer。

### 6.5 调度器（Auto-trigger）
- 每 `scheduler.scan_interval_seconds`（默认 5s）扫一次 `status='plan' AND trigger_mode='auto'` 的任务，按优先级升序处理。
- 对每个任务，依次检查：
  1. 所有依赖处于 `done`；
  2. `COUNT(*) FROM attempts WHERE state IN ('queued','running','needs_input') < scheduler.global_max_concurrent`（全局）；
  3. `COUNT(*) ...AND server_id = ? < server.max_concurrent`（Server 级）；
  4. `COUNT(*) ...AND server_id = ? AND model = ? < model.max_concurrent`（Profile 级，默认 5）；
  5. 目标 Server 健康（缓存 30s 的 `/health/detailed` 结果）。
- 满足则创建 Attempt 并异步 `POST /v1/runs`；任一闸门不满足则跳过该任务留到下轮。
- Manual 触发路径（`POST /api/tasks/{id}/attempts`）走同一套并发检查函数 `CanStart(serverID, model)`，闸门不满足返回 409 + 受限层级。
- 并发计数使用 `idx_attempts_server_model_state` 索引，常数时间级别开销；读取配置上限用 `atomic.Pointer[Config]` 快照无锁。

### 6.6 并发与一致性
- 所有状态迁移都走 Board Service 的 `Transition(taskID, to, reason)`，内部 `BEGIN IMMEDIATE` 事务 + 合法性检查。
- `AttemptRunner` 与调度器只能通过 Board Service 改状态，禁止直接写 DB。
- SSE Hub 使用 ring buffer（每 topic 保留最后 N 条），浏览器断线重连时以 `Last-Event-ID` 回放。

---

## 7. 接口设计

### 7.1 REST（后端自身对外）
```
# --- 任务与尝试 ---
GET    /api/tasks?status=&tag=&q=&server=&model=       列表（基础字段）
POST   /api/tasks                                      创建
GET    /api/tasks/{id}                                 含 description
PATCH  /api/tasks/{id}                                 编辑属性
DELETE /api/tasks/{id}                                 彻底删除（级联清理）
POST   /api/tasks/{id}/transition  {to, reason}        状态迁移
POST   /api/tasks/{id}/attempts    {server_id?, model?} 创建尝试（Start）
GET    /api/attempts/{id}                              尝试详情（不含 events）
POST   /api/attempts/{id}/messages {text}              向 Hermes 追加用户消息
POST   /api/attempts/{id}/cancel                       取消
GET    /api/attempts/{id}/messages?tail=5              末尾 N 条"消息"（默认 5；打开卡片时首屏）
GET    /api/attempts/{id}/messages?before_seq=X&limit=20  向上翻页（取 seq < X 的最近 limit 条）
GET    /api/attempts/{id}/events?since_seq=N&limit=500  原始事件按 seq 正序读取（调试/完整导出用）
GET    /api/stream/board          (SSE)                看板级事件
GET    /api/stream/attempt/{id}?since_seq=N (SSE)      尝试级事件（Last-Event-ID 恢复）

# --- Hermes Servers 管理（写入 data/config.yaml，非 SQLite）---
GET    /api/servers                                    列出已注册 Hermes Server（api_key 不回显）
POST   /api/servers                                    新增 {id, name, base_url, api_key, is_default, max_concurrent, models[]}
PATCH  /api/servers/{id}                               修改（api_key 可省略则保留旧值；可改 max_concurrent、models[]）
DELETE /api/servers/{id}                               删除
POST   /api/servers/{id}/test                          测试连接（调 /health + /v1/models）
GET    /api/servers/{id}/models                        拉取该 server 当前可用 models（调 Hermes /v1/models 带缓存，用于 Import）

# --- 其它设置（全部落 data/config.yaml）---
GET    /api/tags · POST · DELETE                       标签管理（SQLite）
GET    /api/settings · PUT                             全局参数：scheduler.*、archive.*、server.listen、cors_origins
GET    /api/preferences · PUT                          用户偏好（language、声音开关&音量）

# --- 配置热加载 ---
POST   /api/config/reload                              从 data/config.yaml 重新加载配置到内存（供用户外部编辑后刷新用）
GET    /api/config                                     返回当前内存配置快照（脱敏：不含密钥 / 不含 session_secret）

# --- 访问控制 / 登录（账号验证功能）---
GET    /api/auth/status                                返回 { enabled, logged_in }
POST   /api/auth/enable   {username, password}         启用账号验证（仅当当前 disabled）
POST   /api/auth/disable  {password}                   关闭账号验证（校验当前密码）
POST   /api/auth/change   {old_password, new_password} 改密
POST   /api/auth/login    {username, password}         登录，Set-Cookie
POST   /api/auth/logout                                退出
GET    /healthz                                        无鉴权
```

鉴权规则（二选一，取决于 `auth.enabled`）：
- `auth.enabled = false`：所有 `/api/*` 默认放行；但 `/api/auth/enable` 永远允许（用于首次开启）。
- `auth.enabled = true`：除 `/api/auth/login`、`/api/auth/status`、`/healthz`、静态资源外，其它 `/api/*` 都要求 cookie；未登录返回 401。
- `/api/auth/enable` 在已启用状态下返回 409；避免二次覆盖凭据。

### 7.2 Hermes 调用封装（示意）
```go
// 每个 HermesServer 注册一个 HermesClient 实例，按 server_id 路由。
// 设计要点：一次 Attempt = 一个 conversation（Session），每轮用户输入对应一次 CreateResponse。
type HermesClient interface {
    // 在指定 conversation 上起一次新 response（= 新 run）。首轮也走这个入口。
    CreateResponse(ctx, req ResponseRequest) (runID, responseID string, err error)

    // 订阅某 run 的事件流；支持 after 游标做断线补齐。
    StreamEvents(ctx, runID string, after uint64, out chan<- Event) error

    // 获取 response 当前完整状态（用于 Runs events 游标不可用时的兜底补齐）。
    GetResponse(ctx, responseID string) (ResponseSnapshot, error)

    // 取消当前活跃 run（conversation 本身不取消，只取消本轮执行）。
    CancelRun(ctx, runID string) error

    Health(ctx) (HealthStatus, error)
    Models(ctx) ([]Model, error)
}

type ResponseRequest struct {
    Conversation string   // 固定取 attempt_id，作为 Session 的持久身份
    Model        string   // profile，默认 "hermes-agent"
    Input        string   // 本轮用户 / 系统输入
    SystemPrompt string   // 仅首轮传；后续轮由 conversation 自动继承
    Tools        []string // 可选白名单，默认放开 hermes 全集
    Stream       bool     // true：走 SSE
}

type HermesPool interface {
    Get(serverID string) (HermesClient, error)    // 按 server_id 获取 client（带 api_key）
    Default() (HermesClient, error)                // 默认 server
    Reload()                                       // server 增删改时重建客户端
}
```
- **首轮**：`CreateResponse` 带上 `SystemPrompt`（由看板注入任务上下文：标题 + description + 依赖摘要）与初始 `Input`。
- **后续轮（用户追问）**：只带 `Conversation` + `Input`，Hermes 服务端根据 conversation 自动继承历史（含工具调用与结果）。
- 每个 Client 在构造时把 `API_SERVER_KEY` 作为 `Authorization: Bearer <key>` 固定注入所有请求。

### 7.3 删除级联
删除 Task 时：
1. 事务内：删 `task_tags`、`task_deps`、`attempts`（仅 DB 行）、`tasks` 行；
2. 事务外 best-effort：`rm -rf data/task/{id}.json`、`rm -rf data/attempt/{aid}` （每个子 attempt）；
3. 对仍在 running 的 Attempt，`HermesClient.CancelRun` 异步触发；若失败记录到 "reaper" 队列后台重试。
4. 即便第 2、3 步有残留，DB 不再有引用，前端不会再看到。

---

## 8. 前端设计（Vue 3，无构建）

### 8.1 目录结构
```
web/
  index.html
  manifest.webmanifest    # PWA manifest
  sw.js                   # Service Worker（app-shell 缓存 + 离线回退）
  assets/
    vue.global.js         # Vue 3 生产版本（本地）
    app.css
    animations.css        # 绿色光环 / 橙色闪烁
    responsive.css        # 断点与移动端布局
    icons/                # PWA 图标：192、512、maskable、apple-touch-icon
    sounds/               # start.ogg / input.ogg / done.ogg + CREDITS.md
  locales/
    zh-CN.json
    en.json
  js/
    app.js                # createApp 入口；根据 /api/auth/status 决定渲染登录页或看板
    i18n.js               # $t(key, params) + 语言切换 + 持久化
    sound.js              # 预加载 & 按事件播放 & 音量控制 & 首次手势解锁
    pwa.js                # 注册 sw.js、监听 beforeinstallprompt、显示"添加到主屏幕"
    components/
      Login.js            # 登录页（账号验证启用后门面）
      Board.js
      Column.js
      TaskCard.js
      TaskModal.js
      AttemptPane.js
      EventStream.js      # 订阅 SSE、渲染工具调用/文本
      AgentSelect.js      # 级联：Hermes Server → Model
      TagInput.js
      LanguageSwitch.js   # 顶栏 🌐 中/EN
      SettingsServers.js  # 多 server CRUD + 测试连接 + 展示 /v1/models
      SettingsAuth.js     # 启用 / 关闭账号验证、改密
      SettingsPrefs.js    # 语言、声音开关/音量、每类事件细分
    stores/
      board.js            # 轻量 reactive store
      attempt.js
      auth.js             # 登录态、servers 缓存
      prefs.js            # language / sound 偏好（双写后端 + localStorage）
    api.js                # fetch 封装（自动带 cookie、401 跳登录）
    sse.js                # EventSource 封装 + 断线重连
  favicon.svg
```

### 8.2 约束
- 不使用 webpack/vite/rollup；浏览器原生 ES Module（`<script type="module">`）直接加载。
- Vue 使用 **Global Build**（`vue.global.js`）+ 手写 `defineComponent` / 运行时模板字符串，避开 `.vue` SFC 编译需求。
- CSS 手写，动画关键帧：
  ```css
  @keyframes glow-rotate { to { --a: 360deg; } }
  .card.executing { border: 2px solid transparent;
    background: linear-gradient(#1e1e2a,#1e1e2a) padding-box,
                conic-gradient(from var(--a,0), #00e676, #00c853, #00e676) border-box;
    animation: glow-rotate 2.5s linear infinite; }
  @keyframes flash { 0%,100%{box-shadow:0 0 0 0 #ff9800}50%{box-shadow:0 0 0 6px #ff980055} }
  .card.needs-input { border-color: #ff9800; animation: flash 1s ease-in-out infinite; }
  ```
- 拖拽：原生 `draggable="true"` + dragover/drop；落点校验由前端做一次乐观更新，后端 `POST /transition` 是最终裁决。桌面与平板用 DnD，手机（`<768px`）退化为"长按 → Action Sheet"。
- 顶栏 meta：`<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">`，适配刘海屏安全区。
- Service Worker：`app.js` 内调用 `navigator.serviceWorker.register('/sw.js')`；升级策略采用 "skipWaiting + Clients.claim"，前端检测到新版本时弹 toast 让用户刷新。

### 8.3 页面
1. 登录页（仅 `auth.enabled=true` 时出现；否则直接进主看板）。
2. 主看板（桌面 6 列并排 / 平板 3 列滚动 / 手机单列 + 顶部 tab）。
3. 任务详情抽屉（描述编辑、依赖、触发方式、Agent 下拉=server+model 级联、标签）。
4. 执行面板（Attempt 列表 + 对话输出（tail 5 + 向上翻页）+ 对话输入）。
5. 系统设置页（顶部提供 **「从文件重新加载配置」** 按钮 → `POST /api/config/reload`，以及「下载当前 config.yaml」与「查看快照 diff」的辅助动作）：
   - **Hermes Servers** —— 列表 CRUD、Test Connection、查看该 server 的 models、切换默认 server；每个 server 可编辑 `max_concurrent`；每个 model 行可编辑 `max_concurrent`（默认 5）与 `is_default`。
   - **全局并发** —— 设置 `scheduler.global_max_concurrent`、扫描间隔等。
   - **访问控制** —— 启用 / 关闭账号密码、修改密码。
   - **偏好设置** —— 语言（中文 / English）、声音提醒（总开关 + 分事件开关 + 音量）。
   - **归档** —— Archive 自动清理天数。
   - **标签管理**（仅标签仍走 SQLite）。

> 所有保存按钮的语义都是"更新内存 + 持久化到 `data/config.yaml`"。用户也可以直接 `vim data/config.yaml`，然后点顶部"从文件重新加载配置"按钮即可生效，无需重启。

---

## 9. 目录规划（仓库根）

```
/
├── cmd/taskboard/main.go
├── internal/...            (见 6.2)
├── web/                    (前端静态，go:embed 打包进二进制)
│   ├── index.html · sw.js · manifest.webmanifest
│   ├── assets/ (含 sounds/、icons/)
│   ├── locales/ (zh-CN.json、en.json)
│   └── js/ · components · stores
├── data/                   (运行期生成；.gitignore)
│   ├── config.yaml         (访问控制 & 少量全局配置)
│   ├── db/
│   ├── task/
│   └── attempt/
├── docs/
│   └── requirements.md     (本文件)
├── scripts/
├── go.mod
└── README.md
```

---

## 10. 性能与容量考虑

| 关注点 | 方案 |
|---|---|
| 看板首屏 | 仅读 SQLite 基础字段；`tasks.description` 留空，详情按需再拉。 |
| 列表分页 | 单列 > 100 条启用 `limit 50 + infinite scroll`。 |
| 热点缓存 | 后端内存里维护 `map[taskID]*TaskLite`，写路径同步失效；看板级 SSE 只推 diff。 |
| 事件流 I/O | 追加写 NDJSON；SSE Hub 每 topic 只保留最近 500 条，历史用 `GET /events?from=` 拉。 |
| SQLite | `PRAGMA journal_mode=WAL; synchronous=NORMAL; busy_timeout=5000;` |
| 大量 Attempt | 对 `attempts` 按 task+state 建索引；归档任务的 Attempt 数据不预加载。 |
| 清理 | Archive 超过 N 天后台任务批量物理删除；每次启动做一次 FS 与 DB 的 orphan 对账。 |

---

## 11. 安全与部署

### 11.1 访问控制（Board Web 登录）
- **默认关闭**：匿名即可访问，`auth.enabled=false`。
- 管理员在"设置 → 访问控制"启用账号密码：
  - `username`（唯一）+ `password`（≥8 位）；
  - 密码使用 `bcrypt`（cost=12）哈希后写入 `data/config.yaml`；
  - 同时生成 32 字节随机 `session_secret` 用于签名 cookie（或 JWT HS256）。
- 登录：`POST /api/auth/login` 校验通过后发 `HttpOnly; SameSite=Lax; Path=/` cookie，TTL 默认 7 天；过期自动跳登录页。
- 关闭账号验证：要求输入当前密码；关闭后清空 yaml 中的凭据字段，并撤销所有现有 cookie（轮换 `session_secret`）。
- 中间件逻辑：
  ```
  if config.Auth.Enabled && !isPublicPath(r) && !validCookie(r):
      if isAPI(r): return 401
      else:        return 302 /login
  ```

### 11.2 Hermes Server 凭据
- 多个 Server 各自存一份 `api_key`（Hermes 的 `API_SERVER_KEY`）。
- 写入 `data/config.yaml` 前用 AES-GCM 加密成 `api_key_enc`（字段名以 `_enc` 结尾）；密钥来自环境变量 `APP_SECRET`（首次启动若缺失则随机生成并写 `data/db/.secret`，权限 0600）。
- 读到内存后按需解密注入 `Authorization: Bearer <key>` 头；**不**再下发到前端。前端设置页对已存在的 server 只回显占位（`••••`），修改 api_key 需重新输入。
- 用户手动编辑 `config.yaml` 时可直接写 `api_key: "<plaintext>"`；`POST /api/config/reload` 时后端识别到明文字段会在落盘回写时自动加密并删除明文 key（即"读取时宽容、写出时加密"）。

### 11.3 进程 & 文件
- 默认监听 `0.0.0.0:1900`（`server.listen`）；生产环境若只在本机使用，建议改为 `127.0.0.1:1900`；若需局域网开放，建议同步启用账号验证 + 配置 CORS allowlist。
- 端口可在**配置中心页面**或直接编辑 `data/config.yaml` 的 `server.listen` 修改。
- `data/config.yaml` 权限 0600；`data/` 目录总体 0700。
- 日志不打印 Hermes `api_key`、登录密码、用户消息全文；仅记录长度与 hash 前缀（调试可开启 verbose）。
- 单二进制 + `data/` 目录；推荐 systemd service 启动。
- 备份建议：`sqlite3 .backup` + `rsync data/`（包含 `config.yaml`）。

---

## 12. 里程碑建议

1. **M1（骨架）**：Go HTTP + SQLite schema + 静态页面；任务 CRUD + 拖拽；不接 Hermes。
2. **M2（Hermes 接入）**：`HermesClient` + AttemptRunner + SSE 转发；单 Attempt 端到端。
3. **M3（完整状态机）**：依赖、自动触发、调度器、Verify 回退。
4. **M4（体验）**：动画、多并发 Attempt、Agent 切换、搜索 / 过滤 DSL。
5. **M5（稳态）**：备份、metric、重启重连、对账、压力测试 500 task × 20 并发。

---

## 13. 开放问题

1. ~~Hermes Runs API 是否对单个 run 支持"追加 user 消息"？~~ **已确认不支持**。结论已在 §3、§6.4.2、§7.2 落地：Attempt 内的持续对话统一走 Responses API + 命名 `conversation`，每轮输入产生一个新 `run_id`，Runs API 只负责订阅本轮事件流；Attempt↔Session（= conversation）的 1:1 关系不变。
2. 验证阶段追问时前端 UI 默认在"当前 Attempt"继续发消息（不新开 Attempt）；若用户想对比不同策略，可显式"New Attempt"另起一个 Attempt（对应新 conversation，新 Session）。
3. 是否需要人工审批 tool call（dangerous terminal）？v1 建议不拦截，仅在事件流中高亮；v2 可接 ACP 审批流。
4. 多用户/权限：v1 不做；需要时以 APP_TOKEN 按环境变量分发即可。
