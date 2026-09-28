# Homelab Monitor

Homelab Monitor is a small, self-hosted, read-only dashboard for a mixed
Linux/Windows fleet. An agent runs on each monitored machine; the backend
polls agents, tracks availability and resource use, sends optional Discord
alerts, and serves a mobile-friendly dashboard.

The project deliberately does **not** execute commands on monitored hosts.
Remote restarts and browser SSH are out of scope.

## What it does

- Collects hostname, CPU, memory, disk use, uptime, and optional local TCP
  service checks from each agent.
- Polls agents concurrently on a background schedule and keeps a cached
  fleet view, metric trends, and last-seen time.
- Sends debounced Discord alerts for host/service outages and sustained
  CPU, memory, or disk thresholds.
- Provides authenticated multi-user dashboard access with SQLite-backed
  accounts and sessions.
- Lets an authenticated user add, tag, and remove agent addresses.

## Architecture

```text
Linux / Windows host                  AWS EC2
+-------------------+             +----------------------------+
| agent :8080       | <-Tailscale-| backend -> dashboard        |
| /stats only       |             | SQLite + HTTPS via Serve    |
+-------------------+             +----------------------------+
```

Agents listen locally and are reachable only through your LAN or tailnet.
For the recommended EC2 layout, the backend calls each agent over its
Tailscale name or IP and the dashboard is exposed privately through
Tailscale Serve. No EC2 inbound security-group rule is needed.

## Local quick start

Install the Go version declared in `go.mod`, then run an agent:

```bash
go run ./cmd/agent
```

The agent serves `GET /stats` on port `8080`. Add service checks with
`HOMELAB_SERVICES`:

```bash
HOMELAB_SERVICES='jellyfin:8096,plex:32400' go run ./cmd/agent
```

From the repository root, seed the first dashboard account and start the
backend:

```bash
go run ./cmd/backend seed
HOMELAB_AGENTS='127.0.0.1:8080' go run ./cmd/backend
```

Open `http://localhost:9090/` and log in. `HOMELAB_AGENTS` only seeds the
file-backed fleet on its first run; later changes are made in the dashboard
or by editing the configured agents file.

## Tailscale and EC2 deployment

The deployment guide is [deploy/EC2_TAILSCALE.md](deploy/EC2_TAILSCALE.md).
It covers an Amazon Linux EC2 backend, Session Manager (no SSH key or port
22), Tailscale ACLs, private HTTPS through Tailscale Serve, systemd, and
verification. The key deployment settings are:

```ini
# /etc/homelab-monitor/backend.env on EC2
HOMELAB_PORT=127.0.0.1:9090
HOMELAB_COOKIE_SECURE=1
HOMELAB_AGENT_TOKEN=<one-long-random-secret>
HOMELAB_AGENTS=server-a:8080,server-b:8080
```

Set the same `HOMELAB_AGENT_TOKEN` on every agent. It is defense in depth:
Tailscale controls network reachability, while the token prevents an
unauthorized peer from reading or impersonating an agent response.

## Configuration

| Variable | Purpose | Default |
|---|---|---|
| `AGENT_PORT` | Agent listen port | `8080` |
| `HOMELAB_SERVICES` | Agent-local `name:port` TCP checks | unset |
| `HOMELAB_PORT` | Backend listen address | `:9090` |
| `HOMELAB_DB_FILE` | SQLite users and sessions file | `data/homelab.db` |
| `HOMELAB_AGENTS` | Initial comma-separated `host:port` seed list | `localhost:8080,localhost:8081` |
| `HOMELAB_AGENTS_FILE` | Persisted fleet configuration | `data/agents.json` |
| `HOMELAB_AGENT_TOKEN` | Shared token required by agent `/stats` and sent by backend polls | unset |
| `HOMELAB_COOKIE_SECURE` | Add the Secure flag to dashboard cookies; enable behind HTTPS | unset |
| `HOMELAB_POLL_INTERVAL` | Poll frequency in seconds | `5` |
| `DISCORD_WEBHOOK_URL` | Discord alert webhook | unset |
| `DISCORD_ALERT_THRESHOLD` | Consecutive polls before an alert state changes | `2` |
| `HOMELAB_CPU_THRESHOLD` | CPU alert percentage | `90` |
| `HOMELAB_MEM_THRESHOLD` | Memory alert percentage | `90` |
| `HOMELAB_DISK_THRESHOLD` | Disk alert percentage | `90` |

Example environment files and systemd units live in `deploy/`.

## Test

```bash
go test ./...
go test -race ./...
```

## Current scope

The monitoring stack is ready for a small personal fleet. Before calling a
deployment complete, run the EC2/Tailscale guide, add the real agents, and
verify polling and dashboard access from a second tailnet device. Future
work should focus on operational polish such as automated build releases,
database backups, and dashboard usability—not remote host control.
