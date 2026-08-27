/* Ledger rebuild console: polls /api/overview and renders the projection
   lifecycle — the versioned tables, the in-flight attempt, and the operator
   controls whose availability follows the attempt's phase. */

const $ = (id) => document.getElementById(id);

let overview = null;
let offerOverride = false;

/* ============ polling ============ */

async function poll() {
  try {
    const resp = await fetch("/api/overview");
    if (!resp.ok) throw new Error(`overview: HTTP ${resp.status}`);

    overview = await resp.json();
    $("conn-pill").textContent = "live";
    $("conn-pill").className = "pill ok";
    render();
  } catch (err) {
    $("conn-pill").textContent = "disconnected";
    $("conn-pill").className = "pill err";
  }
}

setInterval(poll, 1000);
poll();

/* ============ rendering ============ */

function render() {
  const o = overview;
  const phase = o.attempt ? o.attempt.phase : "none";
  const runActive = o.run.active;

  $("live-pill").textContent = o.live ? `reads → ${o.live.id}` : "no live version";

  const traffic = $("traffic-toggle");
  traffic.textContent = o.trafficEnabled ? `⏸ Stop traffic (${o.trafficWrites})` : "▶ Start traffic";

  renderLifecycle(o, phase, runActive);
  renderPolicy(o);
  renderVersions(o);
}

function renderLifecycle(o, phase, runActive) {
  const badge = $("phase-badge");
  badge.textContent = phase;
  badge.className = `badge ${phase}`;

  const info = $("attempt-info");

  if (!o.attempt) {
    info.innerHTML = o.live
      ? row("live", o.live.id) + row("revision", `cutover #${o.cutoverRevision}`) + row("allocated", `v${o.allocated}`)
      : `<div class="hint">No read model exists yet. Start the first rebuild to build, promote, and serve <span class="v">account_balances_v1</span>.</div>`;
  } else {
    let html = row("target", o.attempt.target.id) + row("reason", o.attempt.reason);
    if (o.attempt.previous) html += row("previous", o.attempt.previous.id);
    if (o.attempt.runner) html += row("runner", o.attempt.runner.slice(0, 8) + (o.attempt.claimStanding ? " · claim standing" : " · released"));
    info.innerHTML = html;
  }

  const status = $("run-status");
  if (runActive) {
    status.className = "active";
    status.textContent = "run active in this process: building, tailing, and reconciling…";
  } else if (o.run.finished && o.run.failed) {
    status.className = "failed";
    status.textContent = `run ended: ${o.run.result}`;
  } else if (o.run.finished) {
    status.className = "clean";
    status.textContent = "run ended cleanly";
  } else {
    status.className = "hidden";
  }

  show("begin-form", !o.attempt);

  const resumable = o.attempt && !runActive;
  show("resume-form", resumable);
  if (resumable) {
    const standing = o.attempt.claimStanding;
    show("takeover-fields", standing);
    $("resume-hint").textContent = standing
      ? "The recorded runner has not released its claim. If that process is provably gone, take the claim over — the attestation is recorded durably in the claim."
      : "No process is driving this attempt. Resume the build in this process.";
  }

  show("attempt-actions", Boolean(o.attempt));
  if (!o.attempt) offerOverride = false;

  const building = ["created", "building", "caught_up"].includes(phase);

  show("promote-btn", runActive && building);
  $("promote-btn").disabled = phase !== "caught_up";
  $("promote-btn").title = phase === "caught_up"
    ? "Cut reads over to the freshly built version"
    : "Promotion requires the run's catch-up certification";

  show("rollback-btn", phase === "promoted");
  show("retire-btn", (phase === "promoted" || phase === "retiring") && !offerOverride);
  $("retire-btn").textContent = phase === "retiring" ? "🗑 Retry retirement" : "🗑 Retire previous";
  show("abandon-form", building);
  show("override-form", offerOverride && (phase === "promoted" || phase === "retiring"));
}

function renderPolicy(o) {
  const badge = $("policy-badge");

  if (o.policy.generation === 0) {
    badge.textContent = "none recorded";
    badge.className = "badge";
  } else if (o.policy.unwitnessed) {
    badge.textContent = `gen ${o.policy.generation} · unwitnessed`;
    badge.className = "badge retiring";
  } else {
    badge.textContent = `gen ${o.policy.generation} · ${o.policy.witnesses.join(", ")}`;
    badge.className = "badge promoted";
  }
}

function renderVersions(o) {
  const container = $("versions");
  container.innerHTML = "";

  if (o.versions.length === 0) {
    container.innerHTML = `<div class="card"><div class="empty">No projection versions yet — open some accounts, start traffic, and begin the first rebuild.</div></div>`;
    return;
  }

  for (const version of o.versions) {
    container.appendChild(versionCard(version));
  }
}

