# Home Server Monitoring Dashboard

A lightweight, self-hosted monitoring and control system for a home server fleet. A small Go agent runs on each machine and reports live system stats; a central backend and web dashboard (planned) will aggregate that data and allow authenticated remote actions like restarting a server, all accessible from a phone.

> 🚧 **Status: Work in progress.** This project is being built incrementally as a hands-on learning exercise. See the Roadmap below for what's actually done vs. planned.

## Why

Most existing monitoring tools are either overkill for a home lab or don't support mixed Linux/Windows environments cleanly. This project is scoped specifically for personal infrastructure: simple to deploy, works across OSes, and reachable from a phone without exposing anything to the public internet. Also, I just wanted to build something that I can use.

## Architecture

- **Agent** (Go) — runs on each monitored machine. Exposes a `/stats` HTTP endpoint that returns hostname, CPU usage, memory usage, disk usage, and uptime as JSON. Uses [`gopsutil`](https://github.com/shirou/gopsutil) for cross-platform system metrics, and compiles to a single static binary for both Linux and Windows.
- **Backend** *(planned)* — a central service running on the home network that polls each Agent directly (pull model — no NAT/port-forwarding complexity since everything stays on the home network or a VPN) and exposes an aggregated API to the frontend.
- **Frontend** *(planned)* — a JavaScript web dashboard, backed by a SQL database, with user login, for visualizing the fleet and triggering remote actions from a phone.

```
[ Agent : Linux server ]  \
[ Agent : Linux server ]   >---(Backend polls each Agent)---> [ Backend API ] ---> [ Web Dashboard ]
[ Agent : Windows server]  /
```

## Tech Stack

| Layer     | Tech                                  |
|-----------|----------------------------------------|
| Agent     | Go, [gopsutil](https://github.com/shirou/gopsutil) |
| Backend   | Go (planned)                          |
| Frontend  | JavaScript + SQL database (planned)   |
| Auth      | TBD — required before any remote-restart capability ships |

## Getting Started (Agent)

Requires [Go](https://go.dev/dl/) installed.

Since this is in progress, currently only the server agent is runnable.

```bash
# run locally
go run agent.go

# build a binary for the current OS
go build -o agent

# cross-compile for Windows from Linux/macOS (or vice versa)
GOOS=windows GOARCH=amd64 go build -o agent.exe
```

Once running, the Agent serves stats at:

```
GET http://<agent-host>:8080/stats
```

## Roadmap

- [x] Walking-skeleton HTTP server with `/stats` endpoint
- [x] Real hostname, CPU, and memory metrics via `gopsutil`
- [x] Real disk usage metrics
- [x] Real uptime metrics
- [ ] Backend service that polls Agents across the home network
- [ ] Authentication for remote actions (non-negotiable before restart ships)
- [ ] Remote restart capability
- [ ] Web dashboard (JS + SQL + login)
- [ ] Mobile-friendly / installable (PWA) dashboard access

## License

TBD
