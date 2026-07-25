/* Estoria Kanban — vanilla JS client.
 *
 * The client is deliberately thin: all state lives in the event stream on the
 * server. The browser holds only the latest board (pushed over SSE) and an
 * optional "viewing" version when time-traveling.
 */

"use strict";

const $ = (sel) => document.querySelector(sel);

const COLORS = ["blue", "purple", "teal", "green", "amber", "pink", "red"];
const SWATCH_HEX = {
  blue: "#60a5fa", purple: "#a78bfa", teal: "#2dd4bf", green: "#34d399",
  amber: "#fbbf24", pink: "#f472b6", red: "#f87171",
};

const state = {
  live: null,        // {version, board} — latest known state
  viewing: null,     // number | null — version being viewed in time travel
  activity: [],      // ascending [{version, type, timestamp, description}]
  stats: null,
  dragging: null,    // card ID being dragged (suppresses re-render)
  pendingRender: false,
  editingCard: null, // card ID open in the modal
};

/* ============ bootstrap ============ */

async function init() {
  buildSwatches();
  wireChrome();

  const res = await fetch("/api/board");
  if (!res.ok) {
    toast("Failed to load board", "error");
    return;
  }
  state.live = await res.json();
  renderBoard();
  updateTimebar();
  refreshMeta();
  connect();
}

/* ============ live updates (SSE) ============ */

function connect() {
  const es = new EventSource("/api/watch");

  es.onopen = () => setPill("● live", "live");

  es.onmessage = (e) => {
    const msg = JSON.parse(e.data);
    if (state.live && msg.version < state.live.version) return; // stale
    state.live = msg;

    if (state.viewing === null) {
      renderBoard();
    } else {
      showBanner(); // refresh "live is now at vN" hint
    }
    updateTimebar();
    refreshMeta();
  };

  es.onerror = () => setPill("reconnecting…", "warn"); // EventSource retries itself
}

function setPill(text, cls) {
  const pill = $("#conn-pill");
  pill.textContent = text;
  pill.className = "pill" + (cls ? " " + cls : "");
}

/* ============ commands ============ */

// Send a command. baseVersion defaults to the latest version we know about;
// on a 409 conflict the command is retried once against the winning version.
async function command(path, body, { retry = true } = {}) {
  if (body.baseVersion === undefined) body.baseVersion = state.live.version;

  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (res.ok) return res.json();

  const err = await res.json().catch(() => ({}));

  if (res.status === 409) {
    await refreshLive(); // resync to the version that won
    if (retry) {
      delete body.baseVersion;
      return command(path, body, { retry: false });
    }
    conflictToast(err);
    throw err;
  }

  toast(err.error || err.message || "Request failed", "error");
  renderBoard(); // roll back any optimistic DOM changes
  throw err;
}

async function refreshLive() {
  const res = await fetch("/api/board");
  if (res.ok) {
    state.live = await res.json();
    if (state.viewing === null) renderBoard();
    updateTimebar();
  }
}

/* ============ board rendering ============ */

function boardToRender() {
  return state.viewing === null ? state.live.board : state.viewingBoard;
}

function renderBoard() {
  if (state.dragging) {
    state.pendingRender = true; // don't yank the DOM mid-drag
    return;
  }

  const board = boardToRender();
  if (!board) return;

  $("#board-name").textContent = board.name || "(unnamed board)";
  document.body.classList.toggle("time-traveling", state.viewing !== null);

  const root = $("#board");
  root.innerHTML = "";

  for (const col of board.columns || []) {
    root.appendChild(renderColumn(col));
  }

  if (state.viewing === null) {
    root.appendChild(renderAddColumn());
  }

  highlightActivity();
}

function renderColumn(col) {
  const el = document.createElement("div");
  el.className = "column";
  el.dataset.id = col.id;

  const header = document.createElement("div");
  header.className = "column-header";

  const title = document.createElement("span");
  title.className = "column-title";
  title.textContent = col.title;
  title.title = "Double-click to rename";
  title.addEventListener("dblclick", () => {
    if (state.viewing !== null) return;
    inlineRename(title, col.title, (name) =>
      command(`/api/columns/${col.id}/rename`, { title: name }));
  });

  const count = document.createElement("span");
  count.className = "column-count";
  count.textContent = (col.cards || []).length;

  header.append(title, count);
  el.appendChild(header);

  const cards = document.createElement("div");
  cards.className = "cards";
  cards.dataset.id = col.id;

  for (const card of col.cards || []) {
    cards.appendChild(renderCard(card));
  }

  wireDropZone(cards);
  el.appendChild(cards);

  if (state.viewing === null) {
    const footer = document.createElement("div");
    footer.className = "column-footer";
    footer.appendChild(renderComposer(col.id));
    el.appendChild(footer);
  }

  return el;
}

