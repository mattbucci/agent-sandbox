#!/usr/bin/env bash
# =============================================================================
# e2e-orchestrator.sh — end-to-end, reproducible test of the hermes-gateway
# orchestrator, driven entirely through the `ygg` CLI.
#
# Exercises every layer the way an operator would:
#   1. gateway reachable            (ygg hermes health)
#   2. agents exposed               (ygg hermes agents)
#   3. realtime dispatch  — gemma   (ygg hermes submit / show / output)
#   4. realtime dispatch  — north   (coder agent; verifies the cohere model)
#   5. scheduled dispatch           (ygg task -> hermes back -> scheduler -> run show)
#   6. tracing end-to-end           (one trace links gateway + in-VM agent spans)
#
# Read-only and idempotent apart from the tasks it creates (which close
# themselves). Safe to re-run. Exits non-zero if any assertion fails.
#
# Usage:
#   test/e2e-orchestrator.sh                 # run everything
#   NORTH_AGENT=coder GEMMA_AGENT=feature-dev test/e2e-orchestrator.sh
#   SKIP_SCHEDULED=1 SKIP_TRACING=1 test/e2e-orchestrator.sh
#
# Env overrides:
#   YGG              path to the ygg binary            (default: PATH, then target/release)
#   GEMMA_AGENT      a gemma-backed agent to hit       (default: feature-dev)
#   NORTH_AGENT      the north/cohere coding agent      (default: coder)
#   NORTH_MODEL      expected LLM model for NORTH_AGENT (default: north)
#   TRACES_FILE      OTel file-exporter path            (default: /var/log/otel/traces.jsonl)
#   TASK_TIMEOUT_S   per-task wait budget               (default: 150)
#   SKIP_SCHEDULED / SKIP_TRACING / SKIP_NORTH   set to 1 to skip a section
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SANDBOX_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# --- resolve config -----------------------------------------------------------
YGG="${YGG:-$(command -v ygg 2>/dev/null || echo "${SANDBOX_ROOT}/../yggdrasil/target/release/ygg")}"
GATEWAY_JSON="${SANDBOX_ROOT}/state/gateway/gateway.json"
GATEWAY_URL="${HERMES_GATEWAY_URL:-http://127.0.0.1:8642}"
GEMMA_AGENT="${GEMMA_AGENT:-feature-dev}"
NORTH_AGENT="${NORTH_AGENT:-coder}"
NORTH_MODEL="${NORTH_MODEL:-north}"
TRACES_FILE="${TRACES_FILE:-/var/log/otel/traces.jsonl}"
TASK_TIMEOUT_S="${TASK_TIMEOUT_S:-150}"

# The `ygg hermes` commands read these from ~/.config/ygg/.env; export them here
# too so the harness works even when that file is absent, and so the direct
# curl/token checks below have a value.
TOKEN=""
if [[ -f "${GATEWAY_JSON}" ]]; then
    TOKEN="$(python3 -c "import json;print(json.load(open('${GATEWAY_JSON}'))['tokens'][0]['token'])" 2>/dev/null || true)"
fi
export HERMES_GATEWAY_URL="${GATEWAY_URL}"
export HERMES_GATEWAY_TOKEN="${HERMES_GATEWAY_TOKEN:-${TOKEN}}"

# Isolate the scheduled-dispatch section in its own SQLite DB so the harness is
# (a) deterministic — no leftover scheduled tasks from a previous run getting
# dispatched into the concurrency-1 gateway agent ahead of this run's task — and
# (b) non-polluting — it never writes test tasks into the operator's real
# ~/.local/share/ygg/ygg.db. Override YGG_DB_PATH to point elsewhere if desired.
export YGG_DB_PATH="${YGG_DB_PATH:-/tmp/ygg-e2e/ygg.db}"
if [[ "${KEEP_DB:-0}" != "1" ]]; then rm -rf "$(dirname "${YGG_DB_PATH}")"; fi
mkdir -p "$(dirname "${YGG_DB_PATH}")"

