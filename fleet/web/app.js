/* Estoria Fleet — vanilla JS client.
 *
 * The client is deliberately thin: all state lives in the event streams on
 * the server. The browser holds only the latest per-device state (pushed over
 * SSE) and re-renders the affected card in place — the grid is never rebuilt
 * wholesale.
 *
 * All charts are hand-rolled SVG: sparklines on the cards, two stacked small
 * multiples in the drawer, and the hydration-benchmark bars. Series colors
 * are fixed: temperature #d95926, humidity #3987e5; benchmark bars are
 * [cold #3987e5, snapshot #d95926, cached #199e70].
 */

"use strict";

const $ = (sel) => document.querySelector(sel);

const SERIES = { temp: "#d95926", humidity: "#3987e5", aqua: "#199e70" };
const BENCH = { cold: "#3987e5", snapshot: "#d95926", cached: "#199e70" };
const GRID_LINE = "#2a2f3a";
const TEXT = "#e6e9ef";
const MUTED = "#8b93a3";

const state = {
  devices: new Map(), // id -> {version, device}
  cards: new Map(),   // id -> element refs for in-place updates
  stats: null,
  totalEvents: 0,     // ticks up live via SSE, resynced by the stats poll
  drawerId: null,
  snapshotVersion: 0, // for the device open in the drawer
  benchRunning: false,
};

/* ============ bootstrap ============ */

async function init() {
  wireChrome();

  const res = await fetch("/api/fleet");
  if (!res.ok) {
    toast("Failed to load fleet", "error");
    return;
  }
  for (const msg of await res.json()) {
    upsertDevice(msg, { quiet: true });
  }

  await pollStats();
  setInterval(pollStats, 2500);
  connect();
}

/* ============ live updates (SSE) ============ */

function connect() {
  const es = new EventSource("/api/watch");

  es.onopen = () => setPill("● live", "live");

  es.onmessage = (e) => {
    upsertDevice(JSON.parse(e.data));
  };

  es.onerror = () => setPill("reconnecting…", "warn"); // EventSource retries itself
}

function setPill(text, cls) {
  const pill = $("#conn-pill");
  pill.textContent = text;
  pill.className = "pill" + (cls ? " " + cls : "");
}

/* ============ device state ============ */

// upsertDevice merges one device message into local state and updates only
// the DOM that depends on it.
function upsertDevice(msg, { quiet = false } = {}) {
  const id = msg.device.id;
  const prev = state.devices.get(id);
  if (prev && msg.version <= prev.version) return; // stale or duplicate

  if (prev) {
    state.totalEvents += msg.version - prev.version;
    renderHeaderCounts();
    if (!quiet) alertToasts(prev.device, msg.device);
  }

  state.devices.set(id, msg);

  if (!state.cards.has(id)) {
    addCard(id);
  }
  updateCard(id);

  if (state.drawerId === id) {
    renderDrawer();
  }
}

// alertToasts announces alert transitions (AlertRaised / AlertCleared events
// arriving via SSE).
function alertToasts(before, after) {
  const was = before.activeAlerts || {};
  const now = after.activeAlerts || {};
  for (const kind of Object.keys(now)) {
    if (!(kind in was)) toast(`🔥 ${after.name}: ${now[kind]}`, "alert", "An <code>AlertRaised</code> event was appended to this device's stream.");
  }
  for (const kind of Object.keys(was)) {
    if (!(kind in now)) toast(`✅ ${after.name}: ${kind} cleared`, "cleared");
  }
}

/* ============ fleet grid ============ */

