// How often the dashboard re-queries the backend (every 5s). The backend
// itself polls each agent, so this just controls how fast the view updates.
const POLL_INTERVAL_MS = 5000;

// DOM anchors used by the render/update loop.
const fleetEl = document.getElementById("fleet");
const statusEl = document.getElementById("status");
const errorBannerEl = document.getElementById("error-banner");
const alertsListEl = document.getElementById("alerts-list");
const alertsEmptyEl = document.getElementById("alerts-empty");
const navAlertBadge = document.getElementById("nav-alert-badge");

// Last successfully-fetched fleet, kept on screen through a failed poll so
// the dashboard never blanks out and reads as a crash.
let lastGoodAgents = null;
let hasLoadedOnce = false;

// Format a raw second count as a human string like "3d 5h 12m", dropping
// the empty higher-order units (so 90 minutes won't print "0d 1h 30m").
function formatUptime(seconds) {
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const parts = [];
  if (d) parts.push(`${d}d`);
  if (h) parts.push(`${h}h`);
  parts.push(`${m}m`);
  return parts.join(" ");
}

// Format an ISO timestamp as a short relative string like "just now",
// "5m ago", "3h ago", falling back to whole days beyond that.
function formatLastSeen(iso) {
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(iso).getTime()) / 1000));
  if (seconds < 60) return "just now";
  const m = Math.floor(seconds / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

// Escape text for safe interpolation into innerHTML. Only actually needed
// for user-entered fields (currently just the tag), but cheap enough to
// apply everywhere it's used.
//
// Quotes matter as much as angle brackets here: several call sites
// interpolate into a double-quoted attribute (data-*, aria-label), where an
// unescaped " lets a value close the attribute and inject new ones - an
// event handler included. The textContent/innerHTML trick escapes & < >
// but NOT quotes, so this does the replacement explicitly.
function escapeHtml(str) {
  return String(str ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

// Map a percentage to a CSS class that colors its bar: green default,
// yellow from 70%, red from 90%.
function barClass(pct) {
  if (pct >= 90) return "bad";
  if (pct >= 70) return "warn";
  return "";
}

// Build a small inline SVG trend line from a metric's recent history.
// Falls back to nothing (caller renders a plain bar instead) until at
// least two samples exist.
function sparkline(values, pct) {
  if (!values || values.length < 2) return "";
  const w = 100;
  const h = 22;
  const min = Math.min(...values);
  const max = Math.max(...values);
  const range = max - min || 1;
  const points = values
    .map((v, i) => {
      const x = (i / (values.length - 1)) * w;
      const y = h - ((v - min) / range) * h;
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");
  const cls = barClass(pct);
  return `<svg class="spark ${cls}" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" aria-hidden="true"><polyline points="${points}" /></svg>`;
}

// Build one metric row (label + sparkline/bar + value) for a card. Renders
// a sparkline once history exists, otherwise falls back to the flat bar so
// the first couple of polls still show something.
function metricRow(label, pct, history) {
  const cls = barClass(pct);
  const trend = sparkline(history, pct);
  const track = trend || `<span class="bar"><i class="${cls}" style="width:${pct}%"></i></span>`;
  return `
    <div class="metric">
      <span>${label}</span>
      ${track}
      <span>${pct.toFixed(1)}%</span>
    </div>`;
}

// Build one service-check row (name + up/down dot) for a card. The name
// comes straight from the polled agent, so it's untrusted input as far as
// this dashboard is concerned - escape it. A service whose agent
// advertises a restart action (svc.action set) also gets a restart
// button; the button carries data-* attributes the delegated click handler
// below reads to arm the confirm modal.
function serviceRow(svc, address) {
  const restartBtn = svc.action
    ? `<button class="restart-btn" type="button" data-service="${escapeHtml(svc.name)}" data-address="${escapeHtml(address)}" aria-label="Restart ${escapeHtml(svc.name)}">Restart</button>`
    : "";
  return `
    <div class="metric">
      <span class="service-name"><span class="dot${svc.up ? "" : " bad"}"></span>${escapeHtml(svc.name)}</span>
      <span class="service-actions">${restartBtn}<span>${svc.up ? "up" : "down"}</span></span>
    </div>`;
}

// Build the card's title row: status dot, display name (tag if set, else
// hostname/address), an optional tag chip, the online/offline text label,
// and an edit button that opens the add/edit-agent modal for this address.
function cardHeader(agent, online, primaryLabel) {
  const tagChip = agent.tag ? `<span class="tag-chip">${escapeHtml(agent.tag)}</span>` : "";
  const editBtn = `<button
      class="card-edit-btn"
      type="button"
      data-address="${escapeHtml(agent.address)}"
      data-tag="${escapeHtml(agent.tag || "")}"
      aria-label="Edit ${escapeHtml(agent.tag || primaryLabel)}"
    >✎</button>`;
  return `
    <h2>
      <span class="dot${online ? "" : " pulse"}"></span><span class="card-host">${escapeHtml(primaryLabel)}</span>${tagChip}
      <span class="card-header-actions">
        <span class="state-label">${online ? "online" : "offline"}</span>
        ${editBtn}
      </span>
    </h2>`;
}

// Render a single agent card. Offline agents (with an error) get a distinct
// "offline" styling and show the failure reason; healthy agents render
// their hostname and metric rows.
function renderCard(agent) {
  const lastSeenRow = agent.last_seen
    ? `<div class="metric"><span>Last seen</span><span>${formatLastSeen(agent.last_seen)}</span></div>`
    : "";

  if (agent.error) {
    return `
      <div class="card offline">
        ${cardHeader(agent, false, agent.address)}
        <div class="error-text">${escapeHtml(agent.error)}</div>
        ${lastSeenRow}
      </div>`;
  }

  const d = agent.data;
  const h = agent.history || {};
  const services = (d.services || []).map((svc) => serviceRow(svc, agent.address)).join("");
  return `
    <div class="card">
      ${cardHeader(agent, true, d.hostname)}
      ${metricRow("CPU", d.cpu_usage, h.cpu)}
      ${metricRow("Memory", d.memory_usage, h.mem)}
      ${metricRow("Disk", d.disk_usage, h.disk)}
      <div class="metric"><span>Uptime</span><span>${formatUptime(d.uptime)}</span></div>
      <div class="metric"><span>Address</span><span>${escapeHtml(agent.address)}</span></div>
      ${lastSeenRow}
      ${services}
    </div>`;
}

// Loading skeleton shown only before the first successful poll lands, so
// the dashboard doesn't sit on a blank grid for the first ~5s.
function renderSkeleton() {
  const card = `
    <div class="card skeleton" aria-hidden="true">
      <h2><span class="skeleton-chip" style="width:60%"></span></h2>
      <div class="skeleton-line"></div>
      <div class="skeleton-line"></div>
      <div class="skeleton-line"></div>
      <div class="skeleton-line" style="width:40%"></div>
    </div>`;
  fleetEl.innerHTML = card.repeat(4);
}

// Shown in place of the skeleton if the very first poll fails - there's no
// last-known fleet to fall back to yet, so a bare retry banner over a blank
// grid would read as broken rather than loading.
function renderFirstLoadError() {
  fleetEl.innerHTML = `
    <div class="empty-state">
      <p>Can't reach the backend</p>
      <p class="muted">Retrying automatically…</p>
    </div>`;
}

function renderEmpty() {
  fleetEl.innerHTML = `
    <div class="empty-state">
      <p>No agents yet</p>
      <p class="muted">Set HOMELAB_AGENTS on the backend to start monitoring hosts.</p>
    </div>`;
}

function showErrorBanner(message) {
  errorBannerEl.querySelector(".error-banner-text").textContent = message;
  errorBannerEl.hidden = false;
}

function hideErrorBanner() {
  errorBannerEl.hidden = true;
}

// Alert type -> short icon glyph, kept alongside the text label so status
// never depends on color alone.
const ALERT_ICON = {
  offline: "●",
  service: "⚠",
  threshold: "▲",
};

function formatSince(iso) {
  return formatLastSeen(iso);
}

// An alert message is composed server-side from the agent's hostname and
// service names, so it carries untrusted content through to the DOM and has
// to be escaped like any other agent-supplied string.
function renderAlert(alert) {
  const icon = ALERT_ICON[alert.type] || "•";
  return `
    <li class="alert-item alert-${escapeHtml(alert.type)}">
      <span class="alert-icon">${icon}</span>
      <span class="alert-body">
        <span class="alert-message">${escapeHtml(alert.message)}</span>
        <span class="alert-since">since ${formatSince(alert.since)}</span>
      </span>
    </li>`;
}

function renderAlerts(alerts) {
  navAlertBadge.textContent = String(alerts.length);
  navAlertBadge.hidden = alerts.length === 0;

  if (alerts.length === 0) {
    alertsListEl.innerHTML = "";
    alertsEmptyEl.hidden = false;
    return;
  }
  alertsEmptyEl.hidden = true;
  alertsListEl.innerHTML = alerts.map(renderAlert).join("");
}

// Fetch the aggregated fleet, re-render every card, and flip the status
// pill to live/error. Errors are caught so a failed poll doesn't crash the
// page - the last-known fleet stays on screen with a retry banner instead.
async function refresh() {
  if (!hasLoadedOnce) renderSkeleton();

  try {
    const [fleetRes, alertsRes] = await Promise.all([fetch("/api/fleet"), fetch("/api/alerts")]);

    if (fleetRes.status === 401 || alertsRes.status === 401) {
      window.location.href = "/login.html";
      return;
    }
    if (!fleetRes.ok) throw new Error(`backend returned ${fleetRes.status}`);
    if (!alertsRes.ok) throw new Error(`backend returned ${alertsRes.status}`);

    const agents = await fleetRes.json();
    const alerts = await alertsRes.json();

    lastGoodAgents = agents;
    hasLoadedOnce = true;
    hideErrorBanner();

    if (agents.length === 0) {
      renderEmpty();
    } else {
      fleetEl.innerHTML = agents.map(renderCard).join("");
    }
    renderAlerts(alerts);

    statusEl.textContent = `live · ${new Date().toLocaleTimeString()}`;
    statusEl.className = "status live";
  } catch (err) {
    statusEl.textContent = `error: ${err.message}`;
    statusEl.className = "status error";

    if (!hasLoadedOnce) {
      renderFirstLoadError();
    }
    // Otherwise lastGoodAgents is already on screen - just surface the
    // retry banner instead of blanking the fleet.
    showErrorBanner("Can't reach backend · retrying…");
  }
}

document.getElementById("retry-btn").addEventListener("click", () => refresh());

document.getElementById("logout").addEventListener("click", async () => {
  await fetch("/api/logout", { method: "POST" });
  window.location.href = "/login.html";
});

// --- Bottom nav (mobile-first tabs) ---------------------------------------

const views = {
  overview: document.getElementById("view-overview"),
  alerts: document.getElementById("view-alerts"),
  settings: document.getElementById("view-settings"),
};
const navButtons = document.querySelectorAll(".nav-btn");
const addAgentFab = document.getElementById("add-agent-fab");

function showView(name) {
  for (const key of Object.keys(views)) {
    views[key].hidden = key !== name;
  }
  navButtons.forEach((btn) => {
    const active = btn.dataset.view === name;
    btn.classList.toggle("active", active);
    btn.setAttribute("aria-current", active ? "page" : "false");
  });
  addAgentFab.hidden = name !== "overview";
}

navButtons.forEach((btn) => {
  btn.addEventListener("click", () => showView(btn.dataset.view));
});

// --- Theme (dark / light / system) ----------------------------------------

const THEME_KEY = "homelab-theme";
const themeSelect = document.getElementById("theme-select");

function applyTheme(theme) {
  if (theme === "system") {
    document.documentElement.removeAttribute("data-theme");
  } else {
    document.documentElement.setAttribute("data-theme", theme);
  }
}

function initTheme() {
  const saved = localStorage.getItem(THEME_KEY) || "system";
  themeSelect.value = saved;
  applyTheme(saved);
}

themeSelect.addEventListener("change", () => {
  localStorage.setItem(THEME_KEY, themeSelect.value);
  applyTheme(themeSelect.value);
});

initTheme();

// --- Pull-to-refresh (touch only, top of the overview list) ---------------

(function setupPullToRefresh() {
  const container = document.getElementById("view-overview");
  const indicator = document.getElementById("pull-indicator");
  const THRESHOLD = 70;
  let startY = null;
  let pulling = false;

  container.addEventListener(
    "touchstart",
    (e) => {
      if (container.scrollTop > 0) return;
      startY = e.touches[0].clientY;
      pulling = true;
    },
    { passive: true }
  );

  container.addEventListener(
    "touchmove",
    (e) => {
      if (!pulling || startY === null) return;
      const delta = e.touches[0].clientY - startY;
      if (delta <= 0) {
        indicator.hidden = true;
        return;
      }
      indicator.hidden = false;
      indicator.textContent = delta > THRESHOLD ? "Release to refresh" : "Pull to refresh";
    },
    { passive: true }
  );

  container.addEventListener("touchend", (e) => {
    if (!pulling || startY === null) return;
    const delta = e.changedTouches[0].clientY - startY;
    pulling = false;
    startY = null;
    indicator.hidden = true;
    if (delta > THRESHOLD) refresh();
  });
})();

// --- Agent management: add via the FAB, edit/remove via a card's ✎ -------

const agentModal = document.getElementById("agent-modal");
const agentModalTitle = document.getElementById("agent-modal-title");
const addressInput = document.getElementById("agent-address-input");
const tagInput = document.getElementById("agent-tag-input");
const modalError = document.getElementById("agent-modal-error");
const removeBtn = document.getElementById("agent-remove-btn");

let modalMode = "add"; // or "edit"
let editingAddress = null;
let confirmingRemove = false;
let removeConfirmTimer = null;

function showModalError(message) {
  modalError.textContent = message;
  modalError.hidden = false;
}

function resetRemoveConfirm() {
  confirmingRemove = false;
  removeBtn.textContent = "Remove server";
  clearTimeout(removeConfirmTimer);
}

function openAddModal() {
  modalMode = "add";
  editingAddress = null;
  agentModalTitle.textContent = "Add server";
  addressInput.value = "";
  addressInput.disabled = false;
  tagInput.value = "";
  modalError.hidden = true;
  removeBtn.hidden = true;
  resetRemoveConfirm();
  agentModal.hidden = false;
  addressInput.focus();
}

function openEditModal(address, tag) {
  modalMode = "edit";
  editingAddress = address;
  agentModalTitle.textContent = "Edit server";
  addressInput.value = address;
  addressInput.disabled = true;
  tagInput.value = tag || "";
  modalError.hidden = true;
  removeBtn.hidden = false;
  resetRemoveConfirm();
  agentModal.hidden = false;
  tagInput.focus();
}

function closeModal() {
  agentModal.hidden = true;
  resetRemoveConfirm();
}

async function saveAgent() {
  const tag = tagInput.value.trim();
  modalError.hidden = true;

  if (modalMode === "add") {
    const address = addressInput.value.trim();
    if (!address) {
      showModalError("Address is required.");
      return;
    }
    const res = await fetch("/api/agents", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ address, tag }),
    });
    if (!res.ok) {
      showModalError(await res.text());
      return;
    }
  } else {
    const res = await fetch("/api/agents", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ address: editingAddress, tag }),
    });
    if (!res.ok) {
      showModalError(await res.text());
      return;
    }
  }

  closeModal();
  refresh();
}

