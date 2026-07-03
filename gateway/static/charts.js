/* charts.js — hand-rolled SVG line charts for the hermes-gateway dashboard.
 *
 * Design contract (dataviz skill): one y-axis per chart, 2px round-joined
 * lines, hairline solid gridlines with a slightly darker baseline, >=8px
 * hover markers with a 2px surface ring, a crosshair + shared tooltip hover
 * layer listing every series at the hovered X (values right-aligned,
 * tabular-nums, line-key swatches), a legend only when >=2 series, selective
 * labels only (the tooltip/table carry values), designed empty states, and a
 * table-view twin so no value is gated behind hover. All dynamic text is set
 * with textContent; nothing is fetched.
 */
"use strict";

const SVG_NS = "http://www.w3.org/2000/svg";

function svgEl(name, attrs) {
  const el = document.createElementNS(SVG_NS, name);
  for (const k in attrs) el.setAttribute(k, attrs[k]);
  return el;
}

function cssVar(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

/* Round a maximum up to a clean 1/2/5 step. */
function niceCeil(v) {
  if (v <= 0) return 1;
  const pow = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 5, 10]) {
    if (v <= m * pow) return m * pow;
  }
  return 10 * pow;
}

function fmtCompact(v) {
  if (v >= 1e6) return (v / 1e6).toFixed(v >= 1e7 ? 0 : 1) + "M";
  if (v >= 1e3) return (v / 1e3).toFixed(v >= 1e4 ? 0 : 1) + "k";
  if (v >= 100) return String(Math.round(v));
  if (v >= 10) return String(Math.round(v * 10) / 10);
  return String(Math.round(v * 100) / 100);
}

function fmtTime(unixSec) {
  const d = new Date(unixSec * 1000);
  return String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
}

/* One shared tooltip element for every chart on the page. */
function tooltipEl() {
  return document.getElementById("tooltip");
}

function hideTooltip() {
  const tt = tooltipEl();
  if (tt) tt.classList.add("hidden");
}

/* renderLineChart(container, opts)
 * opts: {
 *   series: [{name, color, values: number[]}],   // equal lengths, oldest first
 *   startUnix, stepS,                             // x scale
 *   unit,                                         // suffix for tooltip values
 *   emptyMessage,                                 // designed empty state
 *   legend,                                       // force legend visibility (default: series >= 2)
 * }
 */