function renderCard(card) {
  const el = document.createElement("div");
  el.className = "card" + (card.color ? " c-" + card.color : "");
  el.dataset.id = card.id;
  el.draggable = state.viewing === null;

  const title = document.createElement("div");
  title.className = "card-title";
  title.textContent = card.title;
  el.appendChild(title);

  if (card.description) {
    const desc = document.createElement("div");
    desc.className = "card-desc";
    desc.textContent = card.description;
    el.appendChild(desc);
  }

  el.addEventListener("click", () => {
    if (state.viewing === null && !state.dragging) openCardModal(card);
  });

  el.addEventListener("dragstart", (e) => {
    if (state.viewing !== null) return e.preventDefault();
    state.dragging = card.id;
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData("text/plain", card.id);
    requestAnimationFrame(() => el.classList.add("dragging"));
  });

  el.addEventListener("dragend", () => {
    el.classList.remove("dragging");
    state.dragging = null;
    if (state.pendingRender) {
      state.pendingRender = false;
      renderBoard();
    }
  });

  return el;
}

function wireDropZone(cards) {
  cards.addEventListener("dragover", (e) => {
    if (!state.dragging) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";

    const dragged = document.querySelector(".card.dragging");
    if (!dragged) return;

    // live preview: physically move the dragged node to the hover position
    const after = dragAfterElement(cards, e.clientY);
    if (after === null) {
      cards.appendChild(dragged);
    } else if (after !== dragged) {
      cards.insertBefore(dragged, after);
    }
  });

  cards.addEventListener("drop", async (e) => {
    e.preventDefault();
    const cardID = e.dataTransfer.getData("text/plain");
    const dragged = document.querySelector(`.card[data-id="${CSS.escape(cardID)}"]`);
    if (!dragged) return;

    const toColumnId = cards.dataset.id;
    const toIndex = [...cards.children].indexOf(dragged);

    try {
      await command(`/api/cards/${cardID}/move`, { toColumnId, toIndex });
    } catch {
      /* command() already re-rendered / toasted */
    }
  });
}

function dragAfterElement(container, y) {
  const others = [...container.querySelectorAll(".card:not(.dragging)")];
  let closest = { offset: Number.NEGATIVE_INFINITY, el: null };
  for (const el of others) {
    const box = el.getBoundingClientRect();
    const offset = y - box.top - box.height / 2;
    if (offset < 0 && offset > closest.offset) closest = { offset, el };
  }
  return closest.el;
}

/* ============ composers & inline edits ============ */

function renderComposer(columnId) {
  const btn = document.createElement("button");
  btn.className = "add-card-btn";
  btn.textContent = "+ Add card";

  btn.addEventListener("click", () => {
    const composer = document.createElement("div");
    composer.className = "composer";

    const input = document.createElement("textarea");
    input.rows = 2;
    input.placeholder = "Card title";

    const actions = document.createElement("div");
    actions.className = "composer-actions";

    const add = document.createElement("button");
    add.className = "btn primary";
    add.textContent = "Add";

    const cancel = document.createElement("button");
    cancel.className = "btn ghost";
    cancel.textContent = "Cancel";

    const submit = async () => {
      const title = input.value.trim();
      if (!title) return cancelIt();
      add.disabled = true;
      try {
        await command("/api/cards", { columnId, title });
      } catch { /* handled */ }
    };
    const cancelIt = () => composer.replaceWith(btn);

    add.addEventListener("click", submit);
    cancel.addEventListener("click", cancelIt);
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); submit(); }
      if (e.key === "Escape") cancelIt();
    });

    actions.append(add, cancel);
    composer.append(input, actions);
    btn.replaceWith(composer);
    input.focus();
  });

  return btn;
}

function renderAddColumn() {
  const btn = document.createElement("button");
  btn.className = "add-column";
  btn.textContent = "+ Add column";

  btn.addEventListener("click", () => {
    const input = document.createElement("input");
    input.placeholder = "Column title";
    btn.textContent = "";
    btn.appendChild(input);
    input.focus();

    const done = () => renderBoard();
    input.addEventListener("keydown", async (e) => {
      if (e.key === "Enter") {
        const title = input.value.trim();
        if (!title) return done();
        try { await command("/api/columns", { title }); } catch { /* handled */ }
      }
      if (e.key === "Escape") done();
    });
    input.addEventListener("blur", done);
  });

  return btn;
}

