# EC2 backend with Tailscale

This is the recommended production layout: agents run on the machines being
monitored, and one backend runs on EC2. Tailscale carries both the backend's
polls to agents and your private dashboard traffic. Do not create inbound
EC2 rules for ports 22, 80, 443, 8080, or 9090.

## 1. Launch the instance

Use Amazon Linux 2023, a `t3.small` (or comparable Graviton instance if you
build an arm64 binary), 20 GB encrypted gp3 EBS, and a security group with
**no inbound rules**. The instance needs outbound access to install updates,
connect to AWS Systems Manager, and join Tailscale. A public subnet with a
public IPv4 and no ingress is the simplest setup; a private subnet also works
when it has NAT or the necessary VPC endpoints.

Attach an instance profile with `AmazonSSMManagedInstanceCore`, require
IMDSv2, and use AWS Systems Manager Session Manager for administration. Do
not add an SSH key or port-22 rule. For a long-lived backend, enable EBS
snapshots or AWS Backup: the database and fleet configuration are local
state.

## 2. Join the tailnet and restrict it

Install Tailscale on EC2 and every monitored host. On Amazon Linux:

```bash
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up --hostname=homelab-backend
```

For unattended installation, use a short-lived, tagged, reusable auth key
from the Tailscale admin console instead of putting a personal login in a
bootstrap script. Give the EC2 node a backend tag and each monitored node an
agent tag. The following ACL is a minimal pattern; replace the group member
with your own Tailscale identity and adapt existing policy rather than
overwriting it.

```json
{
  "groups": {
    "group:homelab-admin": ["you@example.com"]
  },
  "tagOwners": {
    "tag:homelab-backend": ["group:homelab-admin"],
    "tag:homelab-agent": ["group:homelab-admin"]
  },
  "acls": [
    {
      "action": "accept",
      "src": ["tag:homelab-backend"],
      "dst": ["tag:homelab-agent:8080"]
    },
    {
      "action": "accept",
      "src": ["group:homelab-admin"],
      "dst": ["tag:homelab-backend:443"]
    }
  ]
}
```

Use MagicDNS names such as `nas:8080` for backend agent addresses. They are
stable within the tailnet and avoid hard-coding Tailscale IP addresses.

## 3. Install the backend

First publish or transfer the cleaned source from this workspace. The EC2
instance must not clone the older GitHub revision while the changes in this
workspace are still uncommitted. Once the updated revision is available,
open a Session Manager shell and build it there. The commands below assume
the updated revision has been pushed to the project's GitHub repository.

```bash
sudo dnf install -y git golang
git clone https://github.com/gagewilson814/homelab-monitor.git
cd homelab-monitor
go version
go build -trimpath -o homelab-backend ./cmd/backend

sudo useradd --system --no-create-home --shell /usr/sbin/nologin homelab
sudo install -d -o homelab -g homelab /opt/homelab-monitor/data
sudo install -m 0755 homelab-backend /opt/homelab-monitor/homelab-backend
sudo cp -r web /opt/homelab-monitor/web
sudo chown -R root:root /opt/homelab-monitor/web
```

If the distro `golang` package is older than the version in `go.mod`, install
the matching Go release first, or build a Linux binary in CI/local development
and deliver it through a controlled artifact path.

Install the supplied service and environment file:

```bash
sudo install -d -m 0750 -o root -g homelab /etc/homelab-monitor
sudo install -m 0640 -o root -g homelab deploy/backend.env.example /etc/homelab-monitor/backend.env
sudo cp deploy/homelab-backend.service /etc/systemd/system/
sudo systemctl daemon-reload
```

Edit `/etc/homelab-monitor/backend.env` with these production values:

```ini
HOMELAB_PORT=127.0.0.1:9090
HOMELAB_DB_FILE=/opt/homelab-monitor/data/homelab.db
HOMELAB_AGENTS_FILE=/opt/homelab-monitor/data/agents.json
HOMELAB_COOKIE_SECURE=1
HOMELAB_AGENT_TOKEN=replace-with-a-long-random-secret
HOMELAB_AGENTS=nas:8080,media-server:8080
DISCORD_WEBHOOK_URL=replace-with-your-private-discord-webhook
```

Use `sudoedit /etc/homelab-monitor/backend.env` to set those values. Keep the
webhook URL and agent token private. Seed the first user against the same
database path as the service, then enable the backend:

```bash
sudo -u homelab env HOMELAB_DB_FILE=/opt/homelab-monitor/data/homelab.db /opt/homelab-monitor/homelab-backend seed
sudo systemctl enable --now homelab-backend
sudo systemctl status homelab-backend
```

## 4. Publish the dashboard privately

Tailscale Serve terminates HTTPS inside the tailnet and proxies to the
loopback-only backend. It does not make the dashboard public (do not use
Tailscale Funnel here).

```bash
sudo tailscale serve --bg --https=443 http://127.0.0.1:9090
sudo tailscale serve status
```

Open the HTTPS URL printed by `tailscale serve status` from a device allowed
by the tailnet policy. The Secure session cookie now works because the
browser is using HTTPS, even though the backend is only HTTP on loopback.

## 5. Install each agent

Build the matching agent binary for each host architecture, install
`deploy/homelab-agent.service`, and create `/etc/homelab-monitor/agent.env`.
At minimum set the same token used by the backend:

```ini
HOMELAB_AGENT_TOKEN=replace-with-the-same-long-random-secret
HOMELAB_SERVICES=jellyfin:8096,plex:32400
```

Join the machine to Tailscale with the `tag:homelab-agent` tag, then enable
the service. Confirm from EC2 that `curl` to each `http://name:8080/stats`
returns `401` without the token and succeeds with it. Finally confirm the
dashboard shows the agent online.

## 6. Verify Discord alerts

In Discord, open your chosen channel's settings, create a webhook under
**Integrations → Webhooks**, and copy its URL. Put that URL in
`DISCORD_WEBHOOK_URL` in `/etc/homelab-monitor/backend.env`; do not put it in
the repository or share it in screenshots. Restart the backend after changing
the environment file:

```bash
sudo systemctl restart homelab-backend
sudo journalctl -u homelab-backend -n 30 --no-pager
```

The log should report that Discord alerts are enabled. Once an agent is
online in the dashboard, deliberately stop that agent with
`sudo systemctl stop homelab-agent`. After the configured number of failed
polls (two by default), Discord should receive an offline alert. Start the
agent again and confirm the recovery alert:

```bash
sudo systemctl start homelab-agent
```

## Verification and recovery

- `tailscale status` shows the backend and every agent online.
- `sudo systemctl status homelab-backend` is healthy and `journalctl -u
  homelab-backend -f` shows successful polls.
- The dashboard is reachable only at its `https://*.ts.net` Serve URL from
  an authorized tailnet device.
- Restart the backend service and confirm the account, agent list, and
  dashboard session behavior are as expected.
- Snapshot `/opt/homelab-monitor/data` or its EBS volume before upgrades.

References: [Tailscale Serve](https://tailscale.com/docs/features/tailscale-serve),
[Tailscale on AWS](https://tailscale.com/docs/install/cloud/aws/quickstart),
and [AWS Session Manager](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager.html).
