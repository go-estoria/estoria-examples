/* Estoria Inspector — vanilla JS client.
 *
 * Strictly read-only: every request this file makes is a GET. The client
 * discovers the backend's optional capabilities from /api/info and degrades
 * gracefully — no stream list means manual stream-ID entry; no global feed
 * means no feed tab.
 */

"use strict";

const $ = (sel) => document.querySelector(sel);

const STREAM_PAGE = 50;
const FEED_PAGE = 100;
const TAIL_INTERVAL_MS = 2000;

const state = {
  info: null,       // /api/info response
  streams: [],      // [{id, type, version}] when listStreams is available
  filter: "",

  // stream view
  selected: null,   // stream ID string
  dir: "reverse",   // "reverse" (newest first, default) | "forward"
  events: [],       // current page(s), in display order
  hasMore: false,
  nextAfter: 0,

  // global feed, kept ascending by global position (newest last)
  feed: [],
  feedLoaded: false,
  feedMaxPos: 0,    // highest global position seen (tail cursor)
  feedTotal: 0,

  tailTimer: null,
  activeTab: "streams",
};

/* ============ bootstrap ============ */

async function init() {
  wireChrome();

  const info = await getJSON("/api/info");
  if (!info) return;
  state.info = info;

  const pill = $("#backend-pill");
  pill.textContent = info.label;
  pill.title = info.dsn;
  pill.className = "pill backend";

  capBadge($("#cap-liststreams"), "ListStreams", info.capabilities.listStreams);
  capBadge($("#cap-readall"), "ReadAll", info.capabilities.readAll);

  if (info.capabilities.listStreams) {
    $("#stream-browser").hidden = false;
    await loadStreams();
  } else {
    $("#stream-manual").hidden = false;
  }

  if (info.capabilities.readAll) {
    $("#tab-feed").hidden = false;
    $("#tail-toggle").hidden = false;
  }
}

function capBadge(el, name, available) {
  el.hidden = false;
  el.textContent = `${name} ${available ? "✓" : "✗"}`;
  el.classList.add(available ? "yes" : "no");
  el.title = available
    ? `This backend exposes ${name} as a store-specific extra`
    : `This backend (or its adapter) does not provide ${name}; the UI degrades gracefully`;
}

/* ============ sidebar: stream list ============ */

async function loadStreams() {
  const res = await getJSON("/api/streams");
  if (!res) return;
  state.streams = res.streams;
  renderStreamList();
}

function renderStreamList() {
  const root = $("#stream-list");
  root.innerHTML = "";

  const filter = state.filter.toLowerCase();
  const visible = state.streams.filter((s) => s.id.toLowerCase().includes(filter));

  // group by stream type (the list arrives sorted by type, then ID)
  const groups = new Map();
  for (const s of visible) {
    if (!groups.has(s.type)) groups.set(s.type, []);
    groups.get(s.type).push(s);
  }

  for (const [type, streams] of groups) {
    const group = document.createElement("div");
    group.className = "stream-group";

    const title = document.createElement("div");
    title.className = "stream-group-title";
    const label = document.createElement("span");
    label.textContent = (isSnapshotType(type) ? "📸 " : "") + type;
    const count = document.createElement("span");
    count.className = "stream-group-count";
    count.textContent = streams.length;
    title.append(label, count);
    group.appendChild(title);

    for (const s of streams) {
      const row = document.createElement("button");
      row.className = "stream-row" + (s.id === state.selected ? " selected" : "");
      row.title = s.id;

      const id = document.createElement("span");
      id.className = "stream-id";
      id.textContent = shortID(s.id);

      const version = document.createElement("span");
      version.className = "stream-version";
      version.textContent = "@" + s.version;

      row.append(id, version);
      row.addEventListener("click", () => selectStream(s.id));
      group.appendChild(row);
    }

    root.appendChild(group);
  }

  if (visible.length === 0) {
    const empty = document.createElement("p");
    empty.className = "hint";
    empty.textContent = state.streams.length === 0
      ? "No streams in this event store yet."
      : "No streams match the filter.";
    root.appendChild(empty);
  }
}

/* ============ stream view ============ */