function addCard(id) {
  const root = document.createElement("article");
  root.className = "device-card";
  root.dataset.id = id;
  root.addEventListener("click", () => openDrawer(id));

  const top = document.createElement("div");
  top.className = "card-top";
  const dot = document.createElement("span");
  dot.className = "status-dot";
  const name = document.createElement("span");
  name.className = "device-name";
  const alert = document.createElement("span");
  alert.className = "alert-badge hidden";
  alert.textContent = "🔥 overheat";
  top.append(dot, name, alert);

  const sub = document.createElement("div");
  sub.className = "device-sub";

  const mid = document.createElement("div");
  mid.className = "card-mid";
  const temp = document.createElement("div");
  temp.className = "temp-big";
  const spark = document.createElement("div");
  spark.className = "sparkline";
  mid.append(temp, spark);

  const bottom = document.createElement("div");
  bottom.className = "card-bottom";
  const battery = document.createElement("span");
  battery.className = "battery";
  const battBar = document.createElement("span");
  battBar.className = "batt-bar";
  const battFill = document.createElement("span");
  battFill.className = "batt-fill";
  battBar.appendChild(battFill);
  const battText = document.createElement("span");
  battery.append(battBar, battText);
  const version = document.createElement("span");
  version.className = "card-version";
  bottom.append(battery, version);

  root.append(top, sub, mid, bottom);
  state.cards.set(id, { root, dot, name, alert, sub, temp, spark, battFill, battText, version });

  // insert in name order without rebuilding the grid
  const grid = $("#grid");
  const mine = state.devices.get(id).device.name;
  let before = null;
  for (const el of grid.children) {
    const other = state.devices.get(el.dataset.id);
    if (other && other.device.name > mine) {
      before = el;
      break;
    }
  }
  grid.insertBefore(root, before);
}

function updateCard(id) {
  const { version, device } = state.devices.get(id);
  const refs = state.cards.get(id);

  refs.name.textContent = device.name;
  refs.name.title = device.name;
  refs.sub.textContent = `${device.model} · ${device.location}`;
  refs.dot.classList.toggle("online", device.status === "online");
  refs.root.classList.toggle("offline", device.status !== "online");
  refs.alert.classList.toggle("hidden", !hasAlert(device));

  if (device.readingCount > 0) {
    refs.temp.innerHTML = "";
    refs.temp.append(device.lastReading.tempC.toFixed(1), smallUnit("°C"));
  } else {
    refs.temp.textContent = "—";
  }

  renderSparkline(refs.spark, device.readings || []);

  const pct = Math.round(device.batteryPct);
  refs.battFill.style.width = pct + "%";
  refs.battFill.className = "batt-fill" + (pct < 20 ? " crit" : pct < 50 ? " low" : "");
  refs.battText.textContent = pct + "%";

  refs.version.textContent = "v" + version.toLocaleString();
  refs.version.title = `${version.toLocaleString()} events in this device's stream`;
}

function smallUnit(text) {
  const el = document.createElement("small");
  el.textContent = text;
  return el;
}

function hasAlert(device) {
  return Object.keys(device.activeAlerts || {}).length > 0;
}

/* ============ sparkline ============ */

