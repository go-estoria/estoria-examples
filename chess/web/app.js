/* Estoria Chess — vanilla JS client.
 *
 * The client is deliberately thin: all state lives in the event streams on
 * the server, one stream per game. The browser holds only the latest game
 * message (pushed over SSE), the legal moves for the live position, and an
 * optional "viewing" version when replaying history.
 */

"use strict";

const $ = (sel) => document.querySelector(sel);

const GLYPHS = { k: "♚", q: "♛", r: "♜", b: "♝", n: "♞", p: "♟" };
const FILES = "abcdefgh";

const state = {
  view: "lobby",     // "lobby" | "game"
  gameId: null,      // UUID of the game being viewed
  games: [],         // lobby summaries
  latest: null,      // {gameId, version, game, san} — latest known state
  viewing: null,     // number | null — version being viewed in replay
  viewingMsg: null,  // game message pinned at state.viewing
  legal: null,       // {version, turn, moves} for the live position
  selected: null,    // origin square selected for a move, e.g. "e2"
  pendingPromo: null,// {from, to} awaiting a promotion piece choice
};

/* ============ bootstrap & routing ============ */

function init() {
  wireChrome();
  connect();
  window.addEventListener("hashchange", route);
  route();
}

function route() {
  const match = location.hash.match(/^#\/g\/([0-9a-f-]{36})$/i);
  if (match) {
    enterGame(match[1]);
  } else {
    enterLobby();
  }
}

function setView(view) {
  state.view = view;
  $("#lobby-view").classList.toggle("hidden", view !== "lobby");
  $("#game-view").classList.toggle("hidden", view !== "game");
  $("#timebar").classList.toggle("hidden", view !== "game");
  $("#lobby-link").classList.toggle("hidden", view !== "game");
  $("#new-game-btn").classList.toggle("hidden", view !== "lobby");
  if (view === "lobby") {
    document.body.classList.remove("time-traveling");
    $("#banner").classList.add("hidden");
  }
}

async function enterLobby() {
  setView("lobby");
  state.gameId = null;
  state.latest = null;
  goLiveState();

  const res = await fetch("/api/games");
  if (!res.ok) {
    toast("Failed to load games", "error");
    return;
  }
  state.games = await res.json();
  renderLobby();
}

async function enterGame(id) {
  setView("game");
  state.gameId = id;
  state.latest = null;
  state.legal = null;
  goLiveState();

  const res = await fetch("/api/games/" + id);
  if (!res.ok) {
    toast(res.status === 404 ? "Game not found" : "Failed to load game", "error");
    location.hash = "#/";
    return;
  }
  state.latest = await res.json();
  renderGame();
  refreshLegalMoves();
}

/* ============ live updates (SSE) ============ */

function connect() {
  const es = new EventSource("/api/watch");

  es.onopen = () => setPill("● live", "live");

  // every message is a full game payload tagged with its gameId: the lobby
  // uses each one to refresh its list, the game view filters for its own game
  es.onmessage = (e) => {
    const msg = JSON.parse(e.data);

    // the hosted demo clears every game on the hour; whatever this tab was
    // looking at is gone, so start over from the lobby
    if (msg.reset) {
      location.reload();
      return;
    }

    if (state.view === "lobby") {
      upsertSummary(msg);
      renderLobby();
      return;
    }

    if (msg.gameId !== state.gameId) return;
    if (state.latest && msg.version <= state.latest.version) return; // stale

    const moved = state.latest !== null;
    state.latest = msg;

    if (state.viewing === null) {
      renderGame(moved);
    } else {
      // keep replaying: the slider max grows as the opponent plays on
      updateTimebar();
      renderMoveList();
      showBanner();
    }
    refreshLegalMoves();
  };

  es.onerror = () => setPill("reconnecting…", "warn"); // EventSource retries itself
}

function setPill(text, cls) {
  const pill = $("#conn-pill");
  pill.textContent = text;
  pill.className = "pill" + (cls ? " " + cls : "");
}

function upsertSummary(msg) {
  const summary = {
    gameId: msg.gameId,
    white: msg.game.white,
    black: msg.game.black,
    moveCount: (msg.game.movesUci || []).length,
    outcome: msg.game.outcome,
    method: msg.game.method,
    turn: msg.game.turn,
    check: msg.game.check,
    version: msg.version,
  };
  const idx = state.games.findIndex((g) => g.gameId === msg.gameId);
  if (idx >= 0) {
    state.games[idx] = summary;
  } else {
    state.games.unshift(summary);
  }
}

/* ============ commands ============ */

// Send a command based on the latest version we know about. A 409 means the
// stream advanced past that version — in chess: the other player moved first.
async function command(path, body) {
  body.baseVersion = state.latest.version;

  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (res.ok) return res.json();

  const err = await res.json().catch(() => ({}));

  if (res.status === 409) {
    toast("Position changed", "conflict",
      "The other player moved first: the stream advanced past " +
      `<code>v${err.expectedVersion}</code> to <code>v${err.actualVersion}</code>. ` +
      "The board has been refreshed — it is a new position now.");
    await refreshGame();
    throw err;
  }

  if (res.status === 422) {
    toast("Illegal move", "error", err.error || "");
    renderGame();
    throw err;
  }

  toast(err.error || err.message || "Request failed", "error");
  renderGame();
  throw err;
}

async function refreshGame() {
  const res = await fetch("/api/games/" + state.gameId);
  if (res.ok) {
    state.latest = await res.json();
    if (state.viewing === null) renderGame();
    updateTimebar();
    refreshLegalMoves();
  }
}

async function refreshLegalMoves() {
  if (!state.latest || state.latest.game.outcome !== "*") {
    state.legal = null;
    return;
  }
  const res = await fetch(`/api/games/${state.gameId}/legal-moves`);
  if (!res.ok) return;
  const legal = await res.json();
  // ignore a slow response for a position that has since changed
  if (state.latest && legal.version === state.latest.version) {
    state.legal = legal;
    if (state.viewing === null) renderBoard();
  }
}

/* ============ lobby rendering ============ */

function renderLobby() {
  const list = $("#game-list");
  list.innerHTML = "";

  $("#lobby-empty").classList.toggle("hidden", state.games.length > 0);

  for (const g of state.games) {
    const card = document.createElement("div");
    card.className = "game-card";

    const matchup = document.createElement("div");
    matchup.className = "matchup";
    const wDot = document.createElement("span");
    wDot.className = "side-dot w";
    const wName = document.createElement("span");
    wName.textContent = g.white;
    const vs = document.createElement("span");
    vs.className = "vs";
    vs.textContent = "vs";
    const bDot = document.createElement("span");
    bDot.className = "side-dot b";
    const bName = document.createElement("span");
    bName.textContent = g.black;
    matchup.append(wDot, wName, vs, bDot, bName);

    const meta = document.createElement("div");
    meta.className = "meta";
    const status = document.createElement("span");
    status.className = "game-status" + (g.outcome !== "*" ? " over" : g.check ? " check" : "");
    status.textContent = summaryStatus(g);
    const count = document.createElement("span");
    count.className = "move-count";
    count.textContent = g.moveCount + (g.moveCount === 1 ? " move" : " moves");
    meta.append(status, count);

    card.append(matchup, meta);
    card.addEventListener("click", () => { location.hash = "#/g/" + g.gameId; });
    list.appendChild(card);
  }
}

function summaryStatus(g) {
  if (g.outcome === "*") {
    return cap(g.turn) + " to move" + (g.check ? " — check!" : "");
  }
  return outcomeText(g);
}

function outcomeText(g) {
  switch (g.outcome) {
    case "1-0":
      return g.method === "Resignation" ? "Black resigned — White wins" : "Checkmate — White wins";
    case "0-1":
      return g.method === "Resignation" ? "White resigned — Black wins" : "Checkmate — Black wins";
    case "1/2-1/2":
      return "Draw — " + (g.method || "agreed").toLowerCase();
    default:
      return g.outcome;
  }
}

const cap = (s) => s.charAt(0).toUpperCase() + s.slice(1);

/* ============ game rendering ============ */

// messageToRender returns the game payload for the version being displayed:
// the live message, or the pinned historical one while replaying.
function messageToRender() {
  return state.viewing === null ? state.latest : state.viewingMsg;
}

function renderGame(animate = false) {
  const msg = messageToRender();
  if (!msg) return;

  document.body.classList.toggle("time-traveling", state.viewing !== null);

  $("#white-name").textContent = state.latest.game.white;
  $("#black-name").textContent = state.latest.game.black;
  $("#pgn-btn").href = `/api/games/${state.gameId}/pgn`;

  renderStatus(msg.game);
  renderBoard(animate);
  renderMoveList();
  renderActions();
  updateTimebar();
}

function renderStatus(game) {
  const el = $("#status-banner");
  el.classList.toggle("over", game.outcome !== "*");
  el.innerHTML = "";

  if (game.outcome !== "*") {
    el.textContent = outcomeText(game);
    return;
  }

  el.append(document.createTextNode(cap(game.turn) + " to move"));
  if (game.check) {
    const check = document.createElement("span");
    check.className = "check-flag";
    check.textContent = " — Check!";
    el.appendChild(check);
  }
}

function renderBoard(animate = false) {
  const msg = messageToRender();
  if (!msg) return;

  const pieces = parseFEN(msg.game.fen);
  const moves = msg.game.movesUci || [];
  const lastMove = moves.length > 0 ? moves[moves.length - 1] : null;
  const interactive = state.viewing === null && msg.game.outcome === "*";
  const legal = interactive && state.legal && state.legal.version === msg.version
    ? state.legal.moves : {};
  const targets = state.selected && legal[state.selected] ? legal[state.selected] : [];

  // find the checked king so its square can be flagged
  let checkSquare = null;
  if (msg.game.check) {
    const kingColor = msg.game.turn === "white" ? "w" : "b";
    for (const [sq, p] of Object.entries(pieces)) {
      if (p.type === "k" && p.color === kingColor) checkSquare = sq;
    }
  }

  const root = $("#board");
  root.innerHTML = "";

  // fixed orientation: white at the bottom (rank 8 rendered first)
  for (let rank = 8; rank >= 1; rank--) {
    for (let file = 0; file < 8; file++) {
      const sq = FILES[file] + rank;
      const el = document.createElement("div");
      el.className = "square " + ((file + rank) % 2 === 0 ? "light" : "dark");
      el.dataset.square = sq;

      const piece = pieces[sq];
      if (piece) {
        const span = document.createElement("span");
        span.className = "piece " + piece.color;
        span.textContent = GLYPHS[piece.type];
        el.appendChild(span);
      }

      if (rank === 1) {
        const coord = document.createElement("span");
        coord.className = "coord file";
        coord.textContent = FILES[file];
        el.appendChild(coord);
      }
      if (file === 0) {
        const coord = document.createElement("span");
        coord.className = "coord rank";
        coord.textContent = rank;
        el.appendChild(coord);
      }

      if (lastMove && (sq === lastMove.slice(0, 2) || sq === lastMove.slice(2, 4))) {
        el.classList.add("last-move");
        if (animate && sq === lastMove.slice(2, 4)) el.classList.add("landed");
      }
      if (sq === checkSquare) el.classList.add("check-square");
      if (sq === state.selected) el.classList.add("selected");

      if (interactive && legal[sq] && legal[sq].length > 0) {
        el.classList.add("selectable");
      }

      const target = targets.find((t) => t.to === sq);
      if (target) {
        el.classList.add("target");
        if (piece) el.classList.add("capture");
        el.dataset.promotion = target.promotion ? "1" : "";
      }

      root.appendChild(el);
    }
  }
}

// parseFEN maps the piece-placement field of a FEN string to {square: piece}.
function parseFEN(fen) {
  const pieces = {};
  const ranks = fen.split(" ")[0].split("/");
  for (let i = 0; i < 8; i++) {
    const rank = 8 - i;
    let file = 0;
    for (const ch of ranks[i]) {
      if (ch >= "1" && ch <= "8") {
        file += +ch;
      } else {
        pieces[FILES[file] + rank] = {
          type: ch.toLowerCase(),
          color: ch === ch.toUpperCase() ? "w" : "b",
        };
        file++;
      }
    }
  }
  return pieces;
}

/* ============ click-to-move ============ */

function wireBoard() {
  $("#board").addEventListener("click", async (e) => {
    const el = e.target.closest(".square");
    if (!el || state.viewing !== null) return;

    const sq = el.dataset.square;

    if (el.classList.contains("target")) {
      const from = state.selected;
      state.selected = null;
      if (el.dataset.promotion) {
        openPromoPicker(from, sq);
      } else {
        await sendMove(from + sq);
      }
      return;
    }

    if (el.classList.contains("selectable") && sq !== state.selected) {
      state.selected = sq;
    } else {
      state.selected = null;
    }
    renderBoard();
  });
}

async function sendMove(uci) {
  renderBoard(); // clear selection highlights immediately
  try {
    await command(`/api/games/${state.gameId}/move`, { uci });
  } catch { /* command() already toasted */ }
}

function openPromoPicker(from, to) {
  state.pendingPromo = { from, to };
  const black = state.latest.game.turn === "black";
  const wrap = $("#promo-choices");
  wrap.innerHTML = "";
  for (const p of ["q", "r", "b", "n"]) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = black ? "black" : "";
    btn.dataset.promo = p;
    btn.textContent = GLYPHS[p];
    wrap.appendChild(btn);
  }
  $("#promo-modal").showModal();
  renderBoard();
}

