# Hermes Task Board · 操作手册

一个看板式的 Hermes Agent 调度器:把任务记下来 → 自动派发给 Hermes 跑 → 跟踪每一次执行 → 验收归档。

> 中文 | [English](./manual.en.md)

---

## 1. 看板与任务流转

界面顶部有六个列(手机上是 tab):

| 列名 | 含义 | 怎么进 / 怎么出 |
|---|---|---|
| **草稿** | 还没准备好执行的想法 / 笔记 | 新建任务默认进这里;拖到「计划」准备执行 |
| **计划** | 排队等执行 | `trigger_mode=auto` 的任务会被调度器自动转到「执行」;`manual` 的需要点「立即执行」 |
| **执行** | Hermes 正在跑 | Attempt 流式生成时;全部 attempt 终止后自动转「验证」 |
| **验证** | 跑完了等你确认 | 你看完输出,可以拖到「完成」;或在卡片里发新消息让 Hermes 继续(自动回流到「执行」) |
| **完成** | 已验收 | 完工后归档 |
| **归档** | 收起不再处理 | 任何列都能拖到这里 |

**手机端**:六个列以 tab 横排,只显示一列。**长按卡片拖到顶部 tab** 可以跨列移动。

---

## 2. 创建任务

点右上角 **「+ 新建任务」**(手机端是右下角悬浮的 `+`)。

字段说明:

- **标题** *(必填)* — 卡片上显示的简短描述
- **描述** *(可选)* — Markdown 格式,支持**多种文件**粘贴 / 拖拽 / 选择上传(需先在设置里配 Aliyun OSS,否则上传被禁):
  - 图片(`png/jpg/gif/webp/svg`)→ 插入 `![](url)`,在描述里直接渲染
  - 音频(`mp3/wav/m4a`)→ 插入 `[🎵 文件名](url)`
  - 视频(`mp4/mov/avi/webm`)→ 插入 `[🎬 文件名](url)`
  - 文档(`pdf/doc/docx/xls/xlsx/ppt/pptx/txt/md`)→ 插入 `[📄 文件名](url)`
  - 单文件大小上限 **50 MB**
- **优先级** P1–P5 — 仅做视觉颜色区分(红→灰),不参与调度
- **触发方式**:
  - `auto` — 任务进入「计划」列后,调度器自动派发(默认)
  - `manual` — 永远不自动跑,需要点卡片里的「立即执行」
- **Hermes Server** *(可选)* — 不填则用默认 server;多个 server 可在设置里管理
- **Model (agent profile)** *(可选)* — 同上,默认走 server 的默认 model;若 server 没配 model,自动 fallback 到 `hermes-agent`
- **标签** *(可选)* — 多个标签逗号分隔。每个标签可在设置里挂 **System Prompt**,跑这个任务时会拼接进去发给 Hermes
- **依赖** *(可选)* — 选其它任务作为前置;前置完成前调度器不会派发本任务

---

## 3. 跑一次任务(Attempt)

打开任务卡片,下方就是「执行记录」面板。

- 第一次:点 `▶ 立即执行` 启动一个 attempt
- 之后:点 `+ 再次执行` 再开一次(`auto` 任务在调度器满足条件时也会自己再开)

Attempt 跑起来后:

- **聊天气泡** 流式显示 Hermes 输出。每条消息右上角有 `⎘` 复制按钮,点了把 markdown 原文进剪贴板。
- **工具调用** 折叠成卡片,点头部展开看 args / output。
- **时间戳** 每条消息右下角灰字。
- **发送框**:Ctrl/⌘+Enter 发送(普通 Enter 是换行)。手机上自动多行,最多 6 行。
- **停止按钮**:两次点击确认,避免误停。
- **↓ 跳到底部**:右上角浮标,长对话时一键滚到底,自动隐藏。
- **↑ 加载更早**:消息流默认只拉最近 30 条,顶部链接翻旧。
- **🔄 刷新**:消息流底部,重连 Hermes 拉最新事件,免得为同步事件去发废话「continue」。

---

## 3.1 在任务消息里使用 Hermes 指令

任务卡片的聊天输入里（或者任务描述里）可以直接输入 Hermes 的 **slash 指令**，Hermes 会在进入 agent loop 之前先处理。适合"我想让 agent 停下 / 重试 / 换模型"这类微调，不用写一整段正文。

从 taskboard 常用的：