function inlineRename(el, current, submit) {
  const input = document.createElement("input");
  input.className = "rename";
  input.value = current;
  el.replaceWith(input);
  input.focus();
  input.select();

  let finished = false;
  const finish = async (save) => {
    if (finished) return;
    finished = true;
    const value = input.value.trim();
    if (save && value && value !== current) {
      try { await submit(value); } catch { /* handled */ }
    } else {
      renderBoard();
    }
  };

  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") finish(true);
    if (e.key === "Escape") finish(false);
  });
  input.addEventListener("blur", () => finish(true));
}

/* ============ card modal ============ */

function buildSwatches() {
  const wrap = $("#swatches");
  const none = document.createElement("button");
  none.type = "button";
  none.className = "swatch";
  none.dataset.color = "";
  none.title = "No color";
  wrap.appendChild(none);

  for (const c of COLORS) {
    const sw = document.createElement("button");
    sw.type = "button";
    sw.className = "swatch";
    sw.dataset.color = c;
    sw.style.setProperty("--sw", SWATCH_HEX[c]);
    sw.title = c;
    wrap.appendChild(sw);
  }

  wrap.addEventListener("click", (e) => {
    const sw = e.target.closest(".swatch");
    if (!sw) return;
    wrap.querySelectorAll(".swatch").forEach((s) => s.classList.remove("selected"));
    sw.classList.add("selected");
  });
}

function openCardModal(card) {
  state.editingCard = card.id;
  $("#card-title").value = card.title;
  $("#card-desc").value = card.description || "";
  $("#swatches").querySelectorAll(".swatch").forEach((s) => {
    s.classList.toggle("selected", (card.color || "") === s.dataset.color);
  });
  $("#card-modal").showModal();
}

function wireModal() {
  const modal = $("#card-modal");

  $("#card-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const selected = $("#swatches .swatch.selected");
    modal.close();
    try {
      await command(`/api/cards/${state.editingCard}/edit`, {
        title: $("#card-title").value,
        description: $("#card-desc").value,
        color: selected ? selected.dataset.color : "",
      });
    } catch { /* handled */ }
  });

  $("#card-delete").addEventListener("click", async () => {
    modal.close();
    try { await command(`/api/cards/${state.editingCard}/delete`, {}); } catch { /* handled */ }
  });

  $("#card-cancel").addEventListener("click", () => modal.close());
}

/* ============ time travel ============ */

function updateTimebar() {
  const slider = $("#time-slider");
  slider.max = state.live.version;
  if (state.viewing === null) slider.value = state.live.version;
  updateTimeLabel();
}

function updateTimeLabel() {
  const v = state.viewing === null ? state.live.version : state.viewing;
  const entry = state.activity.find((a) => a.version === v);
  const when = entry ? " · " + relativeTime(entry.timestamp) : "";
  $("#time-label").textContent = `v${v} / v${state.live.version}${when}`;
}

const debouncedTravel = debounce(async (v) => {
  const res = await fetch("/api/board?version=" + v);
  if (!res.ok) return;
  const msg = await res.json();
  state.viewingBoard = msg.board;
  renderBoard();
  showBanner();
  highlightActivity();
}, 120);

function wireTimebar() {
  const slider = $("#time-slider");

  slider.addEventListener("input", () => {
    const v = +slider.value;
    if (v >= state.live.version) return goLive();
    state.viewing = v;
    updateTimeLabel();
    debouncedTravel(v);
  });

  $("#live-btn").addEventListener("click", goLive);
}

function goLive() {
  state.viewing = null;
  state.viewingBoard = null;
  $("#banner").classList.add("hidden");
  renderBoard();
  updateTimebar();
}

function travelTo(v) {
  if (v >= state.live.version) return goLive();
  state.viewing = v;
  $("#time-slider").value = v;
  updateTimeLabel();
  debouncedTravel(v);
}

function showBanner() {
  if (state.viewing === null) return;
  const banner = $("#banner");
  const entry = state.activity.find((a) => a.version === state.viewing);
  const desc = entry ? ` — ${entry.description}` : "";

  banner.innerHTML = "";
  const text = document.createElement("span");
  text.textContent = `Viewing v${state.viewing} of v${state.live.version}${desc}`;
  const btn = document.createElement("button");
  btn.textContent = "Return to live";
  btn.addEventListener("click", goLive);
  banner.append(text, btn);
  banner.classList.remove("hidden");
}

/* ============ activity & stats ============ */