async function selectStream(id) {
  state.selected = id;
  state.events = [];
  state.hasMore = false;
  state.nextAfter = 0;
  switchTab("streams");
  renderStreamList();

  $("#empty-state").hidden = true;
  $("#feed-view").hidden = true;
  $("#stream-view").hidden = false;
  $("#stream-title").textContent = id;

  const snapshot = isSnapshotType(streamType(id));
  $("#stream-snapshot-chip").hidden = !snapshot;
  $("#stream-snapshot-hint").hidden = !snapshot;

  await loadStreamPage(true);
}

async function loadStreamPage(reset) {
  const after = reset ? 0 : state.nextAfter;
  const url = `/api/streams/${encodeURIComponent(state.selected)}/events` +
    `?dir=${state.dir}&after=${after}&count=${STREAM_PAGE}`;

  const res = await getJSON(url, { allow404: true });
  if (!res) return;

  if (res.error === "stream_not_found") {
    state.events = [];
    renderStreamEvents();
    $("#stream-status").textContent = "stream not found";
    $("#stream-more").hidden = true;
    if (!state.info.capabilities.listStreams) {
      manualError(`No stream ${state.selected} in this event store.`);
    }
    return;
  }

  state.events = reset ? res.events : state.events.concat(res.events);
  state.hasMore = res.hasMore;
  state.nextAfter = res.nextAfter;
  renderStreamEvents();
}

function renderStreamEvents() {
  const tbody = $("#stream-events");
  tbody.innerHTML = "";

  for (const evt of state.events) {
    const row = document.createElement("tr");
    row.className = "event-row";

    row.appendChild(cell("version num", "v" + evt.version));
    row.appendChild(chipCell(evt.eventType));
    row.appendChild(cell("timestamp", formatTime(evt.timestamp)));
    row.appendChild(cell("gpos num", evt.globalPosition === null ? "—" : "#" + evt.globalPosition));

    wireExpandableRow(row, evt, 4);
    tbody.appendChild(row);
  }

  $("#stream-more").hidden = !state.hasMore;
  $("#stream-status").textContent = state.events.length === 0
    ? "no events"
    : `${state.events.length} event${state.events.length === 1 ? "" : "s"} loaded` +
      (state.hasMore ? ` — more available (next after=${state.nextAfter})` : " — end of stream");
}

function setDirection(dir) {
  if (state.dir === dir) return;
  state.dir = dir;
  $("#dir-reverse").classList.toggle("active", dir === "reverse");
  $("#dir-forward").classList.toggle("active", dir === "forward");
  if (state.selected) loadStreamPage(true);
}

/* ============ global feed ============ */

async function loadFeedInitial() {
  // Global reads are forward-only, so the newest events can't be fetched by
  // reading backwards from the end. /api/all/tail scans forward and keeps the
  // last page, then hands back the position to resume tailing from.
  const res = await getJSON(`/api/all/tail?count=${FEED_PAGE}`);
  if (!res) return;

  state.feed = res.events;
  state.feedLoaded = true;
  state.feedTotal = res.total;
  state.feedMaxPos = res.nextAfter;
  renderFeed();
  scrollFeedToBottom();
}

async function pollFeed() {
  // Forward from the last seen global position: AfterPosition is an exclusive
  // lower bound, and the stable-prefix contract guarantees nothing new can
  // commit at or below a position already yielded — so this returns only news,
  // with no gaps.
  const res = await getJSON(`/api/all?after=${state.feedMaxPos}&count=${FEED_PAGE}`);
  if (!res || res.events.length === 0) return;

  const nearBottom = feedNearBottom();
  state.feed = state.feed.concat(res.events);
  state.feedMaxPos = maxGlobalPos(res.events, state.feedMaxPos);
  renderFeed();
  if (nearBottom) scrollFeedToBottom();

  // refresh sidebar versions so stream rows keep up with the feed
  if (state.info.capabilities.listStreams) loadStreams();
}

