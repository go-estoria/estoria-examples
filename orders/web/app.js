/* Estoria Orders — vanilla JS client.
 *
 * The client is deliberately thin: all state lives on the server. The order
 * LIST comes from the order_summaries read model (updated asynchronously by
 * the outbox processor); the detail drawer loads the aggregate itself. SSE
 * pushes both halves: "order" when a command is saved, "delivery" when the
 * outbox lands an event in the read model.
 */

"use strict";

const $ = (sel) => document.querySelector(sel);

const STATUSES = ["placed", "paid", "picked", "shipped", "delivered", "cancelled"];
const STEPS = ["placed", "paid", "picked", "shipped", "delivered"];

const ACTIONS = {
  placed: [
    { label: "Pay", action: "pay", cls: "primary" },
    { label: "Cancel", action: "cancel", cls: "danger" },
  ],
  paid: [
    { label: "Pick", action: "pick", cls: "primary" },
    { label: "Cancel", action: "cancel", cls: "danger" },
  ],
  picked: [
    { label: "Ship", action: "ship", cls: "primary" },
    { label: "Cancel", action: "cancel", cls: "danger" },
  ],
  shipped: [{ label: "Deliver", action: "deliver", cls: "primary" }],
  delivered: [],
  cancelled: [],
};

const TERMINAL_NOTES = {
  delivered: "Delivered — this order's stream is complete.",
  cancelled: "Cancelled — no further transitions are allowed.",
};

const state = {
  orders: [],     // read-model summaries, newest first
  counts: {},     // status -> count, from the read model
  deliveries: [], // recent webhook deliveries, newest first
  pending: 0,     // undelivered outbox rows
  detail: null,   // {version, order, timeline} for the open drawer, or null
};

/* ============ bootstrap ============ */

async function init() {
  wireChrome();
  await Promise.all([refreshOrders(), refreshOutbox()]);
  connect();

  // keep relative timestamps honest
  setInterval(() => {
    renderOrders();
    renderDeliveries();
  }, 30_000);
}

/* ============ live updates (SSE) ============ */

function connect() {
  const es = new EventSource("/api/watch");

  es.onopen = () => {
    setPill("● live", "live");
    refreshOrders(); // resync after any missed updates
    refreshOutbox();
  };

  es.onmessage = (e) => {
    const msg = JSON.parse(e.data);

    if (msg.type === "order") {
      // a command was saved; the aggregate advanced. The LIST doesn't move
      // yet — it waits for the outbox to update the read model.
      if (state.detail && state.detail.order.id === msg.order.id) {
        refreshDetail(msg.order.id);
      }
    } else if (msg.type === "delivery") {
      // the outbox processor delivered an event: the read model advanced
      prependDelivery(msg.delivery);
      debouncedRefreshOrders();
      debouncedRefreshOutbox();
    }
  };

  es.onerror = () => setPill("reconnecting…", "warn"); // EventSource retries itself
}

function setPill(text, cls) {
  const pill = $("#conn-pill");
  pill.textContent = text;
  pill.className = "pill" + (cls ? " " + cls : "");
}

/* ============ commands ============ */

// Send a command based on the version the drawer last saw. On a 409 conflict
// the drawer is refreshed and the command retried once against the winning
// version; a second conflict (or a now-invalid transition) surfaces as a toast.
async function command(path, body, { retry = true } = {}) {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (res.ok) return res.json();

  const err = await res.json().catch(() => ({}));

  if (res.status === 409) {
    const fresh = state.detail ? await refreshDetail(state.detail.order.id) : null;
    if (retry && fresh) {
      body.baseVersion = fresh.version;
      return command(path, body, { retry: false });
    }
    conflictToast(err);
    throw err;
  }

  toast(err.error || err.message || "Request failed", "error");
  throw err;
}

async function newOrder() {
  const btn = $("#new-order");
  btn.disabled = true;
  try {
    const res = await fetch("/api/orders", { method: "POST" });
    if (!res.ok) {
      toast("Failed to place order", "error");
      return;
    }
    const { id } = await res.json();
    await openDetail(id);
  } finally {
    btn.disabled = false;
  }
}

async function act(action) {
  if (!state.detail) return;
  const id = state.detail.order.id;
  try {
    await command(`/api/orders/${id}/${action}`, { baseVersion: state.detail.version });
    await refreshDetail(id); // SSE will also refresh, but be eager
  } catch {
    /* command() already toasted */
  }
}

