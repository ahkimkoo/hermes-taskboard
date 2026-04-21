# Hermes Task Board 要求・アーキテクチャ設計ドキュメント

> バージョン：v0.2
> 著者：Claude Code
> 対象読者：バックエンド / フロントエンド / 運用

## 改訂履歴

- **v0.2 (2026-04-18)** — 2つの重要な制約を強化（§4.8 システム設定）：
  1. **Hermes Server の同時実行数制限は2段階で設定可能であること**：1つはこのServer全体の同時実行数上限（デフォルト10）、もう1つは `(server, profile)` 次元の同時実行数上限（デフォルト **5**）。profileとはHermes側のagent/model、例えば `hermes-agent`。いずれのレベルでも上限超過時は新しいAttemptの開始を拒否する。
  2. **すべてのシステム設定は `data/config.yaml` に統一**：アカウントパスワード、登録済みHermes Servers、スケジューリング/アーカイブ/ Preferenceなどすべてのスイッチを含む。起動時にYAMLを読み込み → メモリにキャッシュ（`atomic.Pointer[Config]`）；すべての変更は**メモリを先に更新し、その後ファイルをアトミックに書き戻す**；設定ページには**「ファイルから設定を再読み込み」ボタン**を提供し、`POST /api/config/reload` に対応。ユーザーが直接 `vim data/config.yaml` で編集した後、プロセスを再起動せずにホットリフレッシュできるようにする。失敗時はロールバックして旧スナップショットを保持。
- **v0.1（初稿）** — 初版要件・アーキテクチャドラフト。

---

## 1. プロジェクト概要

### 1.1 背景
Hermes Agentは、ツールの実行、ファイルの読み書き、ターミナルコマンドの実行、対話式インタラクションが可能なAI Agentです。チームはHermesに実行させる「タスク」をカンバン（Kanban）形式で統一的にオーケストレーションし、実行プロセス全体を可視化し、実行/検証フェーズで人手によるHermesとのインタラクションを可能にすることを望んでいます。

### 1.2 目標
- WebベースのTrelloスタイルのタスクカンバンを提供する。
- 「タスク定義」と「タスク実行（試行）」を分離し、1つのタスクに複数の試行をディスパッチ可能とし、複数の試行を並列実行可能とする。
- 自動/手動トリガー、依存関係オーケストレーション、優先度、タグ、指定Agentをサポートする。
- Hermesの思考、ツール呼び出し、出力ストリームをリアルタイムで可視化；実行中の対話、検証フェーズでのレビューを許可する。
- システムは軽量：単一のGoバイナリ + SQLite + ローカルファイルで、数百の並存タスクを安定してサポート。

### 1.3 対象外
- マルチテナントや複雑な権限体系は構築しない（v1ではデフォルトでシングルユーザー/信頼されたLAN環境）。
- Hermes Agent自体は実装せず、公開されているAPIとのインテグレーションのみを行う。
- 分散タスクスケジューリングは構築しない（Jobsは単一マシン内でスケジューリングすればよい）。

---

## 2. 用語集（Glossary）

| 用語 | 意味 |
|---|---|
| Task（タスク） | ユーザーが定義した要件記述。6つの状態列にわたるライフサイクルを持つ。 |
| Attempt（試行） | 某个Taskを1つのHermes Agentに指派する1回の実行プロセス。同一Taskに複数のAttemptが存在可能で、並列実行も可能。 |
| Session（セッション） | Hermes側の名前付き`conversation`、すなわち1つの長い会話の永続的なID。**1つのAttemptは厳密に1つのSession = 1つのconversation_idに対応**し、ライフサイクルは完全に一致する；検証フェーズでユーザーが追質問しても同一Session内で継続され、新たなSessionは開かない。注：Session内の各「ラウンド」のユーザー入力は、Hermes実装上新しい `run_id` を生成する（Hermes Runs APIは進行中のrunへのメッセージ追加をサポートしないため）；これらのrunはすべて同一conversationに属し、本システムによって単一のSessionビューに集約される。 |
| Board（看板） | 6列ビュー：`Draft` / `Plan` / `Execute` / `Verify` / `Done` / `Archive`。 |
| Trigger（トリガー方式） | `Auto`（Plan進入時にスケジューラーが起動）または `Manual`（ユーザーが「開始」ボタンをクリック）。 |
| Hermes Server | システムに登録された1つのHermes API Serverインスタンス：`base_url` + `API_SERVER_KEY` + `models[]`（複数のprofile、デフォルトは `hermes-agent`）。複数を登録可能で、そのうち1つがデフォルトとしてマークされる。 |
| Agent | 論理的には `(hermes_server_id, model)` の組み合わせ。タスクの「指定Agent」とは、ドロップダウンから特定のserver+modelを選択すること；指定しない場合はデフォルトserverのデフォルトmodelが使用される。 |

---

## 3. Hermesとのインテグレーション方式の選択

Hermes公式は2つの接続方法を提供しています：**ACP（Agent Client Protocol）** と **API Server（`hermes gateway`）**。

| 次元 | ACP | API Server |
|---|---|---|
| トランスポート | stdio / JSON-RPC | HTTP + SSE（OpenAI互換） |
| 位置づけ | エディター（VS Code / Zed / JetBrains）インテグレーション | 「ダッシュボードやthick clientsのためのストリーミングバックエンド」 |
| 複数セッション並列 | ホストプロセスにバインドされ、エディターのライフサイクルに制約される | ネイティブサポート、`POST /v1/runs` で独立したrunを作成 |
| 状態保持 | プロセス実行期のみ | `previous_response_id` / 名前付きconversationの永続化 |
| イベントストリーム | JSON-RPC notification | SSE `chat.completion.chunk` / Runs events |
| 認証 | なし（ローカルstdio） | Bearer Token |
| リモートデプロイ | 不向き | 適している（Host/Port/CORS設定可能） |

**結論：API Serverを採用し、名前付き `conversation` をSessionの唯一の永続IDとする。Responses APIを主軸、Runs APIを補助とする**。主な理由：
1. カンバンは**複数の並行Attempt**を同時に追跡する必要があり、プロセス外で独立してアドレス指定可能なセッションが必要——Responses APIの名前付き `conversation` がこの永続IDを提供する（サーバー側でツール呼び出しと結果を含む全履歴を自動管理）。
2. Goバックエンドはstdio JSON-RPCよりもHTTP/SSEの方が自然に消費でき、Hermesを別のマシンにデプロイすることも容易。
3. `conversation` パラメータ（または `previous_response_id`）は自然に「1つのAttempt = 1つのSessionの長い会話」にマッピングされる；検証フェーズでの追質問は**同一conversation上で新たなラウンドのresponseを開始**するだけ。

**Hermes APIの重要事実（確認済み）**：
- `/v1/chat/completions` はステートレスで、毎回全履歴を渡す必要がある——本シナリオには不向き。
- `/v1/responses` はステートフルで、`conversation: "<name>"`（または `previous_response_id`）によりサーバー側で完全な履歴を維持；**「メッセージ追加」は実際には同一conversation上で新たなresponse/runを作成**すること。
- `/v1/runs` と `/v1/runs/{id}/events` は単一実行（run）のイベントサブスクリプションのみを管理し、**実行中のrunへのuserメッセージ追加はサポートしない**。runは「このラウンドの実行」のワンタイムハンドルであり、1つのSessionライフサイクル内に**複数のrun**が生成される可能性がある。