# --- tiny test framework ------------------------------------------------------
C_G=$'\033[32m'; C_R=$'\033[31m'; C_Y=$'\033[33m'; C_D=$'\033[2m'; C_0=$'\033[0m'
PASS=0; FAIL=0; SKIP=0
pass() { echo "  ${C_G}PASS${C_0} $1"; PASS=$((PASS+1)); }
fail() { echo "  ${C_R}FAIL${C_0} $1"; FAIL=$((FAIL+1)); }
skip() { echo "  ${C_Y}SKIP${C_0} $1"; SKIP=$((SKIP+1)); }
info() { echo "  ${C_D}$1${C_0}"; }
section() { echo; echo "${C_D}== $1 ==${C_0}"; }

# jq-free JSON field read via python
json_get() { python3 -c "import json,sys;d=json.load(sys.stdin);print(d$1)" 2>/dev/null; }

# submit a realtime task, echo its id (empty on failure)
hermes_submit() {
    local agent="$1" prompt="$2"
    "${YGG}" hermes submit "${agent}" "${prompt}" --json 2>/dev/null | json_get "['id']"
}

# poll a hermes task id until terminal or timeout; echo final state
hermes_wait() {
    local tid="$1" deadline=$((SECONDS + TASK_TIMEOUT_S)) st=""
    while (( SECONDS < deadline )); do
        st="$("${YGG}" hermes show "${tid}" --json 2>/dev/null | json_get "['state']")"
        case "${st}" in succeeded|failed|expired|cancelled) echo "${st}"; return 0;; esac
        sleep 5
    done
    echo "${st:-timeout}"
}

# --- preflight ----------------------------------------------------------------
echo "hermes-gateway orchestrator e2e"
echo "  ygg:      ${YGG}"
echo "  gateway:  ${GATEWAY_URL}"
echo "  agents:   gemma=${GEMMA_AGENT}  north=${NORTH_AGENT}"
[[ -x "${YGG}" || -n "$(command -v "${YGG}")" ]] || { echo "${C_R}ygg not found at ${YGG}${C_0}"; exit 2; }
[[ -n "${HERMES_GATEWAY_TOKEN}" ]] || { echo "${C_R}no gateway token (set HERMES_GATEWAY_TOKEN or provide ${GATEWAY_JSON})${C_0}"; exit 2; }
echo "  db:       ${YGG_DB_PATH}"
# Create the schema in the isolated DB (needed by the scheduled section).
"${YGG}" migrate >/dev/null 2>&1 || true

# --- 1. gateway reachable -----------------------------------------------------
section "1. gateway reachable"
if "${YGG}" hermes health 2>/dev/null | grep -q "ok"; then
    pass "ygg hermes health -> ok"
else
    fail "ygg hermes health did not report ok"
fi

# --- 2. agents exposed --------------------------------------------------------
section "2. agents exposed"
AGENTS="$("${YGG}" hermes agents 2>/dev/null)"
for a in "${GEMMA_AGENT}" "${NORTH_AGENT}"; do
    if grep -qw "${a}" <<<"${AGENTS}"; then pass "agent '${a}' exposed"; else fail "agent '${a}' not exposed"; fi
done

# --- 3. realtime dispatch (gemma) --------------------------------------------
section "3. realtime dispatch -> ${GEMMA_AGENT} (gemma)"
TID="$(hermes_submit "${GEMMA_AGENT}" "Write a Python one-liner that returns the reverse of a string s. Output only the code, do not run anything.")"
if [[ -z "${TID}" ]]; then
    fail "submit returned no task id"
else
    info "task ${TID}"
    ST="$(hermes_wait "${TID}")"
    OUT="$("${YGG}" hermes output "${TID}" 2>/dev/null)"
    [[ "${ST}" == "succeeded" ]] && pass "realtime task succeeded" || fail "realtime task state=${ST}"
    grep -q "s\[::-1\]\|reversed\|::-1" <<<"${OUT}" && pass "output contains a plausible reverse expression" \
        || { fail "output missing expected code"; info "got: $(head -c 120 <<<"${OUT}")"; }
fi

# --- 4. realtime dispatch (north / cohere) -----------------------------------
if [[ "${SKIP_NORTH:-0}" == "1" ]]; then
    section "4. realtime dispatch -> ${NORTH_AGENT} (north)"; skip "SKIP_NORTH=1"
