# Running as a service

Runs the Agent and/or Backend under systemd so they start on boot and
restart automatically if they crash, instead of needing a terminal left
open with `go run`.

## Linux (systemd)

1. Build the binaries you need:

   ```bash
   go build -o /usr/local/bin/homelab-agent ./cmd/agent
   # Backend also needs its ./web directory alongside the binary:
   sudo mkdir -p /opt/homelab-monitor
   go build -o /opt/homelab-monitor/homelab-backend ./cmd/backend
   sudo cp -r web /opt/homelab-monitor/web
   ```

2. Create a dedicated user (no login shell, no home directory needed):

   ```bash
   sudo useradd --system --no-create-home --shell /usr/sbin/nologin homelab
   ```

3. Set up the env file(s). Only copy the one(s) for the service you're
   running:

   ```bash
   sudo mkdir -p /etc/homelab-monitor
   sudo cp deploy/agent.env.example /etc/homelab-monitor/agent.env      # if running the Agent
   sudo cp deploy/backend.env.example /etc/homelab-monitor/backend.env  # if running the Backend
   sudo chmod 600 /etc/homelab-monitor/*.env
   sudo chown homelab:homelab /etc/homelab-monitor/*.env
   ```

   Edit `backend.env` and set `HOMELAB_PASSWORD_HASH` (generate one with
   `go run ./cmd/hashpw`) - the backend won't start without it. Fill in
   `HOMELAB_AGENTS` too, or it defaults to `localhost:8080,localhost:8081`.

4. Install and start the unit(s):

   ```bash
   sudo cp deploy/homelab-agent.service deploy/homelab-backend.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now homelab-agent    # if running the Agent
   sudo systemctl enable --now homelab-backend  # if running the Backend
   ```

5. Check on it:

   ```bash
   systemctl status homelab-agent
   journalctl -u homelab-agent -f
   ```

The units run as the unprivileged `homelab` user with `ProtectSystem=strict`
and `NoNewPrivileges=yes`, and restart on failure after 5s. If a future
change makes the Backend write to disk (e.g. the planned SQL-backed
dashboard), its unit will need `ReadWritePaths=` added for wherever that
data lives - `ProtectSystem=strict` makes everything else read-only.

## Windows

Go binaries run fine on Windows, but Windows has no native systemd
equivalent for "restart on crash, start on boot." The simplest option is
[NSSM](https://nssm.cc/) (Non-Sucking Service Manager):

```powershell
nssm install HomelabAgent C:\path\to\homelab-agent.exe
nssm set HomelabAgent AppEnvironmentExtra AGENT_PORT=8080
nssm start HomelabAgent
```

`sc.exe create` works too but doesn't manage environment variables or
crash-restart as conveniently - NSSM is the more common recommendation for
wrapping a plain Go binary as a Windows service.