したがって本システムの**Session ↔ Runの関係**は以下の通り：
```
1 Attempt  ==  1 Session  ==  1 named conversation
                                   │
                                   ├── run_1  (首轮 system+user prompt、tool calls 多步ありうる)
                                   ├── run_2  (ユーザー初回追質問 → 新 run、conversation を継続)
                                   ├── run_3  (ユーザー再追質問 → 新 run)
                                   └── …
```
「1 Attempt = 1 Session」のセマンティクスは**不変**；実装レベルでは、1つのSession内でユーザーラウンドごとに複数の `run_id` が生成されるが、`conversation_id` は1つのみ。

**主な接続ポイント（本システムで使用）**：
- `POST /v1/responses { conversation: "<attempt_id>", input: "<user msg>", ... }` → **新response/runを1回開始**；サーバー側はconversationからコンテキストを自動継承。`run_id` / `response_id` を返す。
- `GET /v1/runs/{run_id}/events`（SSE）→ このrunのツール呼び出し / token delta / ライフサイクルイベントをサブスクライブ。
- 1つのAttempt内でユーザー入力を受け取るたびに上記のペアを呼び出す：まず `POST /v1/responses` でrunを開始し、そのeventsをサブスクライブ。
- `GET /v1/models` → ドロップダウンでモデルを選択。
- `GET /health/detailed` → Serverのヘルスチェック。
- キャンセル：まず `/v1/runs/{run_id}/cancel` を使用（Hermesが提供する場合）；できない場合はSSEを閉じ、クライアント側でcancelledとしてマーク。

> Runs APIの正確なスキーマドキュメントは完全なフィールドが公開されていないため、**バックエンドに `HermesClient` インターフェースのレイヤーをカプセル化**し、リクエスト/レスポンス構造を1つのファイルに収束させ、フィールドの追記や将来のアップグレードを容易にする。

---

## 4. 機能要件

### 4.1 カンバンビュー
- 6列の固定順序：Draft → Plan → Execute → Verify → Done → Archive。
- 各カードの表示：タイトル、優先度バッジ（P1–P5のグラデーション）、タグChip、指派されたAgent、依存数/未準備の依存数、Attempt数バッジ。
- ドラッグ&ドロップによる列変更をサポート（ステートマシン検証付き、§4.4参照）。
- タグ / 優先度 / Agent / キーワードによるフィルタリングをサポート；タイトルと説明の検索をサポート。
- 列内は優先度昇順、次に更新時間降順でソート。

### 4.2 タスク（Task）属性

| フィールド | 型 | 説明 |
|---|---|---|
| `id` | UUID | プライマリキー |
| `title` | string | 必須 |
| `description` | markdown | `data/task/{id}.json` に保存、基本フィールドではない |
| `priority` | enum P1–P5 | P1が最高 |
| `dependencies` | []task_id | 前置タスク、すべて `Done` 状態である必要がありますExecuteに進入可能 |
| `trigger_mode` | enum `auto` / `manual` | Plan → Execute の自動または手動 |
| `preferred_server` | server_id / null | 未指定の場合はデフォルトServerを使用 |
| `preferred_model` | string / null | 未指定の場合はそのServerのデフォルトmodelを使用（通常は `hermes-agent`） |
| `tags` | []string | 複数タグ |
| `status` | enum | 現在の列 |
| `created_at` / `updated_at` | timestamp | |

### 4.3 試行（Attempt）属性

| フィールド | 説明 |
|---|---|
| `id` | UUID |
| `task_id` | 所属タスク |
| `server_id` | 今回使用するHermes Server |
| `model` | 今回使用するmodel（profile名；手動トリガー時は「実行開始」前にserver + modelの変更が可能） |
| `state` | `queued` / `running` / `needs_input` / `completed` / `failed` / `cancelled` |
| `session` | 唯一のHermes Session参照：{ `conversation_id`（Sessionの安定ID、Attempt全体を通じて使用）, `runs[]`（時系列に生成されたrun_idリスト、各ユーザー入力に対応）, `current_run_id`（現在アクティブなrun、存在する場合）, `latest_response_id` } |
| `started_at` / `ended_at` | |
| `summary` | Hermes完了時の最後のassistantメッセージの要約 |

> **並列実行**：同一Taskに対して複数の `running` Attemptを同時に存在させることが可能（例えば2つのAgentまたは2つのPrompt戦略を並列で試す）。
> **Agent切替**：「手動トリガー」タスクのみ、Attemptが `running` に入る前に `server` / `model` の変更を許可する。`running` に入った後は変更不可（Sessionが特定のserverにバインドされているため）。

### 4.4 ステートマシン

**遷移は2種類に分類**：
- **自動遷移**（バックエンドがトリガー）：`Plan → Execute`、`Execute → Verify`、`Verify → Execute` の3つのみ。
- **手動遷移**（ユーザーがカードをドラッグ）：**その他のすべての遷移はユーザーの手動ドラッグで完了**する。`Draft → Plan`、`Verify → Done`、`Done → Archive`、`Archive ↔ 任意`、および任意の引き戻し/スキップを含む。

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

- **Draft → Plan**：ユーザーがドラッグ。
- **Plan → Execute**（自動、この遷移のみが「Attemptを自動作成」する）：
  - `auto` トリガー：スケジューラーがN秒ごとにスキャンし、`Plan` 状態で `dependencies_all_done` のものを検出すると、自動的にAttemptを作成してカードをExecuteに移動。
  - `manual` トリガー：ユーザーがカード上の「Start」をクリック（server + modelを選択可能）、システムがAttemptを作成して自動的にExecuteに移動。Executeへの手動ドラッグはStartクリックと同等。
- **Execute → Verify**（自動）：**カード上のすべてのAttemptが終状態**（`completed` / `failed` / `cancelled`）**に入った場合のみ**、カードは自動的にVerifyに進入する。いずれかのAttemptがまだ `queued` / `running` / `needs_input` の場合、カードはExecuteに留まる。
- **Verify → Done**：**ユーザーがドラッグ**。Acceptボタンは存在しない。
- **Verify → Execute**（自動）：Verifyビューでユーザーが**完了済みのAttempt**に対して追質問を送信——バックエンドはそのAttemptの `conversation_id` に対して `POST /v1/responses` を呼び出し、**新run**を開始（同一conversationを継続するためSessionは不変）、そのAttemptは再び `running` 状態になり（`session.runs[]` に新しい `run_id` を追加）、カードは自動的にExecuteに戻る。Attemptの新規作成はなし、Sessionの新規作成もなし、runハンドルの追加のみ。
- **Done → Archive** / **任意 → Archive**：ユーザーがドラッグ。
- **Archive → 完全削除**：ユーザーが明示的に「Delete」を実行、データのカスケード削除（§7.3参照）。

> UI上でのドラッグは `POST /api/tasks/{id}/transition` の呼び出しと同等。不正なドラッグ（例えばDraftから直接Doneにドラッグ）はバックエンドのステートマシンによって拒否され、弾き戻される。

### 4.5 Hermesとのインタラクション
- **実行ビュー（カードをクリックして開くサイドバー/モーダル）**：
  - 左側：Attemptリスト、状態、server+model、タイムラインを表示。
  - 右側：現在選択中のAttemptの会話出力（Hermes SSEイベントに基づいてレンダリング、内容は `data/attempt/{attempt_id}/events.ndjson` に永続化保存、完全な会話履歴）：
    - Thought / Token delta（淡色、折りたたみ）。
    - ツール呼び出し展開カード（ツール名 / 引数 / 出力）。
    - Assistantテキスト（Markdown）。
    - 人工入力プロンプト（Hermesが入力を要求した場合はハイライト）。
  - **ページネーション戦略**：ビューを開いたとき、**末尾5件の「メッセージ」をデフォルトで読み込む**（1件のメッセージ = 1つのuser/assistantターンまたは1つのtool_callラウンド；イベントストリームを集約した論理ユニット）、トップまでスクロールすると「上へページ送り」がトリガーされ、さらに一段の履歴を読み込む；実行中のAttemptはSSEを通じてリアルタイムで底部に追加され続ける。
  - 底部入力ボックス：ユーザーはいつでもメッセージを送信可能、実際には `POST /v1/responses { conversation: attempt_id, input }` を呼び出す——バックエンドはまず現在のrunがアイドルになるのを待ち（または積極的にcancel）、その後新runを開始し、本Sessionの `runs[]` に追加。
  - 操作ボタン：Stop（現在のrunをキャンセル）、New Attempt（並列で新Attemptを開始）、Switch server/model（manualのみ、かつAttemptがrunningに入っていない場合）。