document.getElementById("add-agent-fab").addEventListener("click", openAddModal);
document.getElementById("agent-cancel-btn").addEventListener("click", closeModal);
document.getElementById("agent-save-btn").addEventListener("click", saveAgent);

// One tap arms a 3s confirmation window instead of a blocking native
// confirm() dialog; a second tap within that window actually removes it.
removeBtn.addEventListener("click", async () => {
  if (!confirmingRemove) {
    confirmingRemove = true;
    removeBtn.textContent = "Confirm remove?";
    removeConfirmTimer = setTimeout(resetRemoveConfirm, 3000);
    return;
  }
  const res = await fetch(`/api/agents?address=${encodeURIComponent(editingAddress)}`, { method: "DELETE" });
  if (!res.ok) {
    showModalError(await res.text());
    resetRemoveConfirm();
    return;
  }
  closeModal();
  refresh();
});

agentModal.addEventListener("click", (e) => {
  if (e.target === agentModal) closeModal();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !agentModal.hidden) closeModal();
  if (e.key === "Escape" && !restartModal.hidden) closeRestartModal();
});

// Event delegation: fleetEl's innerHTML is replaced wholesale on every
// refresh, so a single listener on the stable container beats re-binding a
// click handler per card-edit or restart button on every render.
fleetEl.addEventListener("click", (e) => {
  const editBtn = e.target.closest(".card-edit-btn");
  if (editBtn) {
    openEditModal(editBtn.dataset.address, editBtn.dataset.tag);
    return;
  }
  const restartBtn = e.target.closest(".restart-btn[data-service]");
  if (restartBtn) {
    // The card's display name lives in .card-host (see cardHeader), so
    // the confirm modal can name the machine the command will run on.
    const host = restartBtn.closest(".card")?.querySelector(".card-host")?.textContent || "this host";
    openRestartModal(restartBtn.dataset.service, restartBtn.dataset.address, host);
  }
});