/* ============ data fetching ============ */

async function refreshOrders() {
  const res = await fetch("/api/orders");
  if (!res.ok) return;
  const data = await res.json();
  state.orders = data.orders || [];
  state.counts = data.counts || {};
  renderOrders();
  renderBadges();
}

async function refreshOutbox() {
  const res = await fetch("/api/outbox");
  if (!res.ok) return;
  const data = await res.json();
  state.pending = data.pending || 0;
  state.deliveries = data.deliveries || [];
  renderPending();
  renderDeliveries();
}

const debouncedRefreshOrders = debounce(refreshOrders, 150);
const debouncedRefreshOutbox = debounce(refreshOutbox, 150);

async function refreshDetail(id) {
  const res = await fetch(`/api/orders/${id}`);
  if (!res.ok) return null;
  const data = await res.json();
  if (state.detail && state.detail.order.id === id) {
    state.detail = data;
    renderDetail();
  }
  return data;
}

/* ============ orders table ============ */

function renderBadges() {
  const wrap = $("#badges");
  wrap.innerHTML = "";

  for (const status of STATUSES) {
    const n = state.counts[status] || 0;
    const badge = document.createElement("span");
    badge.className = `badge s-${status}` + (n === 0 ? " zero" : "");

    const dot = document.createElement("span");
    dot.className = "dot";

    const label = document.createElement("span");
    label.textContent = status;

    const count = document.createElement("span");
    count.className = "n";
    count.textContent = n;

    badge.append(dot, label, count);
    wrap.appendChild(badge);
  }
}

function renderOrders() {
  const body = $("#orders-body");
  body.innerHTML = "";
  $("#orders-empty").classList.toggle("hidden", state.orders.length > 0);

  for (const order of state.orders) {
    const tr = document.createElement("tr");
    tr.dataset.id = order.id;
    if (state.detail && state.detail.order.id === order.id) {
      tr.classList.add("selected");
    }

    tr.appendChild(cell("order-id", shortId(order.id)));
    tr.appendChild(cell("customer", order.customer));
    tr.appendChild(cell("num", order.itemCount));
    tr.appendChild(cell("total num", money(order.totalCents)));

    const statusTd = document.createElement("td");
    statusTd.appendChild(chip(order.status));
    tr.appendChild(statusTd);

    tr.appendChild(cell("age", relativeTime(order.placedAt)));

    tr.addEventListener("click", () => openDetail(order.id));
    body.appendChild(tr);
  }
}

function cell(cls, text) {
  const td = document.createElement("td");
  td.className = cls;
  td.textContent = text;
  return td;
}

function chip(status) {
  const span = document.createElement("span");
  span.className = `chip s-${status}`;
  span.textContent = status;
  return span;
}

/* ============ outbox monitor ============ */

function renderPending() {
  const el = $("#pending-count");
  el.textContent = state.pending;
  el.classList.toggle("busy", state.pending > 0);
}

function prependDelivery(d) {
  state.deliveries.unshift(d);
  if (state.deliveries.length > 64) state.deliveries.length = 64;
  renderDeliveries();
}

function renderDeliveries() {
  const list = $("#deliveries");
  list.innerHTML = "";

  if (state.deliveries.length === 0) {
    const li = document.createElement("li");
    li.className = "none";
    li.textContent = "No deliveries yet — place an order to watch the outbox work.";
    list.appendChild(li);
    return;
  }

  for (const d of state.deliveries) {
    const li = document.createElement("li");

    const etype = document.createElement("span");
    etype.className = "etype";
    etype.textContent = d.eventType;

    const target = document.createElement("span");
    target.className = "target";
    target.textContent = d.orderId;
    target.title = d.orderId;

    const v = document.createElement("span");
    v.className = "v";
    v.textContent = "@" + d.streamVersion;

    const when = document.createElement("span");
    when.className = "when";
    when.textContent = relativeTime(d.deliveredAt);

    li.append(etype, target, v, when);
    list.appendChild(li);
  }
}

/* ============ detail drawer ============ */

async function openDetail(id) {
  const res = await fetch(`/api/orders/${id}`);
  if (!res.ok) {
    toast("Failed to load order", "error");
    return;
  }
  state.detail = await res.json();
  renderDetail();
  renderOrders(); // highlight the selected row
  $("#drawer").classList.remove("hidden");
  $("#drawer-scrim").classList.remove("hidden");
}