- **Hermes承認リクエスト**：ACPにある「承認フロー」はAPI Serverモードでは自動公開されないため、本システムはtool_callイベントで `approval_required` のカスタム規約を識別し、確認ダイアログを表示する（v1ではまずはパススルーでブロックしない）。

### 4.6 アニメーション（UIアニメーション）
- **実行中のカード**：緑色の流光ボーダー（`conic-gradient` + `@keyframes rotate` または `box-shadow` パルス）。
- **人工入力が必要**（Attempt state = `needs_input`）：オレンジ色のボーダー点滅（`@keyframes flash`）。
- **すべて完了**：カード上のすべてのAttemptが終状態に入った後、実行状態のアニメーションが消失し、システムは**自動的**にカードをVerify列に移動し、「移動」アニメーションを1回トリガー。
- カードのドラッグ&ドロップ：ネイティブHTML5 DnD + カスタムプレースホルダービジュアル。

### 4.7 タグ
- タグは独立したテーブルで、追加・削除・色の変更をサポート；タスクとは多対多で関連。
- 検索ボックスは `tag:backend` のようなDSLをサポートし、タイトル/説明の簡易キーワードあいまいマッチもサポート。

### 4.8 システム設定

#### 4.8.1 Hermes Server管理（複数server、複数model）

> **同時実行数設定ルール（再掲）**
> 接続された各Hermes Serverは**必ず**個別に最大同時実行数を設定しなければならない；同時に各Server配下の各profile（Hermes側のagent、例えば `hermes-agent`）にも個別に同時実行数上限を指定可能で、**profileレベルのデフォルトは5**。いずれのレベルの上限を超えた新しいAttemptは拒否されなければならない——`auto` タスクはPlanに留まり次回のスキャンを待つ；`manual` は409 `concurrency_limit` を返し、制限されたレベルを明示。

- 設定ページでServerリストの追加/変更/削除を提供；各Serverのフィールド：
  - `id`（安定したslug、ユーザーが編集可能、タスク内の `preferred_server` 値として使用；システム生成後は通常変更しない）
  - `name`（表示名、ユニーク）
  - `base_url`（例：`http://127.0.0.1:8642`）
  - `api_key`（Hermesの `API_SERVER_KEY` に対応、Bearer Tokenとして送信；`config.yaml` 保存時は `APP_SECRET` でAES-GCM暗号化して `api_key_enc` として保存）
  - `is_default`（システム全体で1つのみdefault server）
  - `max_concurrent`（**Serverレベルの同時実行数上限**：本Server全体の最大同時Attempt数、**デフォルト10**）
  - `models[]`（本Serverで利用可能なprofiles）：各アイテムのフィールド
    - `name`（profile名；`hermes-agent` はHermesのデフォルト）
    - `is_default`（このServer内のデフォルトmodel、未指定時に使用）
    - `max_concurrent`（**Profileレベルの同時実行数上限**：`(server, model)` 組み合わせの最大同時Attempt数、**デフォルト5**；ユーザーは設定ページで各profileごとに個別に調整可能）
- ページ上の「Test Connection」ボタン：`GET /health/detailed` + `GET /v1/models` を呼び出してkeyと現在利用可能なモデルを検証；結果は「設定で宣言されたmodelがserver上に実際に存在するか」を通知するために使用。
- Modelsはユーザーが設定で宣言（同時実行数上限などのポリシーを含む）、**自動同期はしない** `GET /v1/models`；`GET /v1/models` は参考表示のみ、「Import」をクリックするとserverにまだ宣言されていないmodelを `models[]` に一括追加可能（デフォルト同時実行数5）。
- Serverの削除：まだタスク/attemptが参照している場合は確認ダイアログを表示；削除後、タスク内の `preferred_server` をnullにクリア（デフォルトserverにフォールバック）。

#### 4.8.2 同時実行数制限の階層
スケジューラーとAttempt起動パスは以下の3段階のゲートを順にチェックする（すべて満たす場合に許可）：
1. **グローバル**：`scheduler.global_max_concurrent` —— すべてのアクティブAttemptの総数上限（デフォルト50）。
2. **Serverレベル**：`hermes_servers[x].max_concurrent` —— 対象server上のアクティブAttempt上限（デフォルト10）。
3. **Profileレベル**：`hermes_servers[x].models[y].max_concurrent` —— 対象(server, model)組み合わせ上のアクティブAttempt上限（**デフォルト5**）。

いずれかのゲートが満たされない場合：
- `auto` トリガータスク：Plan列に留まり、次回のスキャンを待つ。
- `manual` トリガーStart：409 `{code: "concurrency_limit", level: "global"|"server"|"profile"}` を返し、フロントエンドtoastで具体的な制限レベルを提示。

#### 4.8.3 グローバルパラメータ（すべて `data/config.yaml` に配置）
- `scheduler.scan_interval_seconds`（デフォルト5）
- `scheduler.global_max_concurrent`（デフォルト50）
- `archive.auto_purge_days`（デフォルト30、0 = 自動クリーンアップしない）

#### 4.8.4 アカウント / ログイン（オプトイン）
- デフォルト**無効**：誰でもボードWebにアクセス可能、ログイン不要（ローカルマシン/信頼されたLAN向け）。
- 「設定 → アクセス制御」ページで「アカウントパスワード認証を有効にする」を切り替え可能：
  - 有効化時にその場で `username` + `password`（最低8文字）を設定必須；
  - 确认后、凭据（username + bcrypt パスワードハッシュ + ランダム生成の `session_secret`）を `data/config.yaml` に書き込む；
  - 有効化後、直ちに現在のセッションをキックアウトし、ログインページにリダイレクト。
- 有効化後：未ログインのリクエストは401（API）/ `/login` にリダイレクト（ページ）。
- ログイン成功後、サーバーはHttpOnly / SameSite=Laxのcookieを発行（JWTまたは署名済みsession id、TTL設定可能、デフォルト7日）。
- 「パスワード変更」「アカウント認証の無効化」（現在のパスワード入力が必要）をサポート。
- **シングルユーザー**のみ許可（v1ではマルチユーザーやロール権限は実装しない）、シンプルに保つ。

#### 4.8.5 設定ファイルとホットリロード（**重要**）

> **契約（再掲）**
> 1. **すべてのシステム設定（アカウントパスワード、登録済みHermes Server、スケジューリング/アーカイブ/Preferenceなどすべてのスイッチ）は `data/config.yaml` に統一して保存**；SQLiteはビジネスデータ（tasks、attempts、tags、deps）のみを保存。
> 2. **起動時**：プロセスはYAMLを読み込み、メモリにキャッシュ（`atomic.Pointer[Config]` ロックレススナップショット）。
> 3. **変更時**：API / 設定ページを通じてトリガーされる設定変更はすべて、**メモリを先に更新**し、その後 `data/config.yaml` に**アトミックに書き戻す**（`tmp → fsync → rename`）；書き込み失敗時はメモリをロールバック、旧スナップショットを保持。
> 4. **手動編集 + ホットリロード**：設定ページのトップに**「ファイルから設定を再読み込み」ボタン**（`POST /api/config/reload`）を提供——ユーザーが直接 `vim data/config.yaml` で編集後、クリックするだけで有効化、**プロセスの再起動は不要**。失敗時は旧スナップショットを保持し、詳細なエラーを返す。