const refreshMeta = debounce(async () => {
  const [activityRes, statsRes] = await Promise.all([
    fetch("/api/activity"),
    fetch("/api/stats"),
  ]);
  if (activityRes.ok) {
    state.activity = await activityRes.json();
    renderActivity();
    updateTimeLabel();
  }
  if (statsRes.ok) {
    const prev = state.stats;
    state.stats = await statsRes.json();
    renderStats();
    if (prev && state.stats.snapshotCount > prev.snapshotCount) {
      toast(
        `📸 Snapshot written at v${state.stats.lastSnapshotVersion}`,
        "snapshot",
        "Loads now start from this snapshot and replay only newer events.",
      );
    }
  }
}, 250);

function renderActivity() {
  const list = $("#activity");
  list.innerHTML = "";

  for (const entry of [...state.activity].reverse()) {
    const li = document.createElement("li");
    li.dataset.version = entry.version;

    const v = document.createElement("span");
    v.className = "v";
    v.textContent = "v" + entry.version;

    const desc = document.createElement("span");
    desc.textContent = entry.description;

    const when = document.createElement("span");
    when.className = "when";
    when.textContent = relativeTime(entry.timestamp);

    li.append(v, desc, when);
    li.addEventListener("click", () => travelTo(entry.version));
    list.appendChild(li);
  }

  highlightActivity();
}

function highlightActivity() {
  const v = state.viewing === null ? -1 : state.viewing;
  $("#activity").querySelectorAll("li").forEach((li) => {
    li.classList.toggle("current", +li.dataset.version === v);
  });
}

function renderStats() {
  const s = state.stats;

  const stack = $("#stack");
  stack.innerHTML = "";
  for (const layer of s.storeStack) {
    const li = document.createElement("li");
    li.textContent = layer;
    stack.appendChild(li);
  }

  const streams = $("#streams");
  streams.innerHTML = "";
  for (const stream of s.streams) {
    const row = document.createElement("div");
    row.className = "stream-row";
    const id = document.createElement("span");
    id.className = "stream-id";
    id.textContent = stream.id;
    id.title = stream.id;
    const version = document.createElement("span");
    version.className = "stream-version";
    version.textContent = "@" + stream.version;
    row.append(id, version);
    streams.appendChild(row);
  }

  const info = $("#snapshot-info");
  if (s.snapshotCount > 0) {
    info.innerHTML =
      `Latest snapshot: <strong>v${s.lastSnapshotVersion}</strong> ` +
      `(${s.snapshotCount} total, every ${s.snapshotEvery} events).<br>` +
      `Loading the board replays only events after <code>v${s.lastSnapshotVersion}</code>.`;
  } else {
    const remaining = s.snapshotEvery - (s.boardVersion % s.snapshotEvery);
    info.innerHTML =
      `No snapshots yet — the next one lands in <strong>${remaining}</strong> ` +
      `event${remaining === 1 ? "" : "s"}.`;
  }
}

/* ============ chrome ============ */

function wireChrome() {
  wireModal();
  wireTimebar();

  $("#panel-toggle").addEventListener("click", () => {
    $("#panel").classList.toggle("hidden");
  });

  $("#board-name").addEventListener("click", () => {
    if (state.viewing !== null) return;
    inlineRename($("#board-name"), state.live.board.name, (name) =>
      command("/api/board/rename", { name }));
  });

  // deliberately send a command based on a stale version to demonstrate
  // optimistic concurrency: the server will answer 409
  $("#conflict-btn").addEventListener("click", async () => {
    if (state.live.version < 2) {
      toast("Make at least one change first", "error");
      return;
    }
    try {
      await command("/api/board/rename", {
        name: state.live.board.name,
        baseVersion: state.live.version - 1,
      }, { retry: false });
      toast("Unexpectedly succeeded — try again", "error");
    } catch { /* the 409 toast is the point */ }
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

function conflictToast(err) {
  toast("⚡ Version conflict (HTTP 409)", "conflict",
    `The write expected the stream at <code>v${err.expectedVersion}</code>, but it is at ` +
    `<code>v${err.actualVersion}</code>. Estoria saves with <code>ExpectVersion</code>, so the ` +
    `event store rejected the append with a <code>StreamVersionMismatchError</code>. ` +
    `The board has been refreshed.`);
}

function relativeTime(ts) {
  const seconds = Math.round((Date.now() - new Date(ts).getTime()) / 1000);
  if (seconds < 5) return "just now";
  if (seconds < 60) return seconds + "s ago";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return minutes + "m ago";
  const hours = Math.round(minutes / 60);
  if (hours < 24) return hours + "h ago";
  return Math.round(hours / 24) + "d ago";
}

function debounce(fn, ms) {
  let timer;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), ms);
  };
}

init();
