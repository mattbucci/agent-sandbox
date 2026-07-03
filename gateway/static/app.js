/* app.js — hermes-gateway ops dashboard.
 *
 * Token: prompt -> localStorage -> Authorization header on every fetch (no
 * cookies). Polling: overview 2s, tasks 5s, timeseries 10s, traces 10s,
 * egress 15s; x2 backoff to 30s on failure; paused while document.hidden.
 * Rendering is textContent-only — squid URLs, span names and task content are
 * attacker-influenced. Zero external fetches: every request is a relative
 * /dashboard/api/* path.
 *
 * Color: each agent takes a fixed categorical slot (--series-1..8) by its
 * position in the gateway's sorted agent list, so an agent keeps its color on
 * every panel and across reloads; a 9th+ agent folds into "Other". Status
 * colors are reserved for state and always paired with an icon glyph + label.
 */
"use strict";

const TOKEN_KEY = "hgwDashToken";
const MAX_BACKOFF = 30000;

const state = {
  token: localStorage.getItem(TOKEN_KEY) || "",
  agents: [],            // sorted agent names from the overview
  overview: null,
  timeseries: null,
  chartTables: {},       // chart id -> bool (table view toggled)
  drawerTaskId: null,
  taskStateFilter: "",
};

/* ---------- tiny DOM helpers (textContent only) ---------- */

function el(tag, className, text) {
  const e = document.createElement(tag);
  if (className) e.className = className;
  if (text !== undefined) e.textContent = text;
  return e;
}

function byId(id) { return document.getElementById(id); }

function clearEl(e) { e.textContent = ""; }

function fmtInt(v) {
  return Number(v || 0).toLocaleString("en-US");
}

function fmtBytes(v) {
  v = Number(v || 0);
  if (v >= 1 << 30) return (v / (1 << 30)).toFixed(1) + " GiB";
  if (v >= 1 << 20) return (v / (1 << 20)).toFixed(1) + " MiB";
  if (v >= 1 << 10) return (v / (1 << 10)).toFixed(1) + " KiB";
  return v + " B";
}

function fmtAge(sec) {
  sec = Math.max(0, Math.floor(sec));
  if (sec < 60) return sec + "s";
  if (sec < 3600) return Math.floor(sec / 60) + "m" + (sec % 60) + "s";
  if (sec < 86400) return Math.floor(sec / 3600) + "h" + Math.floor((sec % 3600) / 60) + "m";
  return Math.floor(sec / 86400) + "d" + Math.floor((sec % 86400) / 3600) + "h";
}

function fmtClock(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return "";
  return d.toLocaleTimeString("en-GB", { hour12: false });
}

function shortId(id, n) {
  n = n || 12;
  return id && id.length > n ? id.slice(0, n) + "…" : (id || "");
}

/* status badge: icon glyph + label, never color alone */
const STATUS_GLYPH = { good: "●", warning: "▲", serious: "■", critical: "✖", neutral: "○" };

function statusBadge(kind, label) {
  const s = el("span", "status " + kind);
  s.appendChild(el("span", "s-ico", STATUS_GLYPH[kind] || STATUS_GLYPH.neutral));
  s.appendChild(el("span", "", label));
  return s;
}

const TASK_STATE_BADGE = {
  pending: ["warning", "pending"],
  running: ["good", "running"],
  succeeded: ["good", "succeeded"],
  failed: ["critical", "failed"],
  cancelled: ["neutral", "cancelled"],
  expired: ["serious", "expired"],
};

function taskBadge(st) {
  const [kind, label] = TASK_STATE_BADGE[st] || ["neutral", st];
  return statusBadge(kind, label);
}

/* ---------- agent slot colors ---------- */

function slotColor(agent) {
  const idx = state.agents.indexOf(agent);
  if (idx < 0 || idx >= 8) return cssVar("--series-other");
  return cssVar("--series-" + (idx + 1));
}

function agentSwatch(agent) {
  const s = el("span", "swatch");
  s.style.background = slotColor(agent);
  return s;
}