含まれる内容：
- `auth.*`（アカウントパスワード、session secret、TTL）
- `server.*`（リスンアドレス、CORS）
- `hermes_servers[].*`（複数server、各serverにmodelsと同時実行数上限を含む）
- `scheduler.*`、`archive.*`（グローバルパラメータ、スイッチ）
- `preferences.*`（言語、サウンド）

**ライフサイクル**：
- **起動**：プロセス起動時にyamlを読み込み、メモリ `*Config` にデコード（`sync/atomic.Pointer[Config]` によりロックレス読み込み）。
  - 認証フィールド（`auth.password_hash`、`hermes_servers[].api_key_enc`）はメモリ内で暗号化された形態を保持し、必要時に按需復号、平文でログに出力しない。
- **変更**：
  - API / 設定ページを通じて設定項目を変更する際、バックエンドはまず**メモリconfig**をインプレース更新し、その後 `write tmp → fsync → rename` でyamlをアトミックに上書き。
  - 失敗時はロールバック（ファイル書き込み失敗 → メモリを復元）。
- **リロード（ホットリフレッシュ）**：
  - 設定ページの**「ファイルから読み込み」**ボタン → `POST /api/config/reload`；バックエンドはyamlを再読み込み、検証、メモリポインタを置き換え、「副作用同期」を実行（下記参照）。
  - ユーザーは `data/config.yaml` を直接編集した後、このボタンをクリックするだけで、再起動不要。
- **ホットリフレッシュの副作用同期**（冪等でなければならない）：
  - `hermes_servers` の変更 → `HermesPool.Reload()` で影響を受けるclientを再構築；実行中のAttemptは中断しない（それらは該当runが終了するまで古いclient参照を保持）。
  - `auth.enabled` がtrueからfalseに変更 → 既存のcookieを直ちに無効化するか？設定により決定；デフォルトは**既存セッションを有効期限まで保持**し、自己矛盾を避ける。
  - `session_secret` の変更 → すべてのcookieが直ちに無効化（副作用は必然）。
  - `scheduler.*` / `archive.*` / `*.max_concurrent` の変更 → 次回のスキャンで即座に反映；goroutineを**再起動しない**。
  - `preferences.*` の変更 → `/api/stream/board` 経由で `preferences_updated` イベントをプッシュ、フロントエンドは新値を取得してUIを更新。
  - `server.listen` の変更 → バックエンドは旧 `http.Server` をグレースフルに `Shutdown` し、新アドレスでリスナーを再構築（同じhandler muxを再利用）；失敗時は旧アドレスにロールバックしてエラーを返す。ポート変更によりブラウザの接続が切断された場合、フロントエンドがSSE断線を検知して「ポートが変更されました、ブックマークを `<new>` に更新してからリフレッシュしてください」というプロンプトを表示。`server.cors_origins` の変更は即時反映（CORSミドルウェアのスナップショットにのみ影響）。
- **検証**：書き込みまたはリロードのたびに、まずスキーマ検証を実施（フィールド型、`max_concurrent ≥ 1`、`is_default` の一意性、idが空でないなど）、検証失敗時は読み込みを拒否して詳細なエラーを返す；メモリとファイルは元の状態を維持。

### 4.9 国際化（i18n）
- **サポート言語**：`zh-CN`（簡体字中国語）、`en`（English）。v1ではこの2つをターゲットとし、アーキテクチャ上は拡張可能。
- **デフォルト**：初回アクセス時にブラウザの `navigator.language` を読み込み、`zh*` に一致すれば中国語、そうでなければEnglish。
- **切り替え**：トップバーの言語切り替えボタン（🌐 `中 / EN`）、即時グローバルに反映。
- **永続化**：ログイン状態では当該ユーザーの `preferences.language` に書き込み；未ログイン状態では `localStorage` に書き込み。
- **リソースファイル**：`web/locales/zh-CN.json`、`web/locales/en.json`、構造はフラットな key → string。Vue側は非常に小さな `$t(key, params?)` ヘルパーを使用（追加のビルドステップを避けるためvue-i18nは導入しない）。
- **適用範囲**：すべてのUIテキスト、列名、状態ラベル、ボタン、toast、フォーム検証エラー；**Hermesが生成した内容は翻訳しない**（原語のまま保持）。
- **日付/数値フォーマット**：ブラウザネイティブの `Intl.DateTimeFormat` / `Intl.NumberFormat` を使用、`language` に追従。
- **バックエンドエラー**：APIエラーレスポンスは統一された `{ code, message_key, params }` 構造を返す；フロントエンドはcode/message_keyに基づいてローカル辞書でレンダリング。

### 4.10 レスポンシブ & PWA
- **レスポンシブブレークポイント**：
  - `≥1200px`：完全な6列を並列表示；
  - `768–1199px`：3列横並び + 水平スクロール；列ヘッダーをトップに固定；
  - `<768px`（スマートフォン）：**単列**のみ表示、トップに1行の状態タブ（Draft/Plan/…）、左右スワイプジェスチャーで列を切り替え；カードは全幅表示；実行パネルはフルスクリーンモーダルに変更。
- **インタラクション**：
  - ドラッグ&ドロップ：デスクトップではHTML5 DnDを維持；モバイルでは「カードを長押し → 底部にAction Sheetを表示 → 対象列を選択」に変更し、iOS/AndroidのHTML5 DnDの不一致を回避。
  - 入力：実行パネルの入力ボックスはスマートフォンで底部に固定され、ソフトキーボードの表示に合わせて上昇。
- **PWA**：
  - `web/manifest.webmanifest` を提供：name、short_name、start_url、display=`standalone`、theme_color、icons（192/512 PNG + maskable）。
  - `web/sw.js`（Service Worker）を提供：`app-shell` で静的リソースをキャッシュ（index.html、js、css、locales、vue.global.js、icons）；API / SSEリクエストは**キャッシュしない**（常にnetwork-first、失敗時はオフラインプロンプトページを返す）。
  - 「ホーム画面に追加」をサポート、iOSでは `apple-touch-icon` metaと連携；インストール後は独立したAppモードで実行。
  - ログインcookieは起動を跨いで利用可能、スマートフォンで開くと直接カンバンにアクセス。
  - オフライン使用は**v1の範囲外**（カンバンは本質的にリアルタイム）；オフラインではシェルの白画面回避のみを保証。

### 4.11 サウンド通知
- **トリガーイベント**（3種類）：
  1. タスクがExecuteに進入（最初のAttemptが実際にrunningを開始した時）。
  2. 任意のAttemptが `needs_input` に進入。
  3. 任意のAttemptが終状態に到達（completed / failed）→ ステートマシンが「完了」提示音をトリガー；カード全体が自動的にVerifyに移動した場合は重複して鳴らさない。
- **素材**：フリー著作権ライブラリ（freesound.org / mixkit.co / notificationsounds.com、CC0 / CC-BYに準拠）から3つの短い音（すべて ≤1秒、≤30 KB OGGまたはMP3）を収集し、`web/assets/sounds/` に配置：
  - `start.ogg`（軽やかな「叮」）
  - `input.ogg`（上昇「嘟嘟」）
  - `done.ogg`（成功「叮咚」）
  ダウンロード時にソースとライセンスを `web/assets/sounds/CREDITS.md` に記録。
