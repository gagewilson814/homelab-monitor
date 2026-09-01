// How often the dashboard re-queries the backend (every 5s). The backend
// itself polls each agent, so this just controls how fast the view updates.
const POLL_INTERVAL_MS = 5000;

// DOM anchors used by the render/update loop.
const fleetEl = document.getElementById("fleet");
const statusEl = document.getElementById("status");

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

// Map a percentage to a CSS class that colors its bar: green default,
// yellow from 70%, red from 90%.
function barClass(pct) {
  if (pct >= 90) return "bad";
  if (pct >= 70) return "warn";
  return "";
}

// Build one metric row (label + colored bar + value) for a card.
function metricRow(label, pct) {
  return `
    <div class="metric">
      <span>${label}</span>
      <span class="bar"><i class="${barClass(pct)}" style="width:${pct}%"></i></span>
      <span>${pct.toFixed(1)}%</span>
    </div>`;
}

// Build one service-check row (name + up/down dot) for a card.
function serviceRow(svc) {
  return `
    <div class="metric">
      <span class="service-name"><span class="dot${svc.up ? "" : " bad"}"></span>${svc.name}</span>
      <span>${svc.up ? "up" : "down"}</span>
    </div>`;
}

// Render a single agent card. Offline agents (with an error) get a distinct
// "offline" styling and show the failure reason; healthy agents render
// their hostname and metric rows.
function renderCard(agent) {
  if (agent.error) {
    return `
      <div class="card offline">
        <h2><span class="dot"></span>${agent.address}</h2>
        <div class="error-text">${agent.error}</div>
      </div>`;
  }

  const d = agent.data;
  const services = (d.services || []).map(serviceRow).join("");
  return `
    <div class="card">
      <h2><span class="dot"></span>${d.hostname}</h2>
      ${metricRow("CPU", d.cpu_usage)}
      ${metricRow("Memory", d.memory_usage)}
      ${metricRow("Disk", d.disk_usage)}
      <div class="metric"><span>Uptime</span><span>${formatUptime(d.uptime)}</span></div>
      <div class="metric"><span>Address</span><span>${agent.address}</span></div>
      ${services}
    </div>`;
}

// Fetch the aggregated fleet, re-render every card, and flip the status
// pill to live/error. Errors are caught so a failed poll doesn't crash the
// page — it just shows the error state until the next successful poll.
async function refresh() {
  try {
    const res = await fetch("/api/fleet");
    if (res.status === 401) {
      window.location.href = "/login.html";
      return;
    }
    if (!res.ok) throw new Error(`backend returned ${res.status}`);
    const agents = await res.json();

    fleetEl.innerHTML = agents.map(renderCard).join("");
    statusEl.textContent = `live · ${new Date().toLocaleTimeString()}`;
    statusEl.className = "status live";
  } catch (err) {
    statusEl.textContent = `error: ${err.message}`;
    statusEl.className = "status error";
  }
}

document.getElementById("logout").addEventListener("click", async () => {
  await fetch("/api/logout", { method: "POST" });
  window.location.href = "/login.html";
});

// Kick off the first load, then poll on the interval above.
refresh();
setInterval(refresh, POLL_INTERVAL_MS);