// A sparkline is the minimal chart: one 2px temperature line, no axes, no
// grid, ~110×32.
function renderSparkline(host, readings) {
  host.innerHTML = "";
  if (readings.length < 2) return;

  const W = 110, H = 32, pad = 2;
  const temps = readings.map((r) => r.tempC);
  let lo = Math.min(...temps), hi = Math.max(...temps);
  if (hi - lo < 1) { const mid = (hi + lo) / 2; lo = mid - 0.5; hi = mid + 0.5; }

  const x = (i) => pad + (i / (readings.length - 1)) * (W - 2 * pad);
  const y = (v) => pad + (1 - (v - lo) / (hi - lo)) * (H - 2 * pad);
  const d = temps.map((v, i) => `${i ? "L" : "M"}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join("");

  const svg = svgEl("svg", { width: W, height: H, viewBox: `0 0 ${W} ${H}` });
  svg.appendChild(svgEl("path", {
    d, fill: "none", stroke: SERIES.temp, "stroke-width": 2,
    "stroke-linejoin": "round", "stroke-linecap": "round",
  }));
  host.appendChild(svg);
}

/* ============ detail drawer ============ */

function openDrawer(id) {
  if (state.drawerId) {
    state.cards.get(state.drawerId)?.root.classList.remove("selected");
  }
  state.drawerId = id;
  state.snapshotVersion = 0;
  $("#bench").innerHTML = "";
  state.cards.get(id)?.root.classList.add("selected");
  $("#drawer").classList.remove("hidden");
  renderDrawer();
  fetchDetail(id);
}

function closeDrawer() {
  if (state.drawerId) {
    state.cards.get(state.drawerId)?.root.classList.remove("selected");
  }
  state.drawerId = null;
  $("#drawer").classList.add("hidden");
}

// fetchDetail fills in the one thing SSE messages don't carry: the version of
// the device's latest snapshot.
async function fetchDetail(id) {
  const res = await fetch(`/api/devices/${id}`);
  if (!res.ok || state.drawerId !== id) return;
  const msg = await res.json();
  state.snapshotVersion = msg.snapshotVersion || 0;
  upsertDevice(msg, { quiet: true });
  renderDrawer();
}

function renderDrawer() {
  const entry = state.devices.get(state.drawerId);
  if (!entry) return;
  const { version, device } = entry;

  $("#drawer-name").textContent = device.name;
  $("#drawer-dot").className = "status-dot" + (device.status === "online" ? " online" : "");
  $("#drawer-alert").classList.toggle("hidden", !hasAlert(device));

  const banner = $("#alert-banner");
  if (hasAlert(device)) {
    banner.textContent = Object.entries(device.activeAlerts)
      .map(([kind, msg]) => `🔥 ${kind}: ${msg}`).join(" · ");
    banner.classList.remove("hidden");
  } else {
    banner.classList.add("hidden");
  }

  renderMeta(version, device);

  const points = (device.readings || []).map((r) => ({ t: Date.parse(r.at), v: r.tempC }));
  const humPoints = (device.readings || []).map((r) => ({ t: Date.parse(r.at), v: r.humidity }));
  renderLineChart($("#chart-temp"), { color: SERIES.temp, unit: "°C", points });
  renderLineChart($("#chart-hum"), { color: SERIES.humidity, unit: "%", points: humPoints });
}

function renderMeta(version, device) {
  const pct = Math.round(device.batteryPct);
  const battCls = pct < 20 ? "crit" : pct < 50 ? "low" : "ok";
  const snapshotText = state.snapshotVersion > 0
    ? `v${state.snapshotVersion.toLocaleString()} · replays ${(version - state.snapshotVersion).toLocaleString()} events`
    : "none yet";

  const rows = [
    ["Model", device.model],
    ["Location", device.location],
    ["Firmware", device.firmware, "mono"],
    ["Battery", `<span class="batt-val ${battCls}">${pct}%</span>`],
    ["Status", device.status],
    ["Readings recorded", device.readingCount.toLocaleString()],
    ["Temp range", device.readingCount > 0 ? `${device.minTempC.toFixed(1)}–${device.maxTempC.toFixed(1)} °C` : "—"],
    ["Humidity now", device.readingCount > 0 ? `${device.lastReading.humidity.toFixed(1)} %` : "—"],
    ["Stream version", "v" + version.toLocaleString(), "mono"],
    ["Latest snapshot", snapshotText, "mono"],
  ];

  const grid = $("#drawer-meta");
  grid.innerHTML = "";
  for (const [label, value, cls] of rows) {
    const item = document.createElement("div");
    item.className = "meta-item";
    const l = document.createElement("div");
    l.className = "meta-label";
    l.textContent = label;
    const v = document.createElement("div");
    v.className = "meta-value" + (cls ? " " + cls : "");
    v.innerHTML = value;
    item.append(l, v);
    grid.appendChild(item);
  }
}

/* ============ line charts (drawer small multiples) ============ */

// renderLineChart draws one single-series line chart: one y-axis, recessive
// horizontal grid lines, a 2px line, and a hover layer (crosshair + nearest-
// point marker + tooltip) spanning the full chart height. The two drawer
// charts share the same x window because they render the same ring buffer.
function renderLineChart(host, { color, unit, points }) {
  host.innerHTML = "";

  if (points.length < 2) {
    const empty = document.createElement("div");
    empty.className = "chart-empty";
    empty.textContent = "waiting for readings…";
    host.appendChild(empty);
    return;
  }

  const W = host.clientWidth || 450, H = 150;
  const pad = { l: 42, r: 8, t: 8, b: 20 };
  const plotW = W - pad.l - pad.r, plotH = H - pad.t - pad.b;

  const t0 = points[0].t, t1 = points[points.length - 1].t;
  const values = points.map((p) => p.v);
  let lo = Math.min(...values), hi = Math.max(...values);
  const span = (hi - lo) || 1;
  lo -= span * 0.15;
  hi += span * 0.15;

  const x = (t) => pad.l + ((t - t0) / (t1 - t0 || 1)) * plotW;
  const y = (v) => pad.t + (1 - (v - lo) / (hi - lo)) * plotH;

  const svg = svgEl("svg", { width: W, height: H, viewBox: `0 0 ${W} ${H}` });

  // horizontal grid lines + y tick labels (one y-axis, text in theme colors)
  for (const tick of ticks(lo, hi, 4)) {
    const ty = y(tick);
    svg.appendChild(svgEl("line", {
      x1: pad.l, y1: ty, x2: W - pad.r, y2: ty,
      stroke: GRID_LINE, "stroke-width": 1,
    }));
    svg.appendChild(svgText(pad.l - 6, ty + 3.5, fmtNum(tick), {
      "text-anchor": "end", "font-size": 10.5, fill: MUTED,
    }));
  }

  // x tick labels: start, middle, end of the window
  for (const t of [t0, (t0 + t1) / 2, t1]) {
    svg.appendChild(svgText(x(t), H - 6, fmtTime(t), {
      "text-anchor": t === t0 ? "start" : t === t1 ? "end" : "middle",
      "font-size": 10.5, fill: MUTED,
    }));
  }

  // the series line
  const d = points.map((p, i) => `${i ? "L" : "M"}${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join("");
  svg.appendChild(svgEl("path", {
    d, fill: "none", stroke: color, "stroke-width": 2,
    "stroke-linejoin": "round", "stroke-linecap": "round",
  }));

  // hover layer: crosshair + marker (markers appear only on hover)
  const crosshair = svgEl("line", {
    y1: pad.t, y2: H - pad.b, stroke: MUTED, "stroke-width": 1, opacity: 0,
  });
  const marker = svgEl("circle", {
    r: 3.5, fill: color, stroke: "#0f1117", "stroke-width": 1.5, opacity: 0,
  });
  svg.append(crosshair, marker);

  const tooltip = document.createElement("div");
  tooltip.className = "chart-tooltip";
  tooltip.style.display = "none";
  host.appendChild(tooltip);

  // hit target: the full chart area
  const overlay = svgEl("rect", { x: 0, y: 0, width: W, height: H, fill: "transparent" });
  overlay.addEventListener("mousemove", (e) => {
    const box = svg.getBoundingClientRect();
    const mx = e.clientX - box.left;
    let nearest = points[0];
    for (const p of points) {
      if (Math.abs(x(p.t) - mx) < Math.abs(x(nearest.t) - mx)) nearest = p;
    }
    const px = x(nearest.t), py = y(nearest.v);
    crosshair.setAttribute("x1", px);
    crosshair.setAttribute("x2", px);
    crosshair.setAttribute("opacity", 0.5);
    marker.setAttribute("cx", px);
    marker.setAttribute("cy", py);
    marker.setAttribute("opacity", 1);
    tooltip.textContent = `${fmtTime(nearest.t)} · ${nearest.v.toFixed(1)}${unit}`;
    tooltip.style.display = "block";
    const flip = px > W - 120;
    tooltip.style.left = flip ? "" : px + 10 + "px";
    tooltip.style.right = flip ? W - px + 10 + "px" : "";
    tooltip.style.top = Math.max(py - 30, 0) + "px";
  });
  overlay.addEventListener("mouseleave", () => {
    crosshair.setAttribute("opacity", 0);
    marker.setAttribute("opacity", 0);
    tooltip.style.display = "none";
  });
  svg.appendChild(overlay);

  host.appendChild(svg);
}

/* ============ hydration benchmark ============ */

async function runBenchmark() {
  if (!state.drawerId || state.benchRunning) return;
  const id = state.drawerId;
  state.benchRunning = true;
  const btn = $("#bench-btn");
  btn.disabled = true;
  btn.textContent = "Running…";

  try {
    const res = await fetch(`/api/devices/${id}/benchmark`);
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      toast(err.error || "Benchmark failed", "error");
      return;
    }
    const result = await res.json();
    state.snapshotVersion = result.snapshotVersion || 0;
    if (state.drawerId === id) {
      renderBench($("#bench"), result);
      renderDrawer(); // refresh the snapshot row in the meta grid
    }
  } finally {
    state.benchRunning = false;
    btn.disabled = false;
    btn.textContent = "Run hydration benchmark";
  }
}

// renderBench draws the three timed loads as horizontal bars with direct
// value labels. Colors are fixed per load type: cold #3987e5, snapshot
// #d95926, cached #199e70.
function renderBench(host, res) {
  host.innerHTML = "";

  const replayed = res.eventCount - res.snapshotVersion;
  const rows = [
    {
      name: "Cold replay", color: BENCH.cold, micros: res.coldMicros,
      note: `replayed ${res.eventCount.toLocaleString()} events`,
    },
    {
      name: "From snapshot", color: BENCH.snapshot, micros: res.snapshotMicros,
      note: res.snapshotVersion > 0 ? `snapshot + ${replayed.toLocaleString()} events` : "no snapshot yet — full replay",
    },
    {
      name: "From cache", color: BENCH.cached, micros: res.cachedMicros,
      note: "cache hit",
    },
  ];

  const W = host.clientWidth || 450;
  const rowH = 48, barH = 14;
  const svg = svgEl("svg", { width: W, height: rows.length * rowH - 8, viewBox: `0 0 ${W} ${rows.length * rowH - 8}` });
  const maxMicros = Math.max(...rows.map((r) => r.micros), 1);

  const tooltip = document.createElement("div");
  tooltip.className = "chart-tooltip";
  tooltip.style.display = "none";
  host.appendChild(tooltip);

  rows.forEach((row, i) => {
    const yTop = i * rowH;

    svg.appendChild(svgText(0, yTop + 11, row.name, { "font-size": 11, fill: MUTED }));

    const label = svgText(W, yTop + 11, "", { "text-anchor": "end", "font-size": 11.5, fill: MUTED });
    const value = svgEl("tspan", { "font-weight": 600, fill: TEXT });
    value.textContent = fmtMicros(row.micros);
    const note = svgEl("tspan", {});
    note.textContent = ` — ${row.note}`;
    label.append(value, note);
    svg.appendChild(label);

    // track, then the bar: rounded on the value end only
    svg.appendChild(svgEl("rect", {
      x: 0, y: yTop + 18, width: W, height: barH, rx: 4,
      fill: "rgba(42, 47, 58, 0.45)",
    }));
    const barW = Math.max((row.micros / maxMicros) * W, 5);
    svg.appendChild(svgEl("path", { d: barPath(0, yTop + 18, barW, barH, 4), fill: row.color }));

    // per-bar hover tooltip with the exact measurement
    const hit = svgEl("rect", { x: 0, y: yTop, width: W, height: rowH - 8, fill: "transparent" });
    hit.addEventListener("mousemove", (e) => {
      const box = svg.getBoundingClientRect();
      tooltip.textContent = `${row.micros.toLocaleString()} µs`;
      tooltip.style.display = "block";
      tooltip.style.left = Math.min(e.clientX - box.left + 12, W - 90) + "px";
      tooltip.style.top = Math.max(yTop - 6, 0) + "px";
      tooltip.style.right = "";
    });
    hit.addEventListener("mouseleave", () => { tooltip.style.display = "none"; });
    svg.appendChild(hit);
  });

  host.appendChild(svg);
}

// barPath builds a bar rounded on the value (right) end only.
function barPath(x, y, w, h, r) {
  r = Math.min(r, w, h / 2);
  return `M${x},${y} h${w - r} a${r},${r} 0 0 1 ${r},${r} v${h - 2 * r} a${r},${r} 0 0 1 -${r},${r} h${-(w - r)} z`;
}

async function evictFromCache() {
  if (!state.drawerId) return;
  const res = await fetch(`/api/devices/${state.drawerId}/evict`, { method: "POST" });
  if (!res.ok) {
    toast("Evict failed", "error");
    return;
  }
  const { evicted } = await res.json();
  toast(
    evicted ? "Cache entry evicted" : "Not in cache",
    "snapshot",
    "The next load falls through to the <code>SnapshottingStore</code> — run the benchmark now, then again to see the cache re-populated.",
  );
}

/* ============ stats & panel ============ */

async function pollStats() {
  const res = await fetch("/api/stats").catch(() => null);
  if (!res || !res.ok) return;

  const prev = state.stats;
  state.stats = await res.json();
  state.totalEvents = state.stats.totalEvents;

  renderHeaderCounts();
  renderSimButton();
  renderPanel();

  if (prev && state.stats.snapshotEvents > prev.snapshotEvents) {
    toast(
      "📸 Snapshot written",
      "snapshot",
      `${state.stats.snapshotEvents.toLocaleString()} snapshot events across the fleet. ` +
      "Loads replay only the events after each device's latest snapshot.",
    );
    if (state.drawerId) fetchDetail(state.drawerId);
  }
}

function renderHeaderCounts() {
  const s = state.stats;
  $("#stat-devices").innerHTML = `<strong>${state.devices.size}</strong> devices`;
  $("#stat-events").innerHTML = `<strong>${state.totalEvents.toLocaleString()}</strong> events`;
  $("#stat-rate").innerHTML = s ? `<strong>${s.eventsPerSec.toFixed(1)}</strong> ev/s` : "…";
}

function renderSimButton() {
  const btn = $("#sim-btn");
  const running = state.stats?.simRunning;
  btn.textContent = running ? "⏸ Pause sim" : "▶ Resume sim";
  btn.className = "btn ghost " + (running ? "sim-running" : "sim-paused");
}

function renderPanel() {
  const s = state.stats;
  if (!s) return;

  const stack = $("#stack");
  stack.innerHTML = "";
  for (const layer of s.storeStack) {
    const li = document.createElement("li");
    li.textContent = layer;
    stack.appendChild(li);
  }

  $("#totals").innerHTML =
    `<strong>${s.deviceCount}</strong> devices · <strong>${s.deviceEvents.toLocaleString()}</strong> device events<br>` +
    `<strong>${s.snapshotEvents.toLocaleString()}</strong> snapshot events (every <code>${s.snapshotEvery}</code> events per device)<br>` +
    `<strong>${s.eventsPerSec.toFixed(1)}</strong> events/sec across the fleet`;

  const streams = $("#streams");
  streams.innerHTML = "";
  for (const stream of s.streams) {
    const row = document.createElement("div");
    row.className = "stream-row";
    const id = document.createElement("span");
    id.className = "stream-id" + (stream.id.startsWith("devicesnapshot") ? " snapshot" : "");
    id.textContent = stream.id;
    id.title = stream.id;
    const version = document.createElement("span");
    version.className = "stream-version";
    version.textContent = "@" + stream.version.toLocaleString();
    row.append(id, version);
    streams.appendChild(row);
  }
}

/* ============ chrome ============ */

function wireChrome() {
  $("#panel-toggle").addEventListener("click", () => {
    $("#panel").classList.toggle("hidden");
  });

  $("#drawer-close").addEventListener("click", closeDrawer);
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") closeDrawer();
  });

  $("#bench-btn").addEventListener("click", runBenchmark);
  $("#evict-btn").addEventListener("click", evictFromCache);

  $("#sim-btn").addEventListener("click", async () => {
    const target = state.stats?.simRunning ? "stop" : "start";
    const res = await fetch(`/api/sim/${target}`, { method: "POST" });
    if (res.ok) {
      const { running } = await res.json();
      if (state.stats) state.stats.simRunning = running;
      renderSimButton();
      toast(running ? "▶ Simulator running" : "⏸ Simulator paused",
        running ? "snapshot" : "");
    }
  });
}