- **再生**：フロントエンドは `sound.js` モジュールを通じて、SSE `board` チャンネルの `attempt.state_changed` イベントをリッスンし、ルールに従って再生。`HTMLAudioElement` でプリロード；初回のユーザージェスチャー後にブラウザの自動再生ポリシーをアンロック。
- **デフォルトでオン**；設定ページの「環境設定 → サウンド通知」にマスターのオン/オフスイッチ + 各イベントごとのサブスイッチ + 音量（0–100%）。
- **永続化**：ログイン状態では `preferences.sound` に保存（ユーザー粒度）、そうでなければ `localStorage`。
- **モバイル**：PWAウィンドウで開いている場合は正常に鳴響；バックグラウンド/ロック画面では保証されない（ブラウザ制限）。システムWeb Pushは**使用しない**（v1ではNotification APIプッシュを実装しない）。

---

## 5. 非機能要件

| 次元 | 目標 |
|---|---|
| スケール | 数百のタスクが並存；≤50の同時running Attempt。 |
| レイテンシ | カンバンリストAPI < 50ms（メモリキャッシュヒット）；イベントストリーム < 200ms転送遅延。 |
| リソース | アイドル時メモリ < 150MB；単一バイナリで起動。 |
| 可用性 | 任意のtask.json / attemptディレクトリの欠落がシステムクラッシュを引き起こしてはならない、プレースホルダーを表示してwarningを記録するのみ。 |
| リカバリティ | プロセス再起動後、`running` Attemptは自動的にHermesのrun_idへの再接続を試行（まだ生存している場合）、そうでなければ `failed` としてマークし、理由は `detached`。 |
| 観測性 | 構造化ログ（zerolog/slog）+ `/healthz` + `/metrics`（オプションprometheus）。 |
| レスポンシブ | 360px～2560pxの幅をサポート；モバイルLighthouse PWAスコア ≥ 90。 |
| i18n | 中国語/英語の切り替えでページリフレッシュ不要；未翻訳のkeyは `en` 文字列にフォールバック；すべての新しいテキストは同時に `zh-CN` と `en` 辞書に登録する必要がある。 |
| サウンド | 読み込み失敗時はサイレントにダウングレード、UIをブロックしない；完全ミュート時でもイベントは正常にアニメーションと状態更新をトリガー。 |

---

## 6. アーキテクチャ設計

### 6.1 全体トポロジー

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

### 6.2 バックエンドモジュール分割（Go）

```
cmd/taskboard/main.go          エントリポイント：依存関係のアセンブリ、HTTP起動、グレースフルシャットダウン
internal/config                 設定読み込み：起動時env + 実行時data/config.yaml（ホットリロード）
internal/server                 HTTPルーティング、ミドルウェア、静的リソース、SSE Hub
internal/board                  ステートマシン、正当な遷移、ドメインサービス
internal/store
  ├── sqlite                   *sql.DB カプセル化（modernc.org/sqlite 純Goドライバ）
  └── fsstore                   data/task、data/attempt のファイル読み書き
internal/hermes                 HermesClient プール（server_id によるルーティング；runs/responses/models/health をカプセル化）
internal/attempt                AttemptRunner（runの起動、SSEの消費、ディスク書き込み、ブロードキャスト）
internal/scheduler              auto-trigger スキャン、依存関係解決、スロットリング
internal/sse                    サーバー→ブラウザのイベントバス（task/attempt 単位でサブスクライブ）
internal/auth                   アカウントパスワードログイン、bcrypt、session cookie、ミドルウェアガード（オン/オフ可能）
internal/security               API内部トークンミドルウェア、AES-GCM暗号化 hermes api_key
web/                            フロントエンド静的リソース（go:embed で埋め込み）
```

### 6.3 データ保存戦略

#### 6.3.1 SQLite（`data/db/taskboard.db`）——ビジネス基礎情報のみ
SQLiteには**「システム設定」は一切保存しない**（Hermes Servers、アカウント、スイッチ、Preferenceはすべて `data/config.yaml` に移行）。
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
> `preferred_server` / `server_id` は「ソフト外部キー」——Hermes Serverの追加/削除はyamlを基準とし、SQLiteはカスケードしない。参照されているserverがyamlから削除された場合、読み込み時は「未指定」としてデフォルトserverにフォールバック。

#### 6.3.2 ファイルストレージ（SQLiteの膨張を回避）
```
data/
  config.yaml               # アクセス制御 + 少量の人間が読む/編集するグローバル項目
  db/taskboard.db
  task/
    {task_id}.json          # { id, description(markdown), attachments_meta, custom_prompt_prefix, ... }
  attempt/
    {attempt_id}/
      meta.json             # { server_id, model, session:{ conversation_id, runs:[run_id...], current_run_id, latest_response_id }, summary }
      events.ndjson         # 1行につき1つのHermes SSEイベント（追記書き込み）
      transcript.md         # 人間可読なアーカイブ（オプション、完了時に生成）
```

`data/config.yaml` の完全な例（**ファイル権限0600**、バックエンドが初回書き込み時に作成）：
```yaml
auth:
  enabled: false                    # デフォルト無効
  username: ""                      # 有効化後に書き込み
  password_hash: ""                 # bcrypt($2a$...)
  session_secret: ""                # 32バイトランダム値 hex；cookie/JWTの署名に使用
  session_ttl_hours: 168            # 7日

server:                             # このプロセス自体のリスン設定
  listen: "0.0.0.0:1900"            # デフォルトポート1900；127.0.0.1:1900 はローカルのみ、0.0.0.0:1900 はLANアクセス可能
  cors_origins: []

hermes_servers:                     # 登録済みHermes API Serverリスト
  - id: "default"                   # タスク内のpreferred_serverが参照する安定したslug
    name: "Local Hermes"
    base_url: "http://127.0.0.1:8642"
    api_key_enc: "base64(AES-GCM(...))"   # 暗号化されたAPI_SERVER_KEY
    is_default: true
    max_concurrent: 10              # 本Serverの総同時実行数上限（デフォルト10）
    models:
      - name: "hermes-agent"
        is_default: true
        max_concurrent: 5           # 各profileの同時実行数上限デフォルト5
      - name: "hermes-agent-gpt"
        max_concurrent: 5
  # さらに多くのserverを追加可能；is_default: true は1つのみ

scheduler:
  scan_interval_seconds: 5
  global_max_concurrent: 50         # グローバルAttempt同時実行数上限（デフォルト50）

archive:
  auto_purge_days: 30               # Archive列のカードがこの日数を超えた後にバックグラウンドで物理削除；0=無効

preferences:                        # シングルユーザーのPreference
  language: ""                      # 空文字列=ブラウザに追従；それ以外 "zh-CN" / "en"
  sound:
    enabled: true
    volume: 0.7                     # 0.0 ~ 1.0
    events:
      execute_start: true
      needs_input:   true
      done:          true
```

**`internal/config` の責務**：
- 起動時 `Load(path)`：ファイル読み込み → スキーマ検証 → AES-GCM復号検証 → `atomic.Pointer[Config]` に保存。
- 実行時のすべての読み取り専用アクセスは `cfg := configStore.Snapshot()` を通じて不変スナップショットを取得し、並行ロックを回避。
- 書き込みパス：`Mutate(fn)` —— コピーを作成 → `fn(copy)` で変更 → 検証 → ポインタをアトミックに置換 → `persist()`（`tmp + fsync + rename`）→ `config_updated` イベントをブロードキャスト → 副作用フック（`HermesPool.Reload()` など）を実行。
- ホットリロード：`Reload()` —— ファイルを再読み込みし、同じvalidate + swap + hookパスを経由；失敗時は旧スナップショットを保持してエラーを返す。
- `session_secret` / `api_key_enc` のローテーションはMutateパスによって自然に副作用がトリガーされる（cookie無効化 / HermesPool再構築）。

