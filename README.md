# Home Server Monitoring Dashboard

A lightweight, self-hosted monitoring and control system for a home server fleet. A small Go agent runs on each machine and reports live system stats; a central backend and web dashboard (planned) will aggregate that data and allow authenticated remote actions like restarting a server, all accessible from a phone.

> 🚧 **Status: Work in progress.** This project is being built incrementally as a hands-on learning exercise. See the Roadmap below for what's actually done vs. planned.

## Why

Most existing monitoring tools are either overkill for a home lab or don't support mixed Linux/Windows environments cleanly. This project is scoped specifically for personal infrastructure: simple to deploy, works across OSes, and reachable from a phone without exposing anything to the public internet. Also, I just wanted to build something that I can use.

## Architecture

- **Agent** (Go) — runs on each monitored machine. Exposes a `/stats` HTTP endpoint that returns hostname, CPU usage, memory usage, disk usage, and uptime as JSON, plus the up/down status of any locally-configured services (see `HOMELAB_SERVICES` below). Uses [`gopsutil`](https://github.com/shirou/gopsutil) for cross-platform system metrics, and compiles to a single static binary for both Linux and Windows.
- **Backend** (Go) — a central service running on the home network that polls all Agents concurrently on its own background schedule (one goroutine per agent, pull model — no NAT/port-forwarding complexity since everything stays on the home network or a VPN), aggregates the results into an in-memory cache, and serves that cache as JSON at `/api/fleet`. Polling and Discord alerting run independently of whether anyone has the dashboard open. Also serves the static frontend.
- **Frontend** — a minimal vanilla JS/HTML/CSS dashboard served by the Backend. Polls `/api/fleet` every 5s (a fast read of the Backend's cache, not a live agent scrape) and renders live per-agent stat cards, including agents that are unreachable. Gated behind a login page; no database yet — see Roadmap.

```
[ Agent : Linux server ]  \
[ Agent : Linux server ]   >---(Backend polls each Agent concurrently)---> [ Backend API :9090 ] ---> [ Web Dashboard ]
[ Agent : Windows server]  /
```

## Tech Stack

| Layer     | Tech                                  |
|-----------|----------------------------------------|
| Agent     | Go, [gopsutil](https://github.com/shirou/gopsutil) |
| Backend   | Go (goroutines for concurrent polling) |
| Frontend  | Vanilla JavaScript + HTML/CSS (SQL-backed multi-user login planned) |
| Auth      | Single-user session cookie (bcrypt password, in-memory sessions) |

## Getting Started

Requires [Go](https://go.dev/dl/) installed.

### Agent

Run one instance per monitored machine (or multiple locally on different ports for testing via `AGENT_PORT`):

```bash
# run locally (defaults to :8080)
go run ./cmd/agent

# run on a different port
AGENT_PORT=8081 go run ./cmd/agent

# build a binary for the current OS
go build -o agent ./cmd/agent

# cross-compile for Windows from Linux/macOS (or vice versa)
GOOS=windows GOARCH=amd64 go build -o agent.exe ./cmd/agent
```

Once running, the Agent serves stats at:

```
GET http://<agent-host>:<port>/stats
```

To also check that specific services are answering on that machine (not just that the host itself is up), set `HOMELAB_SERVICES` to a comma-separated list of `name:port` pairs:

```bash
HOMELAB_SERVICES='jellyfin:8096,plex:32400' go run ./cmd/agent
```

Each check is a plain TCP dial to `localhost:<port>` — it confirms something is listening, not that the service is actually healthy. Results show up in the Agent's `/stats` response as a `services` array (`{"name": "jellyfin", "port": 8096, "up": true}`), render on the dashboard card, and get their own debounced Discord alert independent of the host-level up/down alert — a hung Jellyfin on an otherwise-healthy host now shows up.

### Backend + Dashboard

The backend must be run from the repo root (it serves the `web/` directory) or run through the go tool below. It polls agents listed in the comma-separated `HOMELAB_AGENTS` env var (defaults to `localhost:8080,localhost:8081`) on its own background schedule, every `HOMELAB_POLL_INTERVAL` seconds (default `5`) — this runs continuously regardless of whether the dashboard is open, so alerting stays live even if nobody's looking at it.

The dashboard and `/api/fleet` require a login. Set `HOMELAB_PASSWORD_HASH` to a bcrypt hash of your chosen password — the backend refuses to start without it. Generate one with the bundled `hashpw` tool:

```bash
go run ./cmd/hashpw
# Password: <type your password, Enter>
# $2a$10$...
```

Then run the backend with that hash:

```bash
# run locally, override which agents to poll
HOMELAB_PASSWORD_HASH='$2a$10$...' HOMELAB_AGENTS="192.168.1.10:8080,192.168.1.11:8080" go run ./cmd/backend
```

Then open the dashboard and log in:

```
http://localhost:9090/
```

Sessions are cookie-based, last 24h, and are held in memory (a backend restart logs everyone out). Agents themselves are not authenticated — they're only expected to be reachable from the home network/VPN, not the public internet.

Aggregated JSON is also available directly at `http://localhost:9090/api/fleet` (requires the same session cookie).

### Discord alerts (optional)

Set `DISCORD_WEBHOOK_URL` to get a Discord message whenever an agent's online/offline state changes (e.g. a server dropping off the network). Alerts are debounced — an agent has to return the same result for `DISCORD_ALERT_THRESHOLD` consecutive background polls (default `2`, so ~10s at the default 5s poll interval) before a transition is confirmed, so a single dropped poll won't page you. Leave `DISCORD_WEBHOOK_URL` unset to disable alerting entirely.

```bash
HOMELAB_PASSWORD_HASH='$2a$10$...' \
DISCORD_WEBHOOK_URL='https://discord.com/api/webhooks/...' \
go run ./cmd/backend
```

## Testing

Tests use the standard library `testing` framework with `net/http/httptest` fakes wherever a live dependency would be needed (a fake Agent server and a recording Discord notifier), so the whole suite runs with no network access:

```bash
# run the whole suite
 go test ./...

# one package, or a single test by name
 go test ./internal/auth/ -run TestValid

# race detector
 go test -race ./...
```

| Package | What's tested |
|---------|---------------|
| `internal/auth` | login/logout flow, bad password, missing hash |
| `internal/stats` | `stats()` response shape + JSON wire format |
| `internal/notify` | Discord `Send` is a no-op when the webhook is unset; error on a bad status |
| `internal/alert` | debounce thresholds, online/offline transitions |
| `cmd/backend` | `poll`/`pollAll` error handling, alert transitions, fleet handler + auth gate, agent-list parsing |

Everything runs on the same platform you build on. The `gopsutil`-based stats tests only cover the host they run on, but the backend and agent logic is platform-independent.

## Configuration

| Env var | Purpose | Default |
|---------|---------|---------|
| `HOMELAB_PASSWORD_HASH` | bcrypt hash of the dashboard/API password (required) | — |
| `HOMELAB_AGENTS` | comma-separated `host:port` agents to poll | `localhost:8080,localhost:8081` |
| `HOMELAB_POLL_INTERVAL` | backend poll cadence, in seconds | `5` |
| `DISCORD_WEBHOOK_URL` | Discord webhook for online/offline alerts (optional) | unset = no alerts |
| `DISCORD_ALERT_THRESHOLD` | consecutive polls to confirm a transition | `2` |

## Roadmap

- [x] Walking-skeleton HTTP server with `/stats` endpoint
- [x] Real hostname, CPU, and memory metrics via `gopsutil`
- [x] Real disk usage metrics
- [x] Real uptime metrics
- [x] Backend service that polls Agents across the home network (concurrent, via goroutines)
- [x] Minimal read-only web dashboard (no SQL yet)
- [x] Single-user session-cookie authentication for the dashboard/API (non-negotiable before restart ships)
- [x] Discord alerts on agent online/offline transitions (debounced)
- [x] Backend polls on its own background schedule, independent of the dashboard being open
- [x] Service-level checks (e.g. is Jellyfin's port actually answering, not just the host)
- [ ] Remote restart capability (deprioritized for now)
- [ ] SQL-backed dashboard with user login
- [ ] Mobile-friendly / installable (PWA) dashboard access
- [ ] Threshold alerts (sustained high CPU/mem, disk nearing full)
- [ ] "Last seen" timestamp per agent on the dashboard
- [ ] Agents run as a system service (systemd/Windows service) so they survive a reboot unattended

## License

TBD
