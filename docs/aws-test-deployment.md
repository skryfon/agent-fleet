# AWS Setup Guide — AgentFleet Test Deployment

Target: single EC2 instance running the full Podman Compose stack (control-plane, supervisor, bridge, Postgres, Zulip, webapp, egress-proxy) per `development-plan.md`.

## 1. Account & billing prep

1. Sign up / confirm AWS account is post-2025-07-15 (credit-based free plan).
2. Complete the 5 onboarding tasks to unlock the full $200 credit (Billing console → Free Tier / Credits page shows the checklist).
3. **Set a budget alert immediately**: Billing → Budgets → create a $20/$50 threshold alert. This is the one manual step people skip and then get surprised.
4. Note account is auto-closed at 6 months or credit exhaustion — put a calendar reminder to terminate/export before then.

## 2. Choose OS and instance

- **AMI: Ubuntu Server 24.04 LTS (x86_64)**. Reasons: kernel ≥5.11 with cgroups v2 by default (required for rootless Podman), `apt` has current Podman packages, most Podman/dsh docs assume Debian/Ubuntu.
  - Avoid Amazon Linux 2023 unless you're comfortable with dnf/SELinux quirks — AL2023 also supports Podman but has less community documentation for rootless setups.
  - Avoid ARM/Graviton (t4g) — your Go binaries and any prebuilt runner images would need ARM builds; not worth it for a test box.
- **Instance type: `t3.medium`** (2 vCPU, 4GB) to start. If you see the runner containers or Postgres getting OOM-killed, resize to `t3.large` (2 vCPU, 8GB) — EC2 lets you stop and change instance type without losing the EBS volume.
- **Storage: 30–40GB gp3 EBS** (default 8GB is too small once you pull Postgres/Zulip images + build runner images). gp3 baseline throughput is enough for a test DB.

## 3. Launch — networking config

Launch wizard → **Network settings**:

- **VPC**: use the default VPC unless you already have one — no need to build custom subnets for a single-instance test.
- **Subnet**: any public subnet (auto-assign public IP: **enabled**), so you can SSH in without a bastion.
- **Security group** — create one scoped narrowly, don't use `0.0.0.0/0` broadly:

| Port | Protocol | Source | Purpose |
|---|---|---|---|
| 22 | TCP | your IP only (`x.x.x.x/32`) | SSH |
| 443 | TCP | your IP only, or `0.0.0.0/0` if you want to demo Zulip/webapp from elsewhere | Caddy HTTPS (webapp + Zulip reverse-proxied) |
| 80 | TCP | same as 443 | Caddy HTTP→HTTPS redirect / ACME challenge |

Do **not** open Postgres (5432), the control-plane API, or any internal ports to the internet — those stay behind Caddy or are only reachable from inside the instance's Podman networks, matching the repo's `runners` network being `internal: true`. AWS security groups govern the instance's *external* boundary; the Compose-internal network isolation (runners can't reach Postgres/Zulip directly) is enforced separately, on the box, by Podman's network config — the two layers are independent, keep both.

- **Elastic IP**: allocate and associate one if you want a stable address (e.g. for Zulip webhook callbacks or GitHub App callback URLs during testing) — otherwise the public IP changes on stop/start.
- **Key pair**: create a new one, download the `.pem`, `chmod 400` it locally.

## 4. First login and OS hardening

```bash
ssh -i agentfleet-test.pem ubuntu@<elastic-ip>
sudo apt update && sudo apt full-upgrade -y
sudo reboot
```

After reboot:

```bash
sudo apt install -y ufw
sudo ufw allow 22/tcp
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw enable
```

This gives you a second firewall layer on the OS itself, independent of the AWS security group — belt and suspenders, cheap insurance if the SG is ever loosened by mistake.

## 5. Install rootless Podman

```bash
sudo apt install -y podman podman-compose uidmap slirp4netns fuse-overlayfs
```

Rootless Podman needs subuid/subgid ranges for the `ubuntu` user (usually pre-populated by `adduser` on 24.04, but verify):

```bash
grep ubuntu /etc/subuid /etc/subgid
# if empty:
sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 ubuntu
```

Enable lingering so rootless containers keep running after you log out of SSH:

```bash
sudo loginctl enable-linger ubuntu
```

Verify cgroups v2 is active (should already be true on Ubuntu 24.04):

```bash
cat /sys/fs/cgroup/cgroup.controllers
```

Confirm Podman itself works rootless:

```bash
podman run --rm hello-world
```

## 6. Get the repo and secrets onto the box

```bash
git clone <agent_fleet repo url> ~/agent_fleet
cd ~/agent_fleet
cp deploy/.env.example deploy/.env   # if it exists — check actual filename in deploy/
```

Fill in `.env` with: GitHub App credentials, Zulip bot token, OmniRoute/model API keys, Postgres password. **Never commit this file** — it should already be gitignored; double check.

## 7. Compose networking inside the box

The repo's `deploy/compose.yaml` defines the network topology (per CLAUDE.md: `runners` network is `internal: true` — no route to Postgres/Zulip/Podman socket from a runner container). You don't need to touch AWS for this — it's pure Podman network config on the instance. Just confirm after bring-up:

```bash
podman network ls
podman network inspect runners   # should show "internal": true
```

## 8. Caddy / TLS

If using a domain name for Zulip/webapp: point an A record at the Elastic IP, let Caddy in `deploy/caddy/` handle ACME/Let's Encrypt automatically — this needs port 80 open (already done in step 3) for the HTTP-01 challenge.

If you don't have a domain and just want IP-based testing, use self-signed certs or access services over SSH tunnel instead of opening 443 publicly:

```bash
ssh -i agentfleet-test.pem -L 8443:localhost:443 -L 8080:localhost:8080 ubuntu@<elastic-ip>
```

then browse `https://localhost:8443` locally — this avoids opening anything but 22 in the security group at all, the tightest option for a private test.

## 9. Bring up the stack

```bash
cd ~/agent_fleet/deploy
podman compose up -d
podman compose ps
podman compose logs -f control-plane
```

Run the composition smoke test from CLAUDE.md before trusting it:

```bash
node deepseek-harness/apps/cli/lib/bin.js --profile agentfleet-runner --dump-config
```

Every `af-*` row should appear, no Cordis fiber `PENDING`.

## 10. Cost & teardown discipline

- Stop the instance (`aws ec2 stop-instances` or console) when not actively testing — EBS storage still bills (~$0.08/GB-mo, ~$3/mo for 40GB) but compute stops.
- Terminate entirely before the 6-month free-plan window closes, or once you're done testing.
- Elastic IP is free while attached to a running instance, but bills (~$3.60/mo) if left allocated to a *stopped* instance — release it if you're pausing for a while.

---

Skipped: multi-AZ/HA, load balancer, RDS-managed Postgres — none needed for a single-box test deployment. Add when moving toward a real staging/prod environment.