**選定理由**：
- イベントストリームは本質的に**追記書き込み**であり、NDJSONはSQLite blobよりもコストが低く、`tail -f` デバッグや按需ストリーミング読み取りにも便利。
- タスクの説明は長いテキストであり、フィルタクエリに参加しないため、ファイルに保存することでSQLiteページの膨張を回避。
- SQLiteはインデックス + トランザクション専用（ステートマシン遷移は必ずトランザクション）、サイズはMBレベルで安定。

#### 6.3.3 欠落データのフォールトトレランス
- `tasks` 行を読み取ったが `data/task/{id}.json` が欠落 → `description=""` + `warning: missing_description` を返し、500エラーにはしない。
- Attemptを読み取ったが `meta.json` が欠落 → `state="unknown"` + warningを返す；`events.ndjson` が欠落 → イベントストリームは空だがカードは表示可能。
- 起動時に `integrity_check` を実行：DBとFSを比較、warningを出力；**自動削除はしない**、ユーザーデータの誤削除を防止。

### 6.4 リアルタイムイベントストリーム設計 & 永続化 & 断線再接続

#### 6.4.1 データの痕跡
- `data/attempt/{attempt_id}/events.ndjson` は**当該Attemptに対応するSessionの完全な会話履歴**の唯一の信頼できるソース；**複数のrunにわたるイベントはすべて同一ファイルに追記**され、論理的に連続したSessionイベントストリームを構成する。バックエンドはHermesイベント（SSEまたは再接続後に補完された履歴）を受信するたびに、**まず**そのファイルに追記書き込みし、**その後**SSE Hubにプッシュしてフロントエンドにブロードキャスト。
- ファイルには元のHermesイベントに加え、システムレベルのマーカー行も挿入：`{"kind":"system","event":"run_start"|"run_end"|"connect"|"disconnect"|"reconnect"|"backfill","run_id":...,"ts":...,"cursor":...}`——特に `run_start` / `run_end` は各ユーザーインタラクションラウンドの境界を区切り、フロントエンドがrun単位で折りたたむまたはSession単位で連続レンダリングできるようにする。
- 各行のNDJSONには単調増加する `seq` が付与（バックエンドが注入、uint64、**runを跨いでグローバルに単調**）、ファイル内のオフセットの論理カーソルであり、SSEの `Last-Event-ID` でもある。

#### 6.4.2 Hermes → Backend（AttemptRunner）
`AttemptRunner` は各**アクティブAttempt**に対して1つのgoroutineを維持；アクティブの定義：`state ∈ {queued, running, needs_input}`。

**1ラウンド（run）のライフサイクル**：
1. 新runの開始：
   - **初回ラウンド**（Attemptが作成された直後）：`POST /v1/responses { conversation: attempt_id, input: <初期システム/ユーザープロンプトの組み合わせ>, model, stream: true }`、`run_id` と `response_id` を取得して `meta.json` に書き込み。
   - **後続ラウンド**（ユーザーの追質問）：`POST /v1/responses { conversation: attempt_id, input: <user msg> }`、同様に新しい `run_id` を返す。
2. 1行 `{"kind":"system","event":"run_start","run_id":...}` を書き込み。
3. `GET /v1/runs/{run_id}/events` をサブスクライブ（このrunが `response_id` を生成する前にサブスクライブ可能な場合）またはレスポンスストリームを読み取り；各イベントで：
   a. `seq := seq+1`、`events.ndjson` に書き込み（`O_APPEND` + fsyncバッチ）；
   b. 軽量な解析（tool_callの識別、入力が必要、完了、失敗）；
   c. `sse.Hub` のトピック `attempt:{id}` にブロードキャスト；
   d. Attempt / Taskの状態変更がヒットした場合、Board Serviceのトランザクションを呼び出してDBに書き込み、`board` をブロードキャスト。
4. run終了 → `run_end` マーカーを書き込み、`meta.session.current_run_id=null` と `latest_response_id` のディスク更新を落盤。Attemptがまだ必要とする場合（キューされたユーザー入力が存在）、直ちに次のラウンドを開始；そうでなければAttemptは終状態（completed/failed/cancelled）に進入。

**ユーザーメッセージのキューイング**：
- 同一Attemptは現在のrunが終了していない限り、並列で新runを開始できない（Hermes conversation内の順序セマンティクス）。
- ユーザーが「送信」をクリック：現在runが実行中の場合、AttemptレベルのFIFOキューにキューイング；UIに「現在実行中、メッセージはキューに追加されました」というプロンプトを表示。「中断してすぐに送信」を選択することも可能——バックエンドは現在のrunをcancelした後、新runを開始。
2. **断線自己回復**：
   - 任意のネットワークエラー（EOF、タイムアウト、5xx）は断線として扱い、`disconnect` システムイベントを記録。
   - **指数バックオフ + ジッター** で再接続（1秒、2秒、4秒、8秒…上限30秒）。
   - 再接続のターゲットは**現在アクティブなrunの `run_id`**（`meta.session.current_run_id`）。試行：
     a. `GET /v1/runs/{run_id}/events?after=<seq>`（Hermesがカーソル再生をサポートしている場合）、断線期間中のイベントを `events.ndjson` に補完書き込み（`backfill` マーク）；
     b. カーソルがサポートされない場合、Responses APIを通じて `conversation` + `previous_response_id` で現在のレスポンスの完全な結果を取得し、すでにディスクに書き込まれたeventsとdiffして補完。
   - その後通常のストリーミングサブスクリプションに入る。
   - 再接続中、Attemptの状態は不変（フロントエンドは引き続きrunningを表示）、UIのトップにのみ "reconnecting…" のグレーバーを表示。
3. **プロセス再起動の回復**：起動時に `attempts` テーブルの `state IN ('queued','running','needs_input')` の行をスキャンし、それぞれに対して：
   - `meta.json` の `session.conversation_id` と `current_run_id` を読み取り；
   - `current_run_id` がまだHermes側で生存している場合、イベントのサブスクリプションを再開（断線再接続と同じ補完ロジックを経由）；
   - そのrunが期限切れ/404の場合：`conversation` を通じてResponses APIで最後のレスポンスの状態を取得；補完可能な場合は継続実行、回復不可能な場合はAttemptを `failed` / `reason='detached'` としてマーク、すでにディスクに書き込まれたeventsは保持。
   - Attemptにまだ処理待ちのユーザー入力が存在する場合（FIFOキュー参照）、旧runの回復完了後に順序に従って次のラウンドを継続。
4. プロセス終了時に中間状態をクリーンアップしない；ディスクへの書き込みがそのまま進捗。

#### 6.4.3 Backend → Browser
- **2つのSSEチャネル**：
  - `GET /api/stream/board`：タスク状態/カードレベルのイベント（move、create、delete、attempt_summary_update）。
  - `GET /api/stream/attempt/{id}?since_seq=N`：某个Attemptの詳細イベントストリーム；`since_seq` のデフォルトは「最新」。フロントエンドはカード詳細に進入したときのみサブスクライブし、ストームを回避。
- ブラウザSSE断線は自動再接続、`Last-Event-ID: <seq>` でバックエンドにどこから回復するかを通知；バックエンドは `events.ndjson` から `seq+1` に直接seekしてプッシュを継続、メモリring bufferに依存しない。