// --- Service restart confirm modal ----------------------------------------

const restartModal = document.getElementById("restart-modal");
const restartModalTitle = document.getElementById("restart-modal-title");
const restartModalMessage = document.getElementById("restart-modal-message");
const restartModalError = document.getElementById("restart-modal-error");
const restartModalOutput = document.getElementById("restart-modal-output");
const restartConfirmBtn = document.getElementById("restart-confirm-btn");
const restartCancelBtn = document.getElementById("restart-cancel-btn");

let restartService = null;
let restartAddress = null;
let restartRunning = false;

function showRestartError(message) {
  restartModalError.textContent = message;
  restartModalError.hidden = false;
}

function openRestartModal(service, address, host) {
  restartService = service;
  restartAddress = address;
  restartRunning = false;
  restartModalTitle.textContent = `Restart ${service}`;
  restartModalMessage.textContent = `Restart ${service}? This runs a configured command on ${host}.`;
  restartModalError.hidden = true;
  restartModalOutput.hidden = true;
  restartModalOutput.textContent = "";
  restartConfirmBtn.disabled = false;
  restartConfirmBtn.hidden = false;
  restartCancelBtn.textContent = "Cancel";
  restartModal.hidden = false;
  restartConfirmBtn.focus();
}

function closeRestartModal() {
  if (restartRunning) return; // never strand a running restart
  restartModal.hidden = true;
  restartService = null;
  restartAddress = null;
}