/* fold 9+ agent series into "Other" for charts */
function chartSeriesForAgents(valueOf) {
  const out = [];
  let other = null;
  state.agents.forEach((agent, idx) => {
    const values = valueOf(agent);
    if (!values) return;
    if (idx < 8) {
      out.push({ name: agent, color: slotColor(agent), values: values });
    } else {
      if (!other) other = { name: "Other", color: cssVar("--series-other"), values: values.slice() };
      else values.forEach((v, i) => { other.values[i] += v; });
    }
  });
  if (other) out.push(other);
  return out;
}

/* ---------- token overlay + fetch ---------- */

function showTokenOverlay(msg) {
  byId("token-msg").textContent = msg || "Enter the ops bearer token for this gateway.";
  byId("token-overlay").classList.remove("hidden");
  byId("token-input").focus();
}

byId("token-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  const v = byId("token-input").value.trim();
  if (!v) return;
  state.token = v;
  localStorage.setItem(TOKEN_KEY, v);
  byId("token-overlay").classList.add("hidden");
  kickAll();
});

byId("token-change").addEventListener("click", () => {
  byId("token-input").value = "";
  showTokenOverlay();
});

async function api(path, opts) {
  const headers = {};
  if (state.token) headers["Authorization"] = "Bearer " + state.token;
  const res = await fetch(path, Object.assign({ headers: headers }, opts || {}));
  if (res.status === 401) {
    showTokenOverlay("Token rejected — enter a valid dashboard token.");
    throw new Error("unauthorized");
  }
  if (res.status === 403) {
    showTokenOverlay("The gateway has no dashboard token configured (dashboard.tokens is empty).");
    throw new Error("forbidden");
  }
  if (!res.ok) throw new Error("http " + res.status);
  return res;
}

async function apiJSON(path) { return (await api(path)).json(); }

/* ---------- pollers: cadence + backoff + hidden-pause ---------- */

const pollers = [];

function makePoller(baseMs, fn, staleTargets) {
  const p = {
    interval: baseMs,
    timer: null,
    async tick() {
      // No token yet: don't fire unauthenticated fetches (they would 401,
      // overwrite the overlay prompt and inflate auth_failures_total).
      if (document.hidden || !state.token) { p.schedule(); return; }
      try {
        await fn();
        p.interval = baseMs;
        (staleTargets || []).forEach((id) => {
          const t = byId(id);
          if (t) t.classList.remove("stale");
        });
      } catch (err) {
        p.interval = Math.min(p.interval * 2, MAX_BACKOFF);
        // Hold the previous render at reduced opacity — no blank flash.
        (staleTargets || []).forEach((id) => {
          const t = byId(id);
          if (t) t.classList.add("stale");
        });
      }
      p.schedule();
    },
    schedule() {
      clearTimeout(p.timer);
      p.timer = setTimeout(() => p.tick(), p.interval);
    },
    kick() {
      clearTimeout(p.timer);
      p.interval = baseMs;
      return p.tick();
    },
  };
  pollers.push(p);
  return p;
}

document.addEventListener("visibilitychange", () => {
  if (!document.hidden) kickAll();
});

/* Kick the overview first and await it so state.agents is populated before
 * the dependent panels (charts, swatch colors) render, then kick the rest. */
async function kickAll() {
  await overviewPoller.kick();
  pollers.forEach((p) => { if (p !== overviewPoller) p.kick(); });
}

/* ---------- table builder ---------- */