### 6.5 スケジューラー（Auto-trigger）
- `scheduler.scan_interval_seconds`（デフォルト5秒）ごとに `status='plan' AND trigger_mode='auto'` のタスクをスキャンし、優先度昇順で処理。
- 各タスクに対して順にチェック：
  1. すべての依存関係が `done` 状態；
  2. `COUNT(*) FROM attempts WHERE state IN ('queued','running','needs_input') < scheduler.global_max_concurrent`（グローバル）；
  3. `COUNT(*) ...AND server_id = ? < server.max_concurrent`（Serverレベル）；
  4. `COUNT(*) ...AND server_id = ? AND model = ? < model.max_concurrent`（Profileレベル、デフォルト5）；
  5. 対象Serverのヘルス（30秒キャッシュの `/health/detailed` 結果）。
- すべて満たす場合、Attemptを作成して非同期で `POST /v1/runs`；いずれかのゲートが満たされない場合、そのタスクをスキップして次回に回す。
- 手動トリガーパス（`POST /api/tasks/{id}/attempts`）も同じ並行数チェック関数 `CanStart(serverID, model)` を経由し、ゲートが満たされない場合は409 + 制限レベルを返す。
- 並行数カウントは `idx_attempts_server_model_state` インデックスを使用、定数時間レベルのオーバーヘッド；設定上限の読み取りは `atomic.Pointer[Config]` スナップショットでロックレス。

### 6.6 並行性と一貫性
- すべての状態遷移はBoard Serviceの `Transition(taskID, to, reason)` を経由し、内部で `BEGIN IMMEDIATE` トランザクション + 正当性チェック。
- `AttemptRunner` とスケジューラーはBoard Serviceを通じてのみ状態を変更可能、直接DBへの書き込みは禁止。
- SSE Hubはring bufferを使用（トピックごとに最後のN件を保持）、ブラウザ断線再接続時は `Last-Event-ID` で再生。

---

## 7. インターフェース設計

### 7.1 REST（バックエンド自体が外部に公開）
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

認証ルール（`auth.enabled` の状態に応じていずれか一方）：
- `auth.enabled = false`：すべての `/api/*` はデフォルトで許可；ただし `/api/auth/enable` は常に許可（初回有効化用）。
- `auth.enabled = true`：`/api/auth/login`、`/api/auth/status`、`/healthz`、静的リソースを除き、他のすべての `/api/*` はcookieが必要；未ログインの場合は401を返す。
- `/api/auth/enable` は有効化済み状態で409を返す；認証情報の二重上書きを回避。

### 7.2 Hermes呼び出しカプセル化（例示）
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
- **初回ラウンド**：`CreateResponse` に `SystemPrompt` を付与（カンバンがタスクコンテキストを注入：タイトル + description + 依存関係の要約）と初期 `Input`。
- **後続ラウンド（ユーザーの追質問）**：`Conversation` + `Input` のみ、Hermesサーバー側はconversationに基づいて履歴を自動継承（ツール呼び出しと結果を含む）。
- 各Clientは構築時に `API_SERVER_KEY` を `Authorization: Bearer ***` としてすべてのリクエストに固定注入。

### 7.3 削除カスケード
Taskを削除する場合：
1. トランザクション内：`task_tags`、`task_deps`、`attempts`（DB行のみ）、`tasks` 行を削除；
2. トランザクション外 best-effort：`rm -rf data/task/{id}.json`、`rm -rf data/attempt/{aid}` （各子attempt）；
3. まだrunning中のAttemptに対して、`HermesClient.CancelRun` を非同期でトリガー；失敗した場合は "reaper" キューに記録してバックグラウンドで再試行。
4. 手順2、3に残留があっても、DBに参照がなければ、フロントエンドには二度と表示されない。

---

## 8. フロントエンド設計（Vue 3、ビルドなし）

### 8.1 ディレクトリ構造
```
web/
  index.html
  manifest.webmanifest    # PWA manifest
  sw.js                   # Service Worker（app-shell キャッシュ + オフラインフォールバック）
  assets/
    vue.global.js         # Vue 3 プロダクションバージョン（ローカル）
    app.css
    animations.css        # 緑色のグロー / オレンジの点滅
    responsive.css        # ブレークポイントとモバイルレイアウト
    icons/                # PWA アイコン：192、512、maskable、apple-touch-icon
    sounds/               # start.ogg / input.ogg / done.ogg + CREDITS.md
  locales/
    zh-CN.json
    en.json
  js/
    app.js                # createApp エントリポイント；/api/auth/status に応じてログインページまたはカンバンをレンダリング
    i18n.js               # $t(key, params) + 言語切替 + 永続化
    sound.js              # プリロード & イベントに応じた再生 & 音量制御 & 初回ジェスチャーアンロック
    pwa.js                # sw.js の登録、beforeinstallprompt のリッスン、「ホーム画面に追加」の表示
    components/
      Login.js            # ログインページ（認証有効化後のファサード）
      Board.js
      Column.js
      TaskCard.js
      TaskModal.js
      AttemptPane.js
      EventStream.js      # SSEサブスクライブ、ツール呼び出し/テキストのレンダリング
      AgentSelect.js      # カスケード：Hermes Server → Model
      TagInput.js
      LanguageSwitch.js   # トップバー 🌐 中/EN
      SettingsServers.js  # 複数server CRUD + 接続テスト + /v1/models の表示
      SettingsAuth.js     # アカウント認証の有効化/無効化、パスワード変更
      SettingsPrefs.js    # 言語、サウンドオン/オフ/音量、各イベントごとのサブスイッチ
    stores/
      board.js            # 軽量リアクティブストア
      attempt.js
      auth.js             # ログイン状態、serversキャッシュ
      prefs.js            # language / sound 設定（バックエンド + localStorage の二重書き込み）
    api.js                # fetch ラッパー（自動cookie付与、401でログインにリダイレクト）
    sse.js                # EventSource ラッパー + 断線再接続
  favicon.svg
```

### 8.2 制約
- webpack/vite/rollupを使用しない；ブラウザネイティブES Module（`<script type="module">`）で直接読み込み。
- Vueは**Global Build**（`vue.global.js`）+ 手書きの `defineComponent` / ランタイムテンプレート文字列を使用、`.vue` SFCコンパイルの必要性を回避。
- CSSは手書き、アニメーションキーフレーム：
  ```css
  @keyframes glow-rotate { to { --a: 360deg; } }
  .card.executing { border: 2px solid transparent;
    background: linear-gradient(#1e1e2a,#1e1e2a) padding-box,
                conic-gradient(from var(--a,0), #00e676, #00c853, #00e676) border-box;
    animation: glow-rotate 2.5s linear infinite; }
  @keyframes flash { 0%,100%{box-shadow:0 0 0 0 #ff9800}50%{box-shadow:0 0 0 6px #ff980055} }
  .card.needs-input { border-color: #ff9800; animation: flash 1s ease-in-out infinite; }
  ```
- ドラッグ&ドロップ：ネイティブ `draggable="true"` + dragover/drop；ドロップポイントの検証はフロントエンドで楽観的更新を1回行い、バックエンドの `POST /transition` が最終裁定。デスクトップとタブレットはDnD、スマートフォン（`<768px`）は「長押し → Action Sheet」にデグレード。
- トップバーmeta：`<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">`、ノッチスクリーンのセーフエリアに対応。
- Service Worker：`app.js` 内で `navigator.serviceWorker.register('/sw.js')` を呼び出し；アップグレード戦略は "skipWaiting + Clients.claim" を採用、フロントエンドが新バージョンを検出したときtoastでユーザーにリフレッシュを促す。

