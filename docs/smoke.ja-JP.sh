#!/usr/bin/env bash
# 簡易 API スモークテスト — hermes-taskboard が 127.0.0.1:1900 で実行中であることを想定。
set -euo pipefail
API="${API:-http://127.0.0.1:1900}"

echo "→ healthz"
curl -sf "$API/healthz" >/dev/null

echo "→ タスク作成"
TID=$(curl -sf -X POST "$API/api/tasks" \
  -H "Content-Type: application/json" \
  -d '{"title":"Smoke: hello","description":"Hi Hermes","priority":3,"status":"plan","trigger_mode":"manual"}' \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["task"]["id"])')
echo "   task_id=$TID"

echo "→ タスク一覧"
curl -sf "$API/api/tasks" | python3 -c 'import sys,json; print("  count=", len(json.load(sys.stdin)["tasks"]))'

echo "→ タイトル変更"
curl -sf -X PATCH "$API/api/tasks/$TID" \
  -H "Content-Type: application/json" \
  -d '{"title":"Smoke: updated"}' >/dev/null

echo "→ 遷移 plan → archive（合法）"
curl -sf -X POST "$API/api/tasks/$TID/transition" \
  -H "Content-Type: application/json" -d '{"to":"archive"}' >/dev/null

echo "→ 遷移 plan → done（新規タスクでは非法なはず）"
curl -s -o /dev/null -w "  HTTP %{http_code}\n" -X POST "$API/api/tasks/$TID/transition" \
  -H "Content-Type: application/json" -d '{"to":"done"}'

echo "→ 削除"
curl -sf -X DELETE "$API/api/tasks/$TID" >/dev/null

echo "✓ スモークテスト成功"