function wirePromoPicker() {
  const modal = $("#promo-modal");

  $("#promo-choices").addEventListener("click", async (e) => {
    const btn = e.target.closest("button[data-promo]");
    if (!btn || !state.pendingPromo) return;
    const { from, to } = state.pendingPromo;
    state.pendingPromo = null;
    modal.close();
    await sendMove(from + to + btn.dataset.promo);
  });

  modal.addEventListener("close", () => { state.pendingPromo = null; });
}

/* ============ move list ============ */

// Version numbering: v1 is the freshly created game, and each ply k lands at
// version k+1. The SAN list is paired into full moves ("1. e4 e5").
function renderMoveList() {
  const list = $("#move-list");
  list.innerHTML = "";

  const san = state.latest.san || [];
  const viewingVersion = state.viewing === null ? state.latest.version : state.viewing;

  if (san.length === 0) {
    const li = document.createElement("li");
    li.className = "moves-empty";
    li.textContent = "No moves yet — white to open.";
    list.appendChild(li);
    return;
  }

  for (let i = 0; i < san.length; i += 2) {
    const num = document.createElement("li");
    num.className = "move-num";
    num.textContent = i / 2 + 1 + ".";
    list.appendChild(num);

    for (const ply of [i, i + 1]) {
      const cell = document.createElement("li");
      if (ply < san.length) {
        const version = ply + 2; // ply k (0-based) lands at version k+2
        cell.className = "ply" + (version === viewingVersion ? " current" : "");
        cell.textContent = san[ply];
        cell.title = `Jump to move ${ply + 1}`;
        cell.addEventListener("click", () => travelTo(version));
      } else {
        cell.className = "ply empty";
      }
      list.appendChild(cell);
    }
  }

  const current = list.querySelector(".ply.current");
  if (current) current.scrollIntoView({ block: "nearest" });
}