function closeDetail() {
  state.detail = null;
  $("#drawer").classList.add("hidden");
  $("#drawer-scrim").classList.add("hidden");
  renderOrders();
}

function renderDetail() {
  const { version, order, timeline } = state.detail;

  $("#drawer-title").textContent = `Order ${shortId(order.id)}`;
  $("#drawer-sub").textContent = `${order.customer} · v${version}`;

  renderStepper(order.status);
  renderItems(order);
  renderActions(order.status);
  renderTimeline(timeline);
}

function renderStepper(status) {
  const stepper = $("#stepper");
  stepper.innerHTML = "";
  stepper.className = "stepper" + (status === "cancelled" ? " cancelled" : "");

  // a cancelled order stalls at the last stage it reached; the timeline in
  // the drawer shows where the cancellation landed
  const reached = status === "cancelled" ? -1 : STEPS.indexOf(status);

  STEPS.forEach((step, i) => {
    const el = document.createElement("div");
    el.className = "step";
    if (reached >= 0) {
      if (i < reached) el.classList.add("done");
      if (i === reached) el.classList.add("current");
    }

    const node = document.createElement("div");
    node.className = "node";

    const label = document.createElement("div");
    label.className = "label";
    label.textContent = step;

    el.append(node, label);
    stepper.appendChild(el);
  });

  document.querySelectorAll(".cancel-note").forEach((el) => el.remove());
  if (status === "cancelled") {
    const note = document.createElement("div");
    note.className = "cancel-note";
    note.textContent = "This order was cancelled before shipping.";
    stepper.after(note);
  }
}

function renderItems(order) {
  const list = $("#drawer-items");
  list.innerHTML = "";

  for (const item of order.items || []) {
    const li = document.createElement("li");

    const sku = document.createElement("span");
    sku.className = "sku";
    sku.textContent = item.sku;

    const name = document.createElement("span");
    name.className = "name";
    name.textContent = item.name;

    const qty = document.createElement("span");
    qty.className = "qty";
    qty.textContent = `×${item.qty}`;

    const price = document.createElement("span");
    price.className = "price";
    price.textContent = money(item.qty * item.priceCents);

    li.append(sku, name, qty, price);
    list.appendChild(li);
  }

  $("#drawer-total").textContent = money(order.totalCents);
}

function renderActions(status) {
  const wrap = $("#drawer-actions");
  wrap.innerHTML = "";

  const actions = ACTIONS[status] || [];
  if (actions.length === 0) {
    const note = document.createElement("span");
    note.className = "terminal";
    note.textContent = TERMINAL_NOTES[status] || "No actions available.";
    wrap.appendChild(note);
    return;
  }

  for (const a of actions) {
    const btn = document.createElement("button");
    btn.className = "btn " + a.cls;
    btn.textContent = a.label;
    btn.addEventListener("click", () => act(a.action));
    wrap.appendChild(btn);
  }
}

function renderTimeline(timeline) {
  const list = $("#timeline");
  list.innerHTML = "";

  for (const entry of timeline || []) {
    const li = document.createElement("li");

    const v = document.createElement("span");
    v.className = "v";
    v.textContent = "v" + entry.version;

    const desc = document.createElement("span");
    desc.textContent = entry.description;

    const when = document.createElement("span");
    when.className = "when";
    when.textContent = relativeTime(entry.timestamp);

    li.append(v, desc, when);
    list.appendChild(li);
  }
}

/* ============ chrome ============ */

function wireChrome() {
  $("#new-order").addEventListener("click", newOrder);
  $("#drawer-close").addEventListener("click", closeDetail);
  $("#drawer-scrim").addEventListener("click", closeDetail);

  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && state.detail) closeDetail();
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
    `The command expected the order at <code>v${err.expectedVersion}</code>, but it is at ` +
    `<code>v${err.actualVersion}</code>. Estoria saves with <code>ExpectVersion</code>, so the ` +
    `event store rejected the append with a <code>StreamVersionMismatchError</code>. ` +
    `The order has been refreshed.`);
}

function shortId(id) {
  return String(id).slice(0, 8);
}

function money(cents) {
  const sign = cents < 0 ? "-" : "";
  const abs = Math.abs(cents);
  return `${sign}$${Math.floor(abs / 100)}.${String(abs % 100).padStart(2, "0")}`;
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