function versionCard(version) {
  const card = document.createElement("div");
  card.className = "card";

  const roles = version.roles.map((role) => `<span class="role ${role}">${role}</span>`).join("");
  const checkpoint = version.checkpoint
    ? `<span class="checkpoint">checkpoint ${version.checkpoint.position} · <span class="${version.checkpoint.ageSeconds < 3 ? "fresh" : "stale"}">${version.checkpoint.ageSeconds.toFixed(1)}s ago</span></span>`
    : `<span class="checkpoint">no checkpoint</span>`;

  let body;

  if (!version.exists) {
    body = `<div class="empty">Table not built yet.</div>`;
  } else if (version.rows.length === 0) {
    body = `<div class="empty">No rows yet.</div>`;
  } else {
    const extra = version.enriched ? `<th class="num">Deposits</th><th class="num">Withdrawals</th><th>Last activity</th>` : "";
    const isLive = version.roles.includes("live");

    const rows = version.rows.map((row) => {
      const extraCells = version.enriched
        ? `<td class="num">${row.deposits ?? 0}</td><td class="num">${row.withdrawals ?? 0}</td><td class="muted">${row.lastActivity ? timeAgo(row.lastActivity) : "—"}</td>`
        : "";
      const actions = isLive
        ? `<td><span class="actions-inline">
             <button class="btn" onclick="amount('${row.accountId}','deposit')">+$10</button>
             <button class="btn" onclick="amount('${row.accountId}','withdraw')">−$10</button>
           </span></td>`
        : "";

      return `<tr><td>${escapeHtml(row.holder)}</td><td class="num">${money(row.balance)}</td>${extraCells}${actions}</tr>`;
    }).join("");

    body = `<div class="table-scroll"><table class="balances">
      <thead><tr><th>Holder</th><th class="num">Balance</th>${extra}${isLive ? "<th></th>" : ""}</tr></thead>
      <tbody>${rows}</tbody>
    </table></div>`;
  }

  card.innerHTML = `<div class="version-head">
      <span class="table-name">${version.ref.id}</span>${roles}${checkpoint}
    </div>${body}`;

  return card;
}

/* ============ actions ============ */

async function post(path, body) {
  const resp = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body ?? {}),
  });

  let payload = {};
  try { payload = await resp.json(); } catch { /* no body */ }

  if (!resp.ok) {
    toast(payload.error || `HTTP ${resp.status}`);
    return { ok: false, ...payload };
  }

  poll();
  return { ok: true, ...payload };
}

$("traffic-toggle").onclick = () => post("/api/traffic", { enabled: !overview?.trafficEnabled });

$("begin-form").onsubmit = (e) => {
  e.preventDefault();
  post("/api/rebuild", { reason: $("begin-reason").value });
  $("begin-reason").value = "";
};

$("resume-form").onsubmit = (e) => {
  e.preventDefault();
  post("/api/rebuild/resume", {
    takeoverActor: $("takeover-actor").value,
    takeoverReason: $("takeover-reason").value,
  });
};

$("promote-btn").onclick = () => post("/api/rebuild/promote");
$("rollback-btn").onclick = () => post("/api/rebuild/rollback");

$("retire-btn").onclick = async () => {
  const result = await post("/api/rebuild/retire");
  if (!result.ok && !result.claimStanding && !result.notCertified) offerOverride = true;
};

$("abandon-form").onsubmit = (e) => {
  e.preventDefault();
  post("/api/rebuild/abandon", { cause: $("abandon-cause").value });
  $("abandon-cause").value = "";
};

$("override-form").onsubmit = async (e) => {
  e.preventDefault();
  const result = await post("/api/rebuild/retire", {
    overrideActor: $("override-actor").value,
    overrideReason: $("override-reason").value,
  });
  if (result.ok) offerOverride = false;
};

$("policy-form").onsubmit = (e) => {
  e.preventDefault();
  const unwitnessed = document.querySelector('input[name="policy"]:checked').value === "unwitnessed";
  post("/api/policy", {
    witnesses: unwitnessed ? [] : ["router"],
    unwitnessed,
    actor: $("policy-actor").value,
    reason: $("policy-reason").value,
  });
};

$("account-form").onsubmit = (e) => {
  e.preventDefault();
  const dollars = parseFloat($("account-deposit").value || "0");
  post("/api/accounts", {
    holder: $("account-holder").value,
    deposit: Math.round(dollars * 100),
  });
  $("account-holder").value = "";
  $("account-deposit").value = "";
};

function amount(accountId, verb) {
  post(`/api/accounts/${accountId}/${verb}`, { amount: 1000 });
}

/* ============ helpers ============ */

function row(key, value) {
  return `<div class="row"><span class="k">${key}</span><span class="v">${escapeHtml(value)}</span></div>`;
}

function show(id, visible) {
  $(id).classList.toggle("hidden", !visible);
}

function money(cents) {
  return (cents / 100).toLocaleString("en-US", { style: "currency", currency: "USD" });
}

function timeAgo(iso) {
  const seconds = (Date.now() - new Date(iso).getTime()) / 1000;
  if (seconds < 60) return `${Math.max(0, Math.round(seconds))}s ago`;
  if (seconds < 3600) return `${Math.round(seconds / 60)}m ago`;
  return `${Math.round(seconds / 3600)}h ago`;
}

function escapeHtml(text) {
  const div = document.createElement("div");
  div.textContent = String(text);
  return div.innerHTML;
}

let toastTimer = null;

function toast(message) {
  const el = $("toast");
  el.textContent = message;
  el.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.add("hidden"), 6000);
}