function renderActions() {
  const over = state.latest.game.outcome !== "*";
  const replaying = state.viewing !== null;
  $("#resign-white").disabled = over || replaying;
  $("#resign-black").disabled = over || replaying;
}

/* ============ replay (time travel) ============ */

function updateTimebar() {
  if (!state.latest) return;
  const slider = $("#time-slider");
  slider.max = state.latest.version;
  if (state.viewing === null) slider.value = state.latest.version;
  updateTimeLabel();
}

function updateTimeLabel() {
  const v = state.viewing === null ? state.latest.version : state.viewing;
  const total = state.latest.version - 1; // moves played so far
  const move = v - 1;                     // moves shown at version v
  $("#time-label").textContent =
    state.viewing === null
      ? `live · move ${total} of ${total}`
      : `move ${move} of ${total}`;
}

const debouncedTravel = debounce(async (v) => {
  const res = await fetch(`/api/games/${state.gameId}?version=${v}`);
  if (!res.ok) return;
  const msg = await res.json();
  if (state.viewing !== v) return; // the slider has moved on
  state.viewingMsg = msg;
  renderGame();
  showBanner();
}, 120);

function wireTimebar() {
  const slider = $("#time-slider");

  slider.addEventListener("input", () => {
    const v = +slider.value;
    if (v >= state.latest.version) return goLive();
    state.viewing = v;
    state.selected = null;
    updateTimeLabel();
    debouncedTravel(v);
  });

  $("#live-btn").addEventListener("click", goLive);
}

