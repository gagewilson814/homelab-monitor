const POLL_INTERVAL_MS = 5000;

const fleetEl = document.getElementById("fleet");
const statusEl = document.getElementById("status");

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

function barClass(pct) {
  if (pct >= 90) return "bad";
  if (pct >= 70) return "warn";
  return "";
}

function metricRow(label, pct) {
  return `
    <div class="metric">
      <span>${label}</span>
      <span class="bar"><i class="${barClass(pct)}" style="width:${pct}%"></i></span>
      <span>${pct.toFixed(1)}%</span>
    </div>`;
}

function renderCard(agent) {
  if (agent.error) {
    return `
      <div class="card offline">
        <h2><span class="dot"></span>${agent.address}</h2>
        <div class="error-text">${agent.error}</div>
      </div>`;
  }

  const d = agent.data;
  return `
    <div class="card">
      <h2><span class="dot"></span>${d.hostname}</h2>
      ${metricRow("CPU", d.cpu_usage)}
      ${metricRow("Memory", d.memory_usage)}
      ${metricRow("Disk", d.disk_usage)}
      <div class="metric"><span>Uptime</span><span>${formatUptime(d.uptime)}</span></div>
      <div class="metric"><span>Address</span><span>${agent.address}</span></div>
    </div>`;
}

async function refresh() {
  try {
    const res = await fetch("/api/fleet");
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

refresh();
setInterval(refresh, POLL_INTERVAL_MS);