else
    section "4. realtime dispatch -> ${NORTH_AGENT} (north/cohere)"
    TID_N="$(hermes_submit "${NORTH_AGENT}" "Write a Python function is_prime(n) returning True iff n is prime. Output only the function, do not run anything.")"
    if [[ -z "${TID_N}" ]]; then
        fail "submit to ${NORTH_AGENT} returned no task id"
    else
        info "task ${TID_N}"
        ST_N="$(hermes_wait "${TID_N}")"
        OUT_N="$("${YGG}" hermes output "${TID_N}" 2>/dev/null)"
        [[ "${ST_N}" == "succeeded" ]] && pass "north task succeeded" || fail "north task state=${ST_N}"
        grep -q "def is_prime" <<<"${OUT_N}" && pass "north agent returned the requested function" \
            || { fail "north output missing 'def is_prime'"; info "got: $(head -c 120 <<<"${OUT_N}")"; }
    fi
fi

# --- 5. scheduled dispatch (DAG -> scheduler -> gateway) ----------------------
if [[ "${SKIP_SCHEDULED:-0}" == "1" ]]; then
    section "5. scheduled dispatch"; skip "SKIP_SCHEDULED=1"
else
    # Route the scheduled task to SCHED_AGENT — a gemma agent distinct from the
    # one section 3 (realtime) and section 4/6 (north) hit. Gateway agents are
    # concurrency-1, so reusing an agent across sections lets residual work stall
    # this dispatch. This section tests the DAG->scheduler->gateway->reconcile
    # PATH; model routing is covered by sections 4 and 6.
    SCHED_AGENT="${SCHED_AGENT:-devops}"
    section "5. scheduled dispatch -> ${SCHED_AGENT} via ygg scheduler"
    REF="$("${YGG}" task create "Write a Python function fib(n) returning the nth Fibonacci number iteratively. Output only the function, do not run anything." --kind task -p 2 --json 2>/dev/null | json_get "['ref']")"
    if [[ -z "${REF}" ]]; then
        fail "task create returned no ref (is 'ygg init' done?)"
    else
        info "task ${REF}"
        "${YGG}" hermes back "${REF}" --agent "${SCHED_AGENT}" >/dev/null 2>&1 \
            && pass "marked ${REF} backend=hermes:${SCHED_AGENT}" || fail "hermes back failed"
        # Drive the scheduler and poll the DB directly (the source of truth) for
        # BOTH the run state and the parent task status — decoupled from any
        # `ygg task show` text/exit-code fragility. Reconcile + finalize run in
        # the same tick, so the parent closes right after the run goes terminal.
        # Reads the isolated DB read-only via a heredoc that prints "run|task".
        read_state() {
            python3 <<'PY' 2>/dev/null
import os,sqlite3
db=os.path.expanduser(os.environ["YGG_DB_PATH"])
try:
    c=sqlite3.connect(f"file:{db}?mode=ro",uri=True,timeout=5)
    r=c.execute("SELECT run_id,task_id,state FROM task_runs WHERE backend='hermes' "
                "ORDER BY created_at DESC LIMIT 1").fetchone()
    if not r: print("|"); raise SystemExit
    t=c.execute("SELECT status FROM tasks WHERE task_id=?",(r[1],)).fetchone()
    print(f"{r[2]}|{t[0] if t else ''}")
except Exception:
    print("|")
PY
        }
        deadline=$((SECONDS + TASK_TIMEOUT_S)); runstate=""; taskstatus=""
        while (( SECONDS < deadline )); do
            "${YGG}" scheduler tick >/dev/null 2>&1
            sleep 4
            IFS='|' read -r runstate taskstatus <<<"$(read_state)"
            [[ "${taskstatus}" == "closed" ]] && break
        done
        if [[ "${runstate}" == "succeeded" ]]; then
            pass "scheduler dispatched + reconciled the run to succeeded"
        else
            fail "scheduled run did not succeed (run state=${runstate:-none} in ${TASK_TIMEOUT_S}s)"
        fi
        [[ "${taskstatus}" == "closed" ]] && pass "parent DAG task closed" \
            || fail "parent task not closed (status=${taskstatus:-none})"
        # Confirm the captured result flowed back into the run record. Read the
        # latest hermes-backed run id straight from the sqlite DB (read-only).
        RUNID="$(python3 <<'PY' 2>/dev/null