/* cols: [{h, num?}], rows: array of arrays of (Node|string|{num,text}) */
function buildTable(cols, rows, emptyMsg) {
  if (!rows.length) return el("div", "empty-note", emptyMsg);
  const tbl = el("table", "dtable");
  const thead = el("thead");
  const hr = el("tr");
  cols.forEach((c) => {
    const th = el("th", c.num ? "num" : "", c.h);
    hr.appendChild(th);
  });
  thead.appendChild(hr);
  tbl.appendChild(thead);
  const tbody = el("tbody");
  rows.forEach((r) => {
    const tr = el("tr");
    if (r.onclick) { tr.classList.add("clickable"); tr.addEventListener("click", r.onclick); }
    (r.cells || r).forEach((cell, i) => {
      const td = el("td", cols[i] && cols[i].num ? "num" : "");
      if (cell instanceof Node) td.appendChild(cell);
      else if (cell && typeof cell === "object" && "text" in cell) {
        td.textContent = cell.text;
        if (cell.cls) td.className += " " + cell.cls;
      } else td.textContent = cell === undefined || cell === null ? "" : String(cell);
      tr.appendChild(td);
    });
    tbody.appendChild(tr);
  });
  tbl.appendChild(tbody);
  return tbl;
}

function degradedNote(detail) {
  return el("div", "degraded-note", "source unavailable: " + (detail || "unknown error"));
}

/* ---------- overview ---------- */

function renderOverview(o) {
  state.overview = o;
  state.agents = o.agents.map((a) => a.agent); // server-sorted, stable slots

  byId("gw-version").textContent = o.gateway.version ? "v" + o.gateway.version + " · pid " + o.gateway.pid : "";
  byId("gw-uptime").textContent = "up " + fmtAge(o.gateway.uptime_s);

  const dot = byId("collector-dot");
  const coll = o.deps.collector || { ok: false, detail: "" };
  dot.classList.toggle("ok", !!coll.ok);
  dot.classList.toggle("bad", !coll.ok);
  dot.title = coll.detail || "";

  renderDepsBanner(o);
  renderStatStrip(o);
  renderAgentCards(o);
  renderQueueTable(o);
}

function renderDepsBanner(o) {
  const banner = byId("deps-banner");
  clearEl(banner);
  const problems = [];
  for (const name of ["collector", "traces_file", "squid_log", "tasks_dir"]) {
    const d = o.deps[name];
    if (d && !d.ok) problems.push(name.replace("_", " ") + ": " + (d.detail || "unavailable"));
  }
  if (o.store_degraded) problems.push("task store degraded: a record failed its last persist (retrying)");
  if ((o.tasks_by_state || {}).orphaned > 0) problems.push(o.tasks_by_state.orphaned + " orphaned task(s) reference unconfigured agents");
  if (!problems.length) { banner.classList.add("hidden"); return; }
  banner.classList.remove("hidden");
  problems.forEach((p) => {
    const row = el("div", "b-row");
    row.appendChild(statusBadge("serious", p));
    banner.appendChild(row);
  });
}

function statTile(label, value, sub, warn) {
  const t = el("div", "tile" + (warn ? " warn" : ""));
  t.appendChild(el("div", "t-label", label));
  t.appendChild(el("div", "t-value", value));
  if (sub) t.appendChild(el("div", "t-sub", sub));
  return t;
}

function renderStatStrip(o) {
  const strip = byId("stat-strip");
  clearEl(strip);
  let running = 0, queued = 0, vmsUp = 0;
  o.agents.forEach((a) => {
    running += a.running.length;
    queued += a.waiting.length;
    if (a.vm && a.vm.alive) vmsUp++;
  });
  const ts = o.tasks_by_state || {};
  strip.appendChild(statTile("Requests (1m)", fmtInt(o.totals.reqs_1m)));
  strip.appendChild(statTile("Errors (1m)", fmtInt(o.totals.errors_1m), null, o.totals.errors_1m > 0));
  strip.appendChild(statTile("p95 (1m)", o.totals.p95_ms_1m > 0 ? fmtCompact(o.totals.p95_ms_1m) : "–", "ms"));
  strip.appendChild(statTile("Running", fmtInt(running)));
  strip.appendChild(statTile("Queued", fmtInt(queued)));
  strip.appendChild(statTile("Tasks pending", fmtInt(ts.pending || 0)));
  strip.appendChild(statTile("Tasks failed", fmtInt(ts.failed || 0), null, (ts.failed || 0) > 0));
  strip.appendChild(statTile("VMs up", fmtInt(vmsUp), "of " + o.agents.length));
}