/* ============ toasts & helpers ============ */

function toast(title, cls = "", detail = "") {
  const el = document.createElement("div");
  el.className = "toast" + (cls ? " " + cls : "");

  const t = document.createElement("div");
  t.className = "toast-title";
  t.textContent = title;
  el.appendChild(t);

  if (detail) {
    const d = document.createElement("div");
    d.className = "toast-detail";
    d.innerHTML = detail;
    el.appendChild(d);
  }

  $("#toasts").appendChild(el);
  setTimeout(() => el.remove(), 7000);
}

function svgEl(name, attrs = {}) {
  const el = document.createElementNS("http://www.w3.org/2000/svg", name);
  for (const [key, value] of Object.entries(attrs)) {
    el.setAttribute(key, value);
  }
  return el;
}

function svgText(x, y, content, attrs = {}) {
  const el = svgEl("text", { x, y, "font-family": "inherit", ...attrs });
  el.textContent = content;
  return el;
}

// ticks picks ~count round values spanning [lo, hi].
function ticks(lo, hi, count) {
  const span = (hi - lo) || 1;
  const rough = span / count;
  const mag = Math.pow(10, Math.floor(Math.log10(rough)));
  const norm = rough / mag;
  const step = (norm >= 5 ? 10 : norm >= 2 ? 5 : norm >= 1 ? 2 : 1) * mag;
  const out = [];
  for (let v = Math.ceil(lo / step) * step; v <= hi + 1e-9; v += step) {
    out.push(v);
  }
  return out;
}

function fmtNum(v) {
  return Math.abs(v) >= 100 || Number.isInteger(v) ? String(Math.round(v)) : v.toFixed(1);
}

function fmtTime(ms) {
  return new Date(ms).toLocaleTimeString([], { hour12: false });
}

function fmtMicros(micros) {
  return micros >= 1000 ? (micros / 1000).toFixed(1) + " ms" : micros + " µs";
}

init();
