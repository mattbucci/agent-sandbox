#!/usr/bin/env bash
# gateway-smoke.sh — manual end-to-end smoke test for the hermes-gateway
# scheduling / task / observability / dashboard upgrade (plan §h).
#
# RUN THIS BY HAND, ON THE HOST, AS ROOT. It needs live agent VMs, the real
# compiled state/gateway/gateway.json, and (for steps 6-7) the otel collector,
# squid, and the dashboard. It is NOT part of `go test` and is never run by CI.
#
# Usage:
#   sudo test/gateway-smoke.sh            # run all steps, pausing between them
#   sudo test/gateway-smoke.sh 3          # start at step 3
#
# Each step prints PASS/FAIL criteria; the operator judges the manual parts.

set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GATEWAY_URL="${GATEWAY_URL:-http://127.0.0.1:8642}"
GATEWAY_JSON="${GATEWAY_JSON:-$REPO/state/gateway/gateway.json}"
AGENT="${SMOKE_AGENT:-feature-dev}"
START_STEP="${1:-1}"

# ---------------------------------------------------------------- helpers ---

say()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
note() { printf '\033[0;33m   %s\033[0m\n' "$*"; }
pause() {
  printf '\n\033[1;35m-- %s --\033[0m\n' "$*"
  read -r -p "   Press Enter when done/observed... " _
}

# Read the first gateway bearer token out of the compiled gateway.json.
# (Never echo it into logs beyond curl's own use.)
token() {
  python3 - "$GATEWAY_JSON" <<'EOF'
import json, sys
cfg = json.load(open(sys.argv[1]))
print(cfg["tokens"][0]["token"])
EOF
}

TOKEN="$(token)"

gcurl() { # authed curl against the gateway
  curl -sS -H "Authorization: Bearer $TOKEN" "$@"
}

chat_body() { # $1 = prompt
  printf '{"model":"%s","messages":[{"role":"user","content":"%s"}],"stream":true}' "$AGENT" "$1"
}

submit_task() { # $1 = priority, $2 = prompt -> prints task id
  gcurl -X POST "$GATEWAY_URL/v1/tasks" \
    -H 'Content-Type: application/json' \
    -d "{\"agent\":\"$AGENT\",\"input\":\"$2\",\"priority\":$1}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])'
}

task_state() { # $1 = task id
  gcurl "$GATEWAY_URL/v1/tasks/$1" \
    | python3 -c 'import json,sys; t=json.load(sys.stdin); print(t.get("state","?"))'
}

step() { [ "$1" -ge "$START_STEP" ]; }

# ------------------------------------------------------------------ steps ---

if step 1; then
say "STEP 1: compile, build, start; legacy wire contract unchanged"
note "Rebuilding config + gateway and restarting."
"$REPO/bin/sandbox-ctl" gateway compile
"$REPO/bin/sandbox-ctl" gateway build
"$REPO/bin/sandbox-ctl" gateway stop || true
"$REPO/bin/sandbox-ctl" gateway start
sleep 1

echo "-- /health (expect exactly {\"status\":\"ok\"}):"
curl -sS "$GATEWAY_URL/health"
echo "-- /v1/capabilities:"
curl -sS "$GATEWAY_URL/v1/capabilities"
echo "-- /v1/models (authed):"
gcurl "$GATEWAY_URL/v1/models"
echo "-- one normal chat (expect SSE chunks then data: [DONE]):"
gcurl -N -H 'Content-Type: application/json' -H 'Accept: text/event-stream' \
  -d "$(chat_body 'Say hello in one short sentence.')" \
  "$GATEWAY_URL/v1/chat/completions" | head -c 2000; echo
pause "PASS if: health/capabilities/models bodies unchanged and webui chat still works"
fi

if step 2; then
say "STEP 2: sync queueing, then 429 when the queue is full"
note "Default concurrency=1, sync_queue_max=4: request 1 runs, 2-5 queue, 6+ get 429."
note "Firing 7 concurrent chats; watch the timing and the final statuses."
for i in $(seq 1 7); do
  {
    code=$(gcurl -o "/tmp/smoke-chat-$i.out" -w '%{http_code}' \
      -H 'Content-Type: application/json' \
      -d "$(chat_body "Chat #$i: count to 20 slowly.")" \
      "$GATEWAY_URL/v1/chat/completions")
    echo "chat #$i -> HTTP $code"
    if [ "$code" = "429" ]; then
      echo "chat #$i body: $(cat /tmp/smoke-chat-$i.out)"
    fi
  } &
done
wait
echo "-- Retry-After header on a saturated request:"
gcurl -sS -D - -o /dev/null -H 'Content-Type: application/json' \
  -d "$(chat_body 'quick')" "$GATEWAY_URL/v1/chat/completions" | grep -i '^retry-after' || true
pause "PASS if: exactly one runs at a time, later ones queue, overflow gets 429 + Retry-After + rate_limit_error envelope"
fi