function renderAgentCards(o) {
  const wrap = byId("agent-cards");
  clearEl(wrap);
  if (!o.agents.length) {
    wrap.appendChild(el("div", "empty-note", "No agents configured."));
    return;
  }
  o.agents.forEach((a) => {
    const card = el("div", "agent-card");
    const head = el("div", "a-head");
    head.appendChild(agentSwatch(a.agent));
    head.appendChild(el("span", "a-name", a.agent));
    card.appendChild(head);

    const vmRow = el("div", "a-row");
    vmRow.appendChild(el("span", "", "vm"));
    if (a.vm && a.vm.alive) vmRow.appendChild(statusBadge("good", a.vm.instance_id + " @ " + a.vm.vm_ip));
    else vmRow.appendChild(statusBadge("critical", "no live VM"));
    card.appendChild(vmRow);

    const addRow = (k, v) => {
      const r = el("div", "a-row");
      r.appendChild(el("span", "", k));
      r.appendChild(el("span", "v tnum", v));
      card.appendChild(r);
    };
    addRow("running / limit", a.running.length + " / " + a.limit);
    addRow("queued (cap " + a.queue_cap + ")", String(a.waiting.length));
    addRow("granted", fmtInt(a.counters.granted));
    addRow("rejected / timeouts", fmtInt(a.counters.rejected_full) + " / " + fmtInt(a.counters.wait_timeouts));

    if (a.last_error) {
      const err = el("div", "a-err");
      err.title = a.last_error.message || "";
      err.textContent = "last error (" + (a.last_error.status || "?") + "): " + (a.last_error.message || "");
      card.appendChild(err);
    }
    wrap.appendChild(card);
  });
}

function renderQueueTable(o) {
  const wrap = byId("queue-table");
  clearEl(wrap);
  const rows = [];
  o.agents.forEach((a) => {
    a.running.forEach((r) => {
      rows.push([
        agentCell(a.agent),
        statusBadge("good", "running"),
        { text: r.kind },
        { text: shortId(r.task_id || r.run_id, 24), cls: "mono" },
        { text: "" },
        { text: shortId(r.session_id, 20), cls: "mono" },
        { text: fmtAge(r.age_s) },
        { text: shortId(r.trace_id, 10), cls: "mono" },
      ]);
    });
    a.waiting.forEach((wq) => {
      rows.push([
        agentCell(a.agent),
        statusBadge("warning", "waiting"),
        { text: wq.kind },
        { text: shortId(wq.task_id || wq.id, 24), cls: "mono" },
        { text: String(wq.priority) },
        { text: "" },
        { text: fmtAge(wq.wait_s) },
        { text: "" },
      ]);
    });
  });
  wrap.appendChild(buildTable(
    [{ h: "agent" }, { h: "state" }, { h: "kind" }, { h: "id" }, { h: "pri", num: true },
     { h: "session" }, { h: "age/wait", num: true }, { h: "trace" }],
    rows, "Nothing running or queued."));
}

function agentCell(agent) {
  const span = el("span", "");
  span.appendChild(agentSwatch(agent));
  span.appendChild(document.createTextNode(" " + agent));
  return span;
}

/* ---------- charts ---------- */

function renderCharts(t) {
  state.timeseries = t;
  const opts = { startUnix: t.start_unix, stepS: t.step_s };

  drawChart("requests", byId("chart-requests"), {
    ...opts,
    series: chartSeriesForAgents((a) => (t.series[a] || {}).count),
    emptyMessage: "No traffic in the last hour",
  });
  drawChart("errors", byId("chart-errors"), {
    ...opts,
    series: chartSeriesForAgents((a) => (t.series[a] || {}).errors),
    emptyMessage: "No errors in the last hour",
  });
  const total = t.series._total || {};
  drawChart("latency", byId("chart-latency"), {
    ...opts,
    unit: " ms",
    series: [
      { name: "avg", color: cssVar("--deemph"), values: total.lat_ms_avg || [] },
      { name: "p95", color: cssVar("--series-1"), values: total.lat_ms_p95 || [] },
    ],
    emptyMessage: "No traffic in the last hour",
  });
}