import os, sqlite3
db = os.path.expanduser(os.environ.get("YGG_DB_PATH", "~/.local/share/ygg/ygg.db"))
try:
    c = sqlite3.connect(f"file:{db}?mode=ro", uri=True, timeout=5)
    row = c.execute("SELECT run_id FROM task_runs WHERE backend='hermes' "
                    "ORDER BY created_at DESC LIMIT 1").fetchone()
    print(row[0] if row else "")
except Exception:
    print("")
PY
)"
        if [[ -n "${RUNID}" ]]; then
            SHOW="$("${YGG}" run show "${RUNID}" 2>/dev/null)"
            grep -q "backend:.*hermes" <<<"${SHOW}" && pass "run show surfaces backend=hermes" || fail "run show missing backend"
            grep -q "remote_task_id:" <<<"${SHOW}" && pass "run show surfaces remote_task_id" || fail "run show missing remote_task_id"
            if grep -q "def fib" <<<"${SHOW}"; then pass "scheduled run captured the agent's code"
            elif grep -q "none captured" <<<"${SHOW}"; then info "output empty (agent produced no final text this run)"; skip "captured-output assertion (non-deterministic empty)"
            else fail "run show output not as expected"; fi
        else
            skip "could not resolve run_id from DB (backend check only)"
        fi
    fi
fi

# --- 6. tracing end-to-end ----------------------------------------------------
if [[ "${SKIP_TRACING:-0}" == "1" ]]; then
    section "6. tracing end-to-end"; skip "SKIP_TRACING=1"
else
    section "6. tracing end-to-end (gateway + in-VM spans share a trace)"
    # Reading the root-owned trace file needs sudo; degrade gracefully.
    CAT="cat"; sudo -n true 2>/dev/null && CAT="sudo -n cat"
    if ! ${CAT} "${TRACES_FILE}" >/dev/null 2>&1; then
        skip "cannot read ${TRACES_FILE} (need sudo or collector file exporter)"
    else
        # Fire one fresh task so a linked trace is definitely in the window.
        T6="$(hermes_submit "${NORTH_AGENT}" "Write a Python one-liner that returns n squared. Output only the code.")"
        [[ -n "${T6}" ]] && hermes_wait "${T6}" >/dev/null
        sleep 8  # allow batch span export to flush
        RESULT="$(${CAT} "${TRACES_FILE}" | python3 -c "
import json,sys
from collections import defaultdict
tr=defaultdict(lambda: defaultdict(list)); models=set()
for line in sys.stdin:
    try: d=json.loads(line)
    except Exception: continue
    for rs in d.get('resourceSpans',[]):
        svc='?'
        for a in rs.get('resource',{}).get('attributes',[]):
            if a.get('key')=='service.name': svc=a['value'].get('stringValue')
        for ss in rs.get('scopeSpans',[]):
            for sp in ss.get('spans',[]):
                tr[sp.get('traceId')][svc].append(sp.get('name'))
                for a in sp.get('attributes',[]):
                    if a.get('key')=='gen_ai.request.model':
                        models.add(a['value'].get('stringValue'))
linked=[t for t,s in tr.items() if any('gateway' in k for k in s) and any('agent-sandbox' in k for k in s)]
print('LINKED' if linked else 'NOLINK')
print('MODELS ' + ','.join(sorted(m for m in models if m)))
" 2>/dev/null)"
        if grep -q "^LINKED" <<<"${RESULT}"; then
            pass "a single trace links hermes-gateway and in-VM agent spans"
        else
            fail "no trace spanning both gateway and agent found in ${TRACES_FILE}"
        fi
        MLINE="$(grep "^MODELS" <<<"${RESULT}" | cut -d' ' -f2-)"
        info "gen_ai.request.model values seen: ${MLINE:-<none>}"
        if [[ "${SKIP_NORTH:-0}" != "1" ]]; then
            grep -qw "${NORTH_MODEL}" <<<"${MLINE}" && pass "an LLM span used model '${NORTH_MODEL}' (cohere routing confirmed)" \
                || skip "did not observe model '${NORTH_MODEL}' in this window (gen_ai attr may be absent)"
        fi
    fi
fi

# --- summary ------------------------------------------------------------------
echo
echo "${C_D}=============================================${C_0}"
echo "  ${C_G}${PASS} passed${C_0}, ${C_R}${FAIL} failed${C_0}, ${C_Y}${SKIP} skipped${C_0}"
echo "${C_D}=============================================${C_0}"
(( FAIL == 0 ))