async function confirmRestart() {
  if (!restartService || restartRunning) return;
  restartRunning = true;
  restartModalError.hidden = true;
  restartConfirmBtn.disabled = true;
  restartConfirmBtn.textContent = "Restarting…";
  restartModalOutput.hidden = false;
  restartModalOutput.textContent = "Running restart command…";

  try {
    const res = await fetch(`/api/agents/${encodeURIComponent(restartAddress)}/restart`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ service: restartService }),
    });
    const text = await res.text();
    let payload = null;
    try {
      payload = JSON.parse(text);
    } catch {
      // Non-JSON error bodies (e.g. plaintext http.Error responses).
    }

    if (!res.ok) {
      // Prefer the agent's own output field (it carries the command's
      // real output even on failure); fall back to the raw body.
      const detail = payload?.output || text || `HTTP ${res.status}`;
      showRestartError(`Restart failed: ${detail}`);
      restartModalOutput.textContent = payload?.output ? `output:\n${payload.output}` : detail;
    } else {
      restartModalOutput.textContent = payload?.output ? `output:\n${payload.output}` : "Restart completed with no output.";
      restartConfirmBtn.hidden = true;
      restartCancelBtn.textContent = "Close";
      refresh(); // pick up the new service state on the card
    }
  } catch (err) {
    showRestartError(`Restart failed: ${err.message}`);
  } finally {
    restartRunning = false;
    restartConfirmBtn.disabled = false;
    restartConfirmBtn.textContent = "Restart";
  }
}

restartConfirmBtn.addEventListener("click", confirmRestart);
restartCancelBtn.addEventListener("click", closeRestartModal);
restartModal.addEventListener("click", (e) => {
  if (e.target === restartModal) closeRestartModal();
});

// Kick off the first load, then poll on the interval above.
refresh();
setInterval(refresh, POLL_INTERVAL_MS);