function goLiveState() {
  state.viewing = null;
  state.viewingMsg = null;
  state.selected = null;
  $("#banner").classList.add("hidden");
}

function goLive() {
  goLiveState();
  renderGame();
}

function travelTo(v) {
  if (v >= state.latest.version) return goLive();
  state.viewing = v;
  state.selected = null;
  $("#time-slider").value = v;
  updateTimeLabel();
  debouncedTravel(v);
}

function showBanner() {
  if (state.viewing === null) return;
  const banner = $("#banner");
  const move = state.viewing - 1;
  const total = state.latest.version - 1;
  const san = move > 0 ? state.latest.san[move - 1] : null;

  banner.innerHTML = "";
  const text = document.createElement("span");
  text.textContent = move === 0
    ? `Viewing the starting position (move 0 of ${total})`
    : `Viewing move ${move} of ${total} — ${san}`;
  const btn = document.createElement("button");
  btn.textContent = "Return to live";
  btn.addEventListener("click", goLive);
  banner.append(text, btn);
  banner.classList.remove("hidden");
}

/* ============ chrome ============ */

function wireChrome() {
  wireBoard();
  wirePromoPicker();
  wireTimebar();

  const modal = $("#new-game-modal");

  $("#new-game-btn").addEventListener("click", () => {
    $("#white-input").value = "";
    $("#black-input").value = "";
    modal.showModal();
  });

  $("#new-game-cancel").addEventListener("click", () => modal.close());

  $("#new-game-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    modal.close();
    const res = await fetch("/api/games", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        white: $("#white-input").value.trim(),
        black: $("#black-input").value.trim(),
      }),
    });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      toast(err.error || "Failed to create game", "error");
      return;
    }
    const msg = await res.json();
    location.hash = "#/g/" + msg.gameId;
  });

  for (const color of ["white", "black"]) {
    $(`#resign-${color}`).addEventListener("click", async () => {
      if (!confirm(`Resign as ${color}? This ends the game.`)) return;
      try {
        await command(`/api/games/${state.gameId}/resign`, { color });
      } catch { /* handled */ }
    });
  }
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

function debounce(fn, ms) {
  let timer;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), ms);
  };
}

init();