### 8.3 ページ
1. ログインページ（`auth.enabled=true` の場合のみ表示；そうでなければ直接メインカンバン）。
2. メインカンバン（デスクトップ6列並列 / タブレット3列スクロール / スマートフォン単列 + トップタブ）。
3. タスク詳細ドロワー（説明の編集、依存関係、トリガー方式、Agentドロップダウン=server+modelカスケード、タグ）。
4. 実行パネル（Attemptリスト + 会話出力（末尾5件 + 上へページ送り）+ 会話入力）。
5. システム設定ページ（トップに**「ファイルから設定を再読み込み」**ボタン → `POST /api/config/reload`、および「現在のconfig.yamlをダウンロード」「スナップショットdiffを表示」の補助アクション）：
   - **Hermes Servers** —— リストCRUD、Test Connection、当該serverのmodelsの表示、デフォルトserverの切り替え；各serverの `max_concurrent` を編集可能；各model行の `max_concurrent`（デフォルト5）と `is_default` を編集可能。
   - **グローバル同時実行数** —— `scheduler.global_max_concurrent`、スキャン間隔などの設定。
   - **アクセス制御** —— アカウントパスワードの有効化/無効化、パスワード変更。
   - **環境設定** —— 言語（中国語 / English）、サウンド通知（マスターのオン/オフ + イベントごとのサブスイッチ + 音量）。
   - **アーカイブ** —— Archive自動クリーンアップ日数。
   - **タグ管理**（タグのみSQLiteを経由）。

> すべての保存ボタンのセマンティクスは「メモリを更新 + `data/config.yaml` に永続化」。ユーザーは直接 `vim data/config.yaml` で編集し、その後トップの「ファイルから設定を再読み込み」ボタンをクリックするだけで有効化、再起動は不要。

---

## 9. ディレクトリ計画（リポジトリルート）

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

## 10. パフォーマンスと容量の考慮

| 注目点 | 方案 |
|---|---|
| カンバンの初画面 | SQLiteの基本フィールドのみ読み取り；`tasks.description` は空のまま、詳細は按需で追加取得。 |
| リストのページネーション | 単列 > 100件の場合 `limit 50 + infinite scroll` を有効化。 |
| ホットキャッシュ | バックエンドメモリで `map[taskID]*TaskLite` を維持、書き込みパスで同期無効化；カンバンレベルSSEはdiffのみをプッシュ。 |
| イベントストリームI/O | NDJSONを追記書き込み；SSE Hubはトピックごとに最新500件のみ保持、履歴は `GET /events?from=` で取得。 |
| SQLite | `PRAGMA journal_mode=WAL; synchronous=NORMAL; busy_timeout=5000;` |
| 大量のAttempt | `attempts` にtask+stateでインデックスを作成；アーカイブタスクのAttemptデータはプリロードしない。 |
| クリーンアップ | ArchiveがN日を超えたタスクをバックグラウンドでバッチ物理削除；起動のたびにFSとDBのorphanの対帳を1回実行。 |

---

## 11. セキュリティとデプロイ

### 11.1 アクセス制御（Board Webログイン）
- **デフォルト無効**：匿名でアクセス可能、`auth.enabled=false`。
- 管理者が「設定 → アクセス制御」でアカウントパスワードを有効化：
  - `username`（ユニーク）+ `password`（≥8文字）；
  - パスワードは `bcrypt`（cost=12）でハッシュ化して `data/config.yaml` に書き込み；
  - 同時に32バイトランダム `session_secret` を生成してcookie（またはJWT HS256）の署名に使用。
- ログイン：`POST /api/auth/login` で検証成功后、`HttpOnly; SameSite=Lax; Path=/` cookieを発行、TTLはデフォルト7日；期限切れは自動的にログインページにリダイレクト。
- アカウント認証の無効化：現在のパスワードの入力を要求；無効化後、yaml内の認証情報フィールドをクリアし、すべての既存cookieを取り消し（`session_secret` をローテーション）。
- ミドルウェアロジック：
  ```
  if config.Auth.Enabled && !isPublicPath(r) && !validCookie(r):
      if isAPI(r): return 401
      else:        return 302 /login
  ```

### 11.2 Hermes Serverの認証情報
- 複数のServerがそれぞれ `api_key`（Hermesの `API_SERVER_KEY`）を1つずつ保持。
- `data/config.yaml` に書き込む前にAES-GCMで暗号化して `api_key_enc` として保存（フィールド名は `_enc` で終了）；キーは環境変数 `APP_SECRET` から取得（初回起動時に欠落している場合はランダムに生成して `data/db/.secret` に書き込み、権限0600）。
- メモリに読み込んだ後、按需で復号して `Authorization: Bearer ***` ヘッダーに注入；**フロントエンドには一切配信しない**。フロントエンド設定ページでは既存のserverに対してプレースホルダー（`••••`）のみを表示し、api_keyの変更には再入力が必要。
- ユーザーが手動で `config.yaml` を編集する際、直接 `api_key: "<plaintext>"` を書き込むことも可能；`POST /api/config/reload` 時にバックエンドが平文フィールドを検出すると、ディスクへの書き戻し時に自動暗号化して平文keyを削除（すなわち「読み込み時は寛容、書き込み時は暗号化」）。

### 11.3 プロセス & ファイル
- デフォルトは `0.0.0.0:1900` をリスン（`server.listen`）；本番環境でローカルのみ使用する場合は `127.0.0.1:1900` に変更を推奨；LAN開放が必要な場合は、アカウント認証の有効化 + CORS allowlistの設定を同時に推奨。
- ポートは**設定センターページ**または直接 `data/config.yaml` の `server.listen` を編集して変更可能。
- `data/config.yaml` の権限は0600；`data/` ディレクトリ全体は0700。
- ログにはHermesの `api_key`、ログインパスワード、ユーザーメッセージの全文を出力しない；長さとハッシュプレフィックスのみを記録（デバッグ時はverboseを有効化可能）。
- 単一バイナリ + `data/` ディレクトリ；systemd serviceでの起動を推奨。
- バックアップ推奨：`sqlite3 .backup` + `rsync data/`（`config.yaml` を含む）。

---

## 12. マイルストーン提案

1. **M1（スケルトン）**：Go HTTP + SQLiteスキーマ + 静的ページ；タスクCRUD + ドラッグ&ドロップ；Hermesは接続しない。
2. **M2（Hermes接続）**：`HermesClient` + AttemptRunner + SSE転送；単一Attemptのエンドツーエンド。
3. **M3（完全ステートマシン）**：依存関係、自動トリガー、スケジューラー、Verifyの引き戻し。
4. **M4（エクスペリエンス）**：アニメーション、複数並行Attempt、Agent切り替え、検索/フィルタDSL。
5. **M5（安定状態）**：バックアップ、metric、再起動再接続、対帳、負荷テスト 500タスク × 20並行。

---

## 13. オープンな問題

1. ~~Hermes Runs APIは単一のrunに対して「userメッセージの追加」をサポートするか？~~ **サポートしないことを確認済み**。結論は§3、§6.4.2、§7.2に反映済み：Attempt内の継続会話はResponses API + 名前付き `conversation` で統一、各ラウンドの入力は新しい `run_id` を生成、Runs APIは当該ラウンドのイベントストリームのサブスクライブのみを担当；Attempt↔Session（= conversation）の1:1関係は不変。
2. 検証フェーズでの追質問時、フロントエンドUIのデフォルトは「現在のAttempt」でメッセージを継続送信（新Attemptを開かない）；ユーザーが異なる戦略を比較したい場合は、明示的に「New Attempt」で別のAttemptを起動可能（新しいconversation、新しいSessionに対応）。
3. ツール呼び出しの人工承認（危険なターミナル）が必要か？v1ではブロックせず、イベントストリーム内でハイライト表示のみを推奨；v2でACP承認フローを接続可能。
4. マルチユーザー/権限：v1では実装しない；必要な場合はAPP_TOKENを環境変数で配布すればよい。