function drawChart(id, container, cfg) {
  if (!container) return;
  // Drop all-zero error series noise: keep series with any signal, but always
  // keep at least the first so the empty state has something to judge.
  let series = cfg.series || [];
  if (id !== "latency") {
    const withSignal = series.filter((s) => s.values && s.values.some((v) => v > 0));
    if (withSignal.length) series = withSignal;
  }
  const finalCfg = Object.assign({}, cfg, { series: series });
  if (state.chartTables[id]) renderChartTable(container, finalCfg);
  else renderLineChart(container, finalCfg);
}

document.querySelectorAll(".table-toggle").forEach((btn) => {
  btn.addEventListener("click", () => {
    const id = btn.dataset.chart;
    state.chartTables[id] = !state.chartTables[id];
    btn.textContent = state.chartTables[id] ? "chart" : "table";
    if (state.timeseries) renderCharts(state.timeseries);
  });
});

/* ---------- tasks ---------- */

byId("task-state-filter").addEventListener("change", (ev) => {
  state.taskStateFilter = ev.target.value;
  tasksPoller.kick();
});

function tasksURL() {
  let u = "/dashboard/api/tasks?limit=100";
  if (state.taskStateFilter) u += "&state=" + encodeURIComponent(state.taskStateFilter);
  return u;
}

function renderTasks(res) {
  const wrap = byId("tasks-table");
  clearEl(wrap);
  if (!res.available) {
    wrap.appendChild(degradedNote(res.detail));
    return;
  }
  const rows = res.data.map((t) => ({
    onclick: () => openDrawer(t.id),
    cells: [
      { text: shortId(t.id, 30), cls: "mono pri" },
      agentCell(t.agent),
      taskBadge(t.state),
      { text: String(t.priority) },
      { text: String(t.attempts) },
      { text: fmtAge(t.age_s) },
      { text: fmtClock(t.updated_at) },
      { text: t.error ? t.error.kind : "" },
    ],
  }));
  wrap.appendChild(buildTable(
    [{ h: "task" }, { h: "agent" }, { h: "state" }, { h: "pri", num: true }, { h: "att", num: true },
     { h: "age", num: true }, { h: "updated", num: true }, { h: "error" }],
    rows, "No tasks" + (state.taskStateFilter ? " in this state." : " yet.")));
}

/* ---------- task drawer ---------- */

async function openDrawer(id) {
  state.drawerTaskId = id;
  const drawer = byId("task-drawer");
  drawer.classList.remove("hidden");
  clearEl(drawer);
  drawer.appendChild(el("div", "muted", "Loading…"));
  try {
    const [detail, output] = await Promise.all([
      apiJSON("/dashboard/api/tasks/" + encodeURIComponent(id)),
      api("/dashboard/api/tasks/" + encodeURIComponent(id) + "/output").then((r) => r.text()).catch(() => ""),
    ]);
    if (state.drawerTaskId !== id) return;
    renderDrawer(detail, output);
  } catch (err) {
    clearEl(drawer);
    drawer.appendChild(el("div", "degraded-note", "Failed to load task: " + err.message));
    drawer.appendChild(closeBtn());
  }
}

function closeBtn() {
  const b = el("button", "ghost-btn", "close");
  b.type = "button";
  b.addEventListener("click", closeDrawer);
  return b;
}

function closeDrawer() {
  state.drawerTaskId = null;
  byId("task-drawer").classList.add("hidden");
}

document.addEventListener("keydown", (ev) => {
  if (ev.key === "Escape") closeDrawer();
});