| 指令 | 作用 |
|---|---|
| `/stop` | 打断正在跑的 turn 并释放 session 锁。比直接关卡片干净 —— 正在执行的 tool call 会被优雅中止。 |
| `/reset`, `/new` | 开一个新 session —— 历史清掉，下一轮冷启动。 |
| `/status` | 打印当前 session 信息（轮数、token 数、模型）。 |
| `/retry` | 把上一条用户消息再发一次给 agent。回答跑偏时用。 |
| `/undo` | 丢掉最近一轮 user + assistant 对话。比 `/reset` 温和。 |
| `/compress` | 手动压缩会话上下文（token 快满了就用）。 |
| `/background` | 这轮在后台跑，结果不进 session 历史。 |
| `/btw` | 临时问一下（不影响主对话线）。 |
| `/model <name>` | 换模型（只影响这个 session）。 |
| `/fast`, `/reasoning`, `/verbose`, `/yolo` | 配置切换（这个 session 内有效）。 |
| `/help`, `/commands` | 看完整指令列表和说明。 |

所有指令都能通过 taskboard 用的 `POST /v1/responses` 路径生效 —— Hermes gateway 在 agent 启动前拦截处理。完整列表在任意正在跑的 Attempt 聊天里发 `/help` 就能看到。

> **taskboard 会给首轮加 `[tb-xxxxxxxx]` 前缀**，这样 Hermes 主机上 `hermes sessions list` 的 Preview 列就能一眼看出哪些 session 是 taskboard 派发的。

---

## 4. 标签与 System Prompt

设置 → **标签** 标签页,可以维护任务标签。每个标签除了名字 / 颜色之外可以加一段 **System Prompt**。

派发任务时,所有挂在该任务上的标签的 system_prompt 会被拼接,**作为 instructions(等价于 role=system)** 发给 Hermes。

例子:
- 标签 `企微通知` 的 prompt 是「任务结束后调用 https://qyapi.weixin.qq.com/... 推送 markdown 摘要」
- 任何打了这个标签的任务都自动获得这个行为,不需要在每个任务描述里重复写

---

## 5. 定时执行

任务卡片里有「定时执行」可折叠区域。展开后点 `+ 添加定时`,模式有:

- **每 N 分钟 / 每 N 小时** — 简单间隔
- **每天 HH:MM** — 每天定点
- **每周 [选星期] HH:MM** — 选一个或多个星期天 + 时间
- **每月 D 号 HH:MM** — 每月某一天
- **高级 (cron)** — 标准 5 字段 cron 表达式

后端**只存 cron**(robfig/cron/v3),前端把上面的友好选项转成对应的 cron 字符串。已存的 cron 也会反向显示成「每周一三五 09:00」这种人话。可以一个任务挂多条定时,各自独立开关。

定时触发会创建一次新的 Attempt(等同于点了「再次执行」),不论任务当前在哪一列。

---

## 6. 设置详解

右上角齿轮进入设置。每个 tab 的字段含义:

### 6.1 Hermes Servers

注册到 Hermes Gateway 的连接。

- **ID / 名称** — 内部标识符 + 显示名
- **Base URL** — 例如 `http://127.0.0.1:8642`(本机)或远程 IP
- **API Key** — Hermes API 鉴权,**保存时本地用 AEAD 加密**(`data/db/.secret` 是密钥)
- **作为默认 Server** — 任务没指定 server 时用这个
- **Server 级最大并发** — 这个 server 同时跑多少 attempt
- **Models (agent profile)**:server 下面的 agent profile 列表
  - **名称** — 必须和 Hermes 里的 agent profile 名字对应,默认 `hermes-agent`
  - **作为默认 model** — server 内部默认
  - **profile 级最大并发** — 这个 profile 同时跑多少 attempt(和 server 级是双重限制)

> 没配 model 时 taskboard 会自动 fallback 到 `"hermes-agent"`(Hermes 自带的默认 profile)。

### 6.2 标签

维护标签库。

- **名称** + **颜色**(色块用于卡片上的彩色 chip)
- **System Prompt** — 见上文 §4

### 6.3 调度器

- **扫描间隔(秒)** — 调度器多久扫一次「计划」列查可派发的任务,默认 5 秒
- **全局最大并发 Attempt 数** — 整个系统同时跑的 attempt 上限。Server 级和 profile 级在它之内再做切分

### 6.4 归档

- **自动清理天数** — `data/attempt/{id}/` 目录的 mtime 超过 N 天且 attempt 已不在 DB 里就清掉,默认 90 天

### 6.5 偏好

- **语言** — `zh-CN` / `en` 即时切换
- **主题** — 暗 / 亮
- **声音**(`enabled` + `volume` + 三个事件开关):
  - `execute_start` — 任务开始执行时播提示音
  - `needs_input` — Attempt 状态变成等输入时
  - `done` — Attempt 终结(完成/失败/取消)时

### 6.6 OSS(图片上传)

任务描述里贴图需要这个。Hermes 接收的是文本,所以图片必须放在 LLM 能 fetch 的公开 URL —— taskboard 本机不能托管,得走 Aliyun OSS。