function renderFeed() {
  const tbody = $("#feed-events");
  tbody.innerHTML = "";

  for (const evt of state.feed) {
    const row = document.createElement("tr");
    row.className = "event-row";

    row.appendChild(cell("gpos num", evt.globalPosition === null ? "—" : "#" + evt.globalPosition));

    const streamCell = document.createElement("td");
    const link = document.createElement("button");
    link.className = "chip stream-link" + (isSnapshotType(streamType(evt.streamId)) ? " snapshot" : "");
    link.textContent = (isSnapshotType(streamType(evt.streamId)) ? "📸 " : "") + shortID(evt.streamId);
    link.title = "Open stream " + evt.streamId;
    link.addEventListener("click", (e) => {
      e.stopPropagation();
      selectStream(evt.streamId);
    });
    streamCell.appendChild(link);
    row.appendChild(streamCell);

    row.appendChild(cell("version num", "v" + evt.version));
    row.appendChild(chipCell(evt.eventType));
    row.appendChild(cell("timestamp", formatTime(evt.timestamp)));

    wireExpandableRow(row, evt, 5);
    tbody.appendChild(row);
  }

  const shown = state.feed.length;
  const hidden = Math.max(state.feedTotal - shown, 0);
  $("#feed-status").textContent = shown === 0
    ? "no events in the store"
    : `${shown} event${shown === 1 ? "" : "s"}` +
      (hidden > 0 ? ` (newest of ${state.feedTotal})` : "") +
      (state.feedMaxPos > 0 ? ` — tail at #${state.feedMaxPos}` : "");
}

function setTail(enabled) {
  clearInterval(state.tailTimer);
  state.tailTimer = null;
  if (!enabled) return;

  if (!state.feedLoaded) loadFeedInitial();
  state.tailTimer = setInterval(pollFeed, TAIL_INTERVAL_MS);
}

function feedNearBottom() {
  const main = $("#main");
  return main.scrollHeight - main.scrollTop - main.clientHeight < 120;
}

function scrollFeedToBottom() {
  const main = $("#main");
  main.scrollTop = main.scrollHeight;
}

function maxGlobalPos(events, initial) {
  let max = initial;
  for (const evt of events) {
    if (evt.globalPosition !== null && evt.globalPosition > max) max = evt.globalPosition;
  }
  return max;
}

/* ============ expandable payload rows ============ */

function wireExpandableRow(row, evt, colspan) {
  row.addEventListener("click", () => {
    const existing = row.nextElementSibling;
    if (existing && existing.classList.contains("payload-row")) {
      existing.remove();
      row.classList.remove("open");
      return;
    }
    row.classList.add("open");
    row.after(payloadRow(evt, colspan));
  });
}

function payloadRow(evt, colspan) {
  const tr = document.createElement("tr");
  tr.className = "payload-row";
  const td = document.createElement("td");
  td.colSpan = colspan;

  td.appendChild(label("payload"));

  if (evt.dataEncoding === "base64") {
    const note = document.createElement("div");
    note.className = "binary-note";
    note.textContent = "⚠ payload is not valid JSON — shown base64-encoded";
    td.appendChild(note);
  }

  const pre = document.createElement("pre");
  pre.className = "payload";
  if (evt.dataEncoding === "empty") {
    pre.textContent = "(no payload)";
  } else {
    pre.innerHTML = tintJSON(evt.data);
  }
  td.appendChild(pre);

  if (evt.metadata && Object.keys(evt.metadata).length > 0) {
    td.appendChild(label("metadata"));
    const meta = document.createElement("pre");
    meta.className = "payload";
    meta.innerHTML = tintJSON(evt.metadata);
    td.appendChild(meta);
  }

  const idLabel = label("event id");
  td.appendChild(idLabel);
  const id = document.createElement("pre");
  id.className = "payload";
  id.textContent = evt.eventId;
  td.appendChild(id);

  tr.appendChild(td);
  return tr;
}

function label(text) {
  const el = document.createElement("div");
  el.className = "payload-label";
  el.textContent = text;
  return el;
}