function renderDrawer(t, output) {
  const drawer = byId("task-drawer");
  clearEl(drawer);

  const head = el("div", "d-head");
  const title = el("h2", "mono", t.id);
  head.appendChild(title);
  head.appendChild(closeBtn());
  drawer.appendChild(head);

  const stateRow = el("div", "");
  stateRow.appendChild(taskBadge(t.state));
  if (t.cancel_requested && t.state === "running") {
    stateRow.appendChild(document.createTextNode("  "));
    stateRow.appendChild(statusBadge("warning", "cancel requested"));
  }
  drawer.appendChild(stateRow);

  const dl = el("dl", "d-grid");
  const field = (k, v) => {
    dl.appendChild(el("dt", "", k));
    dl.appendChild(el("dd", "", v === undefined || v === null || v === "" ? "–" : String(v)));
  };
  field("agent", t.agent);
  field("priority", t.priority);
  field("attempts", t.attempts + " / " + t.max_attempts);
  field("created", t.created_at);
  field("updated", t.updated_at);
  field("deadline", t.deadline);
  field("session", t.session_id);
  field("submitted by", t.submitted_by);
  field("timeout / idle", t.timeout_s + "s / " + t.idle_timeout_s + "s");
  if (t.trace_ids && t.trace_ids.length) field("trace ids", t.trace_ids.join(", "));
  if (t.error) field("error", t.error.kind + ": " + t.error.message);
  if (t.result) field("finish reason", t.result.finish_reason);
  drawer.appendChild(dl);

  if (!["succeeded", "failed", "cancelled", "expired"].includes(t.state)) {
    const cancel = el("button", "danger-btn", t.cancel_requested ? "cancel requested" : "cancel task");
    cancel.type = "button";
    cancel.disabled = !!t.cancel_requested;
    cancel.addEventListener("click", async () => {
      cancel.disabled = true;
      try {
        await api("/dashboard/api/tasks/" + encodeURIComponent(t.id) + "/cancel", { method: "POST" });
        openDrawer(t.id);
        tasksPoller.kick();
      } catch (err) {
        cancel.disabled = false;
      }
    });
    drawer.appendChild(cancel);
  }

  drawer.appendChild(el("h3", "", "Request"));
  drawer.appendChild(el("pre", "", t.request_preview || "(empty)"));

  drawer.appendChild(el("h3", "", "Output"));
  drawer.appendChild(el("pre", "", output || "(no output yet)"));

  if (t.attempt_history && t.attempt_history.length) {
    drawer.appendChild(el("h3", "", "Attempts"));
    const rows = t.attempt_history.map((a) => [
      { text: String(a.attempt) },
      { text: a.outcome || "running" },
      { text: fmtClock(a.started_at) },
      { text: a.ended_at ? fmtClock(a.ended_at) : "" },
      { text: fmtBytes(a.output_bytes) },
      { text: a.error || "" },
    ]);
    drawer.appendChild(buildTable(
      [{ h: "#", num: true }, { h: "outcome" }, { h: "start", num: true }, { h: "end", num: true },
       { h: "output", num: true }, { h: "error" }],
      rows, ""));
  }
}

/* ---------- traces ---------- */

function renderTraces(res) {
  const wrap = byId("traces-panel");
  clearEl(wrap);
  if (!res.available) {
    wrap.appendChild(degradedNote(res.detail));
    return;
  }
  const rows = res.traces.map((tr) => [
    { text: shortId(tr.trace_id, 16), cls: "mono" },
    { text: tr.root_service },
    { text: tr.root_name },
    tr.error ? statusBadge("critical", "error") : statusBadge("good", "ok"),
    { text: fmtCompact(tr.duration_ms) + " ms" },
    { text: String(tr.span_count) },
    { text: (tr.services || []).join(", ") },
    { text: fmtClock(tr.start) },
  ]);
  wrap.appendChild(buildTable(
    [{ h: "trace" }, { h: "root service" }, { h: "root span" }, { h: "status" },
     { h: "duration", num: true }, { h: "spans", num: true }, { h: "services" }, { h: "start", num: true }],
    rows, "No traces in the last " + Math.round((res.window_s || 900) / 60) + " minutes."));
  const meta = el("div", "empty-note",
    res.parsed_lines + " lines parsed" + (res.skipped_lines ? ", " + res.skipped_lines + " skipped" : ""));
  wrap.appendChild(meta);
}