- **启用 / 关闭**
- **Endpoint** — 比如 `oss-cn-shanghai.aliyuncs.com`
- **Bucket / AccessKey ID / Secret** — 阿里云子账号凭据
- **路径前缀** — 图片在 bucket 里的目录,比如 `taskboard/`
- **公网访问 base** — 拼成最终 URL 的前缀,比如 `https://你-cdn.com/taskboard/`

不启用 OSS 时,粘贴 / 拖拽图片会被 UI 拒绝。

### 6.7 访问控制与用户

登录是默认必经的。首次启动会自动创建默认管理员(`admin` / `admin123`),请立刻在 **设置 → 访问控制** 里改掉密码。万一忘记,停掉服务后执行 `taskboard --reset-admin` 即可把 admin 密码重置回 `admin123`(同时会解除禁用状态)。

**目录级别隔离。** 每个用户在 `data/{用户名}/` 下有自己独立的目录,存放密码、偏好、hermes servers、标签(在用户级 `config.yaml` 里)+ `db/taskboard.db`(任务/尝试/定时)+ `task/` 和 `attempt/` 目录。要彻底清理某个用户,停掉服务后 `rm -rf data/{用户名}/` 就行,不会在共享 DB 里留下残留行。

**没有跨用户视图。** 每个账号只能看到自己的看板。要切换到别人视角,退出登录用对方密码重新登入即可。管理员不支持假扮他人 —— 一次只服务一个身份,数据模型更简单。

**管理员 vs 普通用户。** 管理员看到的任务面板和普通用户一样(只看到自己的),不同的是多了几个专属面板:
- **用户管理** —— 添加、重置密码、禁用/启用、删除。
- **全局 / 调度** —— 扫描间隔、全局并发上限。
- **第三方集成**(OSS)—— 阿里云 OSS 凭据,用于图片上传。
- **归档** —— 自动清理天数。
- **从文件重新加载配置** —— 重新读取 `data/config.yaml` 和每个 `data/*/config.yaml`,不用重启进程。

普通用户只看到 **Hermes Servers**、**访问控制**(修改自己密码)、**偏好设置**、**标签**。可以新建 / 编辑 / 删除自己的标签和 Hermes server;勾选 **共享给其他用户** 后,其他人也能只读看到并使用。

**禁用账号。** 管理员在用户管理里点 **禁用** —— 系统会在 `data/{用户名}/disabled` 创建一个空哨兵文件。此后该用户现有 session 下一次请求就会收到 401,登录时提示 `account disabled`。点 **启用** 会删除该哨兵文件。

### 6.8 尝试(Attempts)

每一次「尝试」= 把任务派发给 Hermes 执行一次。在任务详情弹窗里:
- **停止**(二次确认)取消正在运行的尝试。
- 已终止的尝试右侧的 **✕** 会删除该尝试以及事件日志;运行中 / 等待输入的尝试必须先停止。
- 删除任务会级联删除该任务的全部尝试。

---

## 7. 一些手机端特别说明

- **拖卡片**:手指落到卡片上,移动 5px 就开始拖。其它列在 tab 里,把卡片拖到目标 tab 的位置,松手就过去
- **滚屏**:卡片身上 touch 被 drag 占用,滚屏要触摸卡片**两侧 18px 留白**或卡片之间的**14px 间隔**
- **PWA 安装**:浏览器菜单→「添加到主屏幕」即可,标题 `Taskboard`,有独立图标

---

## 8. 故障排查

**任务卡在「计划」列不执行**
- 检查 `trigger_mode` 是不是 `auto`(`manual` 的需要手点「立即执行」)
- 检查依赖是否都已 `done`
- 检查 server 的 base_url 通不通(设置里有「测试连接」)
- 看右下角调度器有没有报错日志

**Attempt 一直显示「执行中」但没新事件**
- taskboard 重启后,孤儿 attempt 启动时会重连 Hermes 的 SSE。如果 Hermes 那边 run 已经结束,attempt 会被标 failed
- 实在卡住,在卡片底部点 `🔄 刷新` 强制重连一次

**手机上看不到刚改的代码 / 行为**
- 静态资源 `Cache-Control: no-cache`,理论上每次都会刷
- 实在不行清浏览器缓存或长按刷新→「清除缓存并硬性重新加载」
- 看左下角版本号是不是新的

**任务标签的 system prompt 没生效**
- 跑出来的事件流里有一条 `— sent system prompt (N chars) —` 是 audit 标记,展开看 `instructions` 字段就能看到实际发出去的内容
- Hermes 会把它叠加在 agent profile 的 base prompt 上,模型理论上看到了,如果不照做就调整 prompt 措辞