function renderLineChart(container, opts) {
  container.textContent = "";
  const series = (opts.series || []).filter((s) => s.values && s.values.length);
  const n = series.length ? series[0].values.length : 0;
  let max = 0;
  for (const s of series) for (const v of s.values) if (v > max) max = v;

  if (!n || max <= 0) {
    const empty = document.createElement("div");
    empty.className = "chart-empty";
    empty.textContent = opts.emptyMessage || "No data in this window";
    container.appendChild(empty);
    return;
  }

  const width = Math.max(container.clientWidth || 560, 280);
  const padL = 42, padR = 10, padT = 10, padB = 24;
  const plotH = 160;
  const height = padT + plotH + padB;
  const plotW = width - padL - padR;
  const yMax = niceCeil(max);

  const xAt = (i) => padL + (n === 1 ? plotW / 2 : (i / (n - 1)) * plotW);
  const yAt = (v) => padT + plotH - (v / yMax) * plotH;

  const svg = svgEl("svg", {
    viewBox: "0 0 " + width + " " + height,
    width: width,
    height: height,
    role: "img",
  });

  const gridColor = cssVar("--grid");
  const baseColor = cssVar("--baseline");
  const inkMuted = cssVar("--text-muted");
  const surface = cssVar("--surface-1");

  // Horizontal hairline gridlines + y tick labels (they carry the values).
  const ySteps = 4;
  for (let i = 1; i <= ySteps; i++) {
    const v = (yMax / ySteps) * i;
    const y = yAt(v);
    svg.appendChild(svgEl("line", { x1: padL, y1: y, x2: padL + plotW, y2: y, stroke: gridColor, "stroke-width": 1 }));
    const lbl = svgEl("text", { x: padL - 6, y: y + 3.5, "text-anchor": "end", "font-size": 10.5, fill: inkMuted });
    lbl.textContent = fmtCompact(v);
    svg.appendChild(lbl);
  }
  // Baseline, one step darker than the grid.
  svg.appendChild(svgEl("line", {
    x1: padL, y1: padT + plotH, x2: padL + plotW, y2: padT + plotH,
    stroke: baseColor, "stroke-width": 1,
  }));

  // X time ticks.
  const xTicks = Math.min(4, n);
  for (let i = 0; i < xTicks; i++) {
    const idx = Math.round((i / Math.max(xTicks - 1, 1)) * (n - 1));
    const lbl = svgEl("text", {
      x: xAt(idx), y: padT + plotH + 15,
      "text-anchor": i === 0 ? "start" : i === xTicks - 1 ? "end" : "middle",
      "font-size": 10.5, fill: inkMuted, "font-variant-numeric": "tabular-nums",
    });
    lbl.textContent = fmtTime(opts.startUnix + idx * opts.stepS);
    svg.appendChild(lbl);
  }

  // Series lines: 2px, round joins/caps.
  for (const s of series) {
    let d = "";
    for (let i = 0; i < n; i++) {
      d += (i ? " L" : "M") + xAt(i).toFixed(1) + " " + yAt(s.values[i]).toFixed(1);
    }
    svg.appendChild(svgEl("path", {
      d: d, fill: "none", stroke: s.color, "stroke-width": 2,
      "stroke-linejoin": "round", "stroke-linecap": "round",
    }));
  }

  // Hover layer: crosshair + markers + shared tooltip. The hit target is the
  // whole plot, far bigger than the marks.
  const crosshair = svgEl("line", { y1: padT, y2: padT + plotH, stroke: baseColor, "stroke-width": 1, visibility: "hidden" });
  svg.appendChild(crosshair);
  const markers = series.map((s) => {
    const m = svgEl("circle", { r: 4.5, fill: s.color, stroke: surface, "stroke-width": 2, visibility: "hidden" });
    svg.appendChild(m);
    return m;
  });

  const hit = svgEl("rect", { x: padL, y: padT, width: plotW, height: plotH, fill: "transparent" });
  hit.style.cursor = "crosshair";
  svg.appendChild(hit);

  function onMove(ev) {
    const rect = svg.getBoundingClientRect();
    const sx = (ev.clientX - rect.left) * (width / rect.width);
    let idx = Math.round(((sx - padL) / plotW) * (n - 1));
    idx = Math.max(0, Math.min(n - 1, idx));
    const cx = xAt(idx);
    crosshair.setAttribute("x1", cx);
    crosshair.setAttribute("x2", cx);
    crosshair.setAttribute("visibility", "visible");
    series.forEach((s, i) => {
      markers[i].setAttribute("cx", cx);
      markers[i].setAttribute("cy", yAt(s.values[idx]));
      markers[i].setAttribute("visibility", "visible");
    });

    const tt = tooltipEl();
    if (!tt) return;
    tt.textContent = "";
    const title = document.createElement("div");
    title.className = "tt-title tnum";
    title.textContent = fmtTime(opts.startUnix + idx * opts.stepS);
    tt.appendChild(title);
    for (const s of series) {
      const row = document.createElement("div");
      row.className = "tt-row";
      const key = document.createElement("span");
      key.className = "tt-key";
      key.style.borderTopColor = s.color;
      const name = document.createElement("span");
      name.className = "tt-name";
      name.textContent = s.name;
      const val = document.createElement("span");
      val.className = "tt-val";
      val.textContent = fmtCompact(s.values[idx]) + (opts.unit || "");
      row.appendChild(key);
      row.appendChild(name);
      row.appendChild(val);
      tt.appendChild(row);
    }
    tt.classList.remove("hidden");
    const ttW = tt.offsetWidth, ttH = tt.offsetHeight;
    let left = ev.clientX + 14;
    if (left + ttW > window.innerWidth - 8) left = ev.clientX - ttW - 14;
    let top = ev.clientY - ttH / 2;
    top = Math.max(8, Math.min(window.innerHeight - ttH - 8, top));
    tt.style.left = left + "px";
    tt.style.top = top + "px";
  }

  function onLeave() {
    crosshair.setAttribute("visibility", "hidden");
    for (const m of markers) m.setAttribute("visibility", "hidden");
    hideTooltip();
  }

  hit.addEventListener("pointermove", onMove);
  hit.addEventListener("pointerleave", onLeave);

  container.appendChild(svg);

  // Legend: required for >=2 series; a single-series chart gets none (the
  // panel title names it). Text wears ink tokens; the swatch carries identity.
  const wantLegend = opts.legend !== undefined ? opts.legend : series.length >= 2;
  if (wantLegend && series.length >= 2) {
    const legend = document.createElement("div");
    legend.className = "legend";
    for (const s of series) {
      const item = document.createElement("span");
      item.className = "lg-item";
      const key = document.createElement("span");
      key.className = "lg-key";
      key.style.borderTopColor = s.color;
      const name = document.createElement("span");
      name.textContent = s.name;
      item.appendChild(key);
      item.appendChild(name);
      legend.appendChild(item);
    }
    container.appendChild(legend);
  }
}

/* renderChartTable(container, opts) — the accessibility/table-view twin of a
 * line chart: the most recent 12 buckets, one row per time, one column per
 * series. Every charted value is reachable without hovering. */
function renderChartTable(container, opts) {
  container.textContent = "";
  const series = (opts.series || []).filter((s) => s.values && s.values.length);
  const n = series.length ? series[0].values.length : 0;
  if (!n) {
    const empty = document.createElement("div");
    empty.className = "chart-empty";
    empty.textContent = opts.emptyMessage || "No data in this window";
    container.appendChild(empty);
    return;
  }
  const rowsN = Math.min(12, n);
  const tbl = document.createElement("table");
  tbl.className = "dtable";
  const thead = document.createElement("thead");
  const hr = document.createElement("tr");
  const thTime = document.createElement("th");
  thTime.textContent = "time";
  hr.appendChild(thTime);
  for (const s of series) {
    const th = document.createElement("th");
    th.className = "num";
    th.textContent = s.name;
    hr.appendChild(th);
  }
  thead.appendChild(hr);
  tbl.appendChild(thead);
  const tbody = document.createElement("tbody");
  for (let r = n - rowsN; r < n; r++) {
    const tr = document.createElement("tr");
    const tdT = document.createElement("td");
    tdT.className = "num";
    tdT.textContent = fmtTime(opts.startUnix + r * opts.stepS);
    tr.appendChild(tdT);
    for (const s of series) {
      const td = document.createElement("td");
      td.className = "num";
      td.textContent = fmtCompact(s.values[r]) + (opts.unit || "");
      tr.appendChild(td);
    }
    tbody.appendChild(tr);
  }
  tbl.appendChild(tbody);
  container.appendChild(tbl);
}