/* ---------- egress ---------- */

function renderEgress(res) {
  const wrap = byId("egress-panel");
  const deniedWrap = byId("egress-denied");
  clearEl(wrap);
  clearEl(deniedWrap);
  if (!res.available) {
    wrap.appendChild(degradedNote(res.detail));
    deniedWrap.appendChild(degradedNote(res.detail));
    return;
  }

  const tiles = el("div", "tile-row");
  tiles.appendChild(statTile("Requests", fmtInt(res.totals.requests), "last " + Math.round(res.window_s / 60) + "m"));
  tiles.appendChild(statTile("Denied", fmtInt(res.totals.denied), null, res.totals.denied > 0));
  tiles.appendChild(statTile("Bytes", fmtBytes(res.totals.bytes)));
  wrap.appendChild(tiles);

  wrap.appendChild(el("h3", "", "By agent"));
  wrap.appendChild(buildTable(
    [{ h: "agent" }, { h: "vm ip" }, { h: "requests", num: true }, { h: "denied", num: true }, { h: "bytes", num: true }],
    res.by_agent.map((a) => [
      a.agent === "unknown" ? { text: "unknown" } : agentCell(a.agent),
      { text: a.vm_ip, cls: "mono" },
      { text: fmtInt(a.requests) },
      { text: fmtInt(a.denied) },
      { text: fmtBytes(a.bytes) },
    ]), "No egress traffic in this window."));

  wrap.appendChild(el("h3", "", "Top hosts"));
  wrap.appendChild(buildTable(
    [{ h: "host" }, { h: "requests", num: true }, { h: "bytes", num: true }, { h: "denied", num: true }],
    res.top_hosts.map((h) => [
      { text: h.host, cls: "mono" },
      { text: fmtInt(h.requests) },
      { text: fmtBytes(h.bytes) },
      { text: fmtInt(h.denied) },
    ]), "No egress traffic in this window."));

  if (!res.denied.length) {
    const okNote = el("div", "empty-note");
    okNote.appendChild(statusBadge("good", "No denied requests in this window"));
    deniedWrap.appendChild(okNote);
  } else {
    deniedWrap.appendChild(buildTable(
      [{ h: "time", num: true }, { h: "agent" }, { h: "host" }, { h: "method" }, { h: "result" }],
      res.denied.map((d) => [
        { text: fmtClock(d.ts) },
        d.agent === "unknown" ? { text: "unknown" } : agentCell(d.agent),
        { text: d.host, cls: "mono" },
        { text: d.method },
        statusBadge("critical", d.result),
      ]), ""));
  }
}

/* ---------- wire the pollers ---------- */

const overviewPoller = makePoller(2000,
  async () => renderOverview(await apiJSON("/dashboard/api/overview")),
  ["stat-strip", "agent-cards", "queue-table"]);

const tasksPoller = makePoller(5000,
  async () => renderTasks(await apiJSON(tasksURL())),
  ["tasks-table"]);

const timeseriesPoller = makePoller(10000,
  async () => renderCharts(await apiJSON("/dashboard/api/timeseries?window_s=3600")),
  ["chart-requests", "chart-errors", "chart-latency"]);

const tracesPoller = makePoller(10000,
  async () => renderTraces(await apiJSON("/dashboard/api/traces?limit=50&window_s=900")),
  ["traces-panel"]);

const egressPoller = makePoller(15000,
  async () => renderEgress(await apiJSON("/dashboard/api/egress?window_s=900")),
  ["egress-panel", "egress-denied"]);

if (!state.token) showTokenOverlay();
kickAll();