if step 3; then
say "STEP 3: task priorities 0/10/5 run in order 10, 5, 0; output grows"
T0=$(submit_task 0  'Task P0: write a 4-line poem about queues.')
T10=$(submit_task 10 'Task P10: write a 4-line poem about priorities.')
T5=$(submit_task 5  'Task P5: write a 4-line poem about schedulers.')
echo "submitted: P0=$T0 P10=$T10 P5=$T5"
note "Watch started_at order: P10 first, then P5, then P0 (concurrency=1)."
for i in $(seq 1 30); do
  echo "t+${i}s: P10=$(task_state "$T10") P5=$(task_state "$T5") P0=$(task_state "$T0")"
  sleep 1
done
echo "-- P10 record (check started_at vs the others):"
gcurl "$GATEWAY_URL/v1/tasks/$T10" | python3 -m json.tool | head -40
echo "-- watching P0 output grow (two snapshots):"
gcurl "$GATEWAY_URL/v1/tasks/$T0/output" | wc -c
sleep 3
gcurl "$GATEWAY_URL/v1/tasks/$T0/output" | wc -c
pause "PASS if: run order was 10,5,0 and /output grew between snapshots"
fi

if step 4; then
say "STEP 4: crash recovery (kill -9) and clean-stop parity"
TK=$(submit_task 0 'Long task: write a 30-line story, slowly.')
echo "submitted $TK; waiting for it to start..."
until [ "$(task_state "$TK")" = "running" ]; do sleep 1; done
note "kill -9 the gateway mid-attempt:"
pkill -9 -f hermes-gateway || true
sleep 1
"$REPO/bin/sandbox-ctl" gateway start
sleep 2
echo "-- state after restart (spool empty => pending retry with not_before;"
echo "   spool non-empty + retry_on_partial=false => failed(interrupted)):"
gcurl "$GATEWAY_URL/v1/tasks/$TK" | python3 -m json.tool | grep -E '"state"|"error"|"not_before"|"attempts"'
pause "Observe the recovered state, then continue for the clean-stop half"

TK2=$(submit_task 0 'Second long task: write another slow 30-line story.')
until [ "$(task_state "$TK2")" = "running" ]; do sleep 1; done
note "clean stop (SIGTERM) mid-attempt:"
"$REPO/bin/sandbox-ctl" gateway stop
"$REPO/bin/sandbox-ctl" gateway start
sleep 2
gcurl "$GATEWAY_URL/v1/tasks/$TK2" | python3 -m json.tool | grep -E '"state"|"error"|"not_before"|"attempts"'
pause "PASS if: both interruption paths land in the same outcome class per the recovery matrix"
fi

if step 5; then
say "STEP 5: cancel a running task tears down the VM stream"
TC=$(submit_task 0 'Cancel me: write a very long essay, take your time.')
until [ "$(task_state "$TC")" = "running" ]; do sleep 1; done
echo "-- cancelling $TC:"
gcurl -X POST "$GATEWAY_URL/v1/tasks/$TC/cancel" | python3 -m json.tool | grep -E '"state"|"cancel_requested"'
sleep 2
echo "-- final state (expect cancelled):"
task_state "$TC"
echo "-- cancel again (expect already_terminal:true):"
gcurl -X POST "$GATEWAY_URL/v1/tasks/$TC/cancel" | grep -o '"already_terminal":true' || echo "MISSING already_terminal"
pause "PASS if: state=cancelled quickly and the VM stopped generating (check VM logs / CPU)"
fi

if step 6; then
say "STEP 6: observability degrades without touching routing (needs G2/DASH)"
note "a) stop the collector; chat must still work; drop counter must climb:"
systemctl stop otelcol-contrib 2>/dev/null || pkill -f otelcol || true
gcurl -N -H 'Content-Type: application/json' \
  -d "$(chat_body 'still alive?')" "$GATEWAY_URL/v1/chat/completions" | tail -c 300; echo
curl -sS "$GATEWAY_URL/metrics" | grep -E 'otlp_spans_dropped_total|otlp_export_batches_total' || \
  note "(/metrics not present until LANE-G2 lands)"
pause "Restart the collector, then continue"
note "b) make traces.jsonl unreadable; dashboard trace panel must show available:false:"
chmod 000 /var/log/otel/traces.jsonl 2>/dev/null || note "(no traces.jsonl on this host)"
pause "Check /dashboard traces panel shows a degraded banner; everything else fine"
chmod 644 /var/log/otel/traces.jsonl 2>/dev/null || true
note "c) move the squid log; egress panel must degrade, routing untouched:"
mv /var/log/squid/access.log /var/log/squid/access.log.smoke 2>/dev/null || note "(no squid log)"
pause "Check egress panel degrades gracefully"
mv /var/log/squid/access.log.smoke /var/log/squid/access.log 2>/dev/null || true
fi

if step 7; then
say "STEP 7: dashboard from a LAN machine (needs LANE-DASH)"
cat <<EOF
   Manually, from another machine on the LAN:
     1. Open http://<host>:8642/dashboard/  -> token prompt (no data without it)
     2. Enter a dashboard token from config/sandbox.yaml (dashboard.tokens)
     3. All panels populate; browser devtools network tab shows ZERO external
        fetches (no CDN, no fonts)
     4. Provoke a squid deny from inside a VM, e.g.:
          curl https://definitely-not-allowlisted.example.com
        -> a red denied row appears in the egress panel
EOF
pause "PASS if all four manual checks hold"
fi

say "Smoke test complete."