// tintJSON pretty-prints a JSON value with CSS-class syntax tinting.
// No libraries: escape HTML first, then classify tokens with one regex.
function tintJSON(value) {
  const pretty = JSON.stringify(value, null, 2);
  const escaped = pretty
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");

  return escaped.replace(
    /("(\\u[a-fA-F0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(?:true|false|null)\b|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g,
    (match) => {
      let cls = "j-num";
      if (match.startsWith('"')) {
        cls = match.endsWith(":") ? "j-key" : "j-str";
      } else if (/^(true|false|null)$/.test(match)) {
        cls = "j-lit";
      }
      return `<span class="${cls}">${match}</span>`;
    },
  );
}

/* ============ tabs & chrome ============ */

function switchTab(tab) {
  state.activeTab = tab;
  $("#tab-streams").classList.toggle("active", tab === "streams");
  $("#tab-feed").classList.toggle("active", tab === "feed");

  if (tab === "feed") {
    $("#stream-view").hidden = true;
    $("#empty-state").hidden = true;
    $("#feed-view").hidden = false;
    if (!state.feedLoaded) loadFeedInitial();
  } else {
    $("#feed-view").hidden = true;
    $("#stream-view").hidden = state.selected === null;
    $("#empty-state").hidden = state.selected !== null;
  }
}

function wireChrome() {
  $("#tab-streams").addEventListener("click", () => switchTab("streams"));
  $("#tab-feed").addEventListener("click", () => switchTab("feed"));

  $("#stream-filter").addEventListener("input", (e) => {
    state.filter = e.target.value;
    renderStreamList();
  });

  $("#dir-reverse").addEventListener("click", () => setDirection("reverse"));
  $("#dir-forward").addEventListener("click", () => setDirection("forward"));
  $("#stream-more").addEventListener("click", () => loadStreamPage(false));

  $("#tail-checkbox").addEventListener("change", (e) => {
    if (e.target.checked && state.activeTab !== "feed") switchTab("feed");
    setTail(e.target.checked);
  });

  const openManual = () => {
    manualError("");
    const id = $("#manual-id").value.trim();
    if (!id) return;
    if (!/^[^_]+_[0-9a-fA-F-]{36}$/.test(id)) {
      manualError("Expected the form type_uuid, e.g. board_e5701a1a-b0a2-4d00-8000-000000000001");
      return;
    }
    selectStream(id);
  };

  $("#manual-open").addEventListener("click", openManual);
  $("#manual-id").addEventListener("keydown", (e) => {
    if (e.key === "Enter") openManual();
  });
}

function manualError(message) {
  const el = $("#manual-error");
  el.textContent = message;
  el.hidden = message === "";
}

/* ============ helpers ============ */

function cell(cls, text) {
  const td = document.createElement("td");
  td.className = cls;
  td.textContent = text;
  return td;
}

function chipCell(eventType) {
  const td = document.createElement("td");
  const chip = document.createElement("span");
  chip.className = "chip type";
  chip.textContent = eventType;
  td.appendChild(chip);
  return td;
}

function streamType(id) {
  const idx = id.indexOf("_");
  return idx > 0 ? id.slice(0, idx) : id;
}

function isSnapshotType(type) {
  return type.endsWith("snapshot");
}

function shortID(id) {
  const idx = id.indexOf("_");
  if (idx <= 0) return id;
  return id.slice(0, idx) + "_" + id.slice(idx + 1, idx + 9) + "…";
}

function formatTime(ts) {
  const d = new Date(ts);
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" }) +
    " " + d.toLocaleTimeString(undefined, { hour12: false });
}

// getJSON fetches a URL and returns the decoded body, or null after showing a
// toast for unexpected errors. 404s pass through when allowed so callers can
// render "stream not found" in place.
async function getJSON(url, { allow404 = false } = {}) {
  let res;
  try {
    res = await fetch(url);
  } catch {
    toast("Request failed", "error", "Is the inspector server still running?");
    return null;
  }

  const body = await res.json().catch(() => ({}));

  if (res.ok || (allow404 && res.status === 404)) return body;

  if (res.status === 501 && body.error === "capability_unavailable") {
    toast("Capability unavailable", "error", body.message);
    return null;
  }

  toast(body.error || `HTTP ${res.status}`, "error", body.message || "");
  return null;
}

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
    d.textContent = detail;
    el.appendChild(d);
  }

  $("#toasts").appendChild(el);
  setTimeout(() => el.remove(), 7000);
}

init();
