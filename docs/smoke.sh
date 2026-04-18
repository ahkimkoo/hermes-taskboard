#!/usr/bin/env bash
# Quick API smoke test — assumes hermes-taskboard is running on 127.0.0.1:1900.
set -euo pipefail
API="${API:-http://127.0.0.1:1900}"

echo "→ healthz"
curl -sf "$API/healthz" >/dev/null

echo "→ create task"
TID=$(curl -sf -X POST "$API/api/tasks" \
  -H "Content-Type: application/json" \
  -d '{"title":"Smoke: hello","description":"Hi Hermes","priority":3,"status":"plan","trigger_mode":"manual"}' \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["task"]["id"])')
echo "   task_id=$TID"

echo "→ list tasks"
curl -sf "$API/api/tasks" | python3 -c 'import sys,json; print("  count=", len(json.load(sys.stdin)["tasks"]))'

echo "→ patch title"
curl -sf -X PATCH "$API/api/tasks/$TID" \
  -H "Content-Type: application/json" \
  -d '{"title":"Smoke: updated"}' >/dev/null

echo "→ transition plan → archive (legal)"
curl -sf -X POST "$API/api/tasks/$TID/transition" \
  -H "Content-Type: application/json" -d '{"to":"archive"}' >/dev/null

echo "→ transition plan → done (should be illegal on a fresh task)"
curl -s -o /dev/null -w "  HTTP %{http_code}\n" -X POST "$API/api/tasks/$TID/transition" \
  -H "Content-Type: application/json" -d '{"to":"done"}'

echo "→ delete"
curl -sf -X DELETE "$API/api/tasks/$TID" >/dev/null

echo "✓ smoke ok"
