# Development handoff

This file contains safe, version-controlled context for continuing development
on another device. It must never contain tokens, passwords, private keys, or
other secret values.

## Current active work — 2026-08-18 (authoritative)

This section supersedes older `Current stage` sections retained below as
historical context.

- Active branch: `codex/live-deployment-progress`
- Remote branch: `origin/codex/live-deployment-progress`
- Feature tip before the Host-move work: `b498d4c`

Continue the current work with:

```sh
git fetch origin
git switch codex/live-deployment-progress
git pull --ff-only
```

### Current Telegram Node operations

- **Ноды** first shows one button per Remnawave panel; Nodes from different
  panels are never mixed in one list.
- Each panel separates Nodes into critically low online, disabled, and
  active/stable groups. Group buttons display live counts and each Node opens
  an operator-safe card.
- A connected, enabled Node is critical when `usersOnline <= 50`. This is a
  fixed configurable threshold; panel median and Node count are not used.
- Disabled Nodes are medium priority. Disconnected/connecting Nodes and Nodes
  without a fresh metric are excluded from low-online alerts because connection
  loss is monitored separately.
- Live online counts come from Remnawave `GET /api/system/nodes/metrics` and are
  joined to `/api/nodes` by UUID. A temporary metrics failure does not make the
  inventory unavailable; affected Nodes are excluded until metrics recover.
- Background checks run every 5 minutes. Two consecutive critical samples are
  required. While critical, alerts repeat every 15 minutes to every ID in
  `TELEGRAM_ALLOWED_USERS`; recovery generates a separate message.
- Temporary connection/metrics gaps retain incident state and do not cause a
  duplicate alert after reconnection.
- Critical alerts and manually opened cards include **Изменить IP ноды**. It
  resolves the selected Node and panel again and verifies the UUID before any
  workflow starts.
- The selected-Node IP menu exposes every available main-menu method:
  **Панель + DNS-балансировка**, **Cherry (сервер)**, and **Royal (сервер)**.
  Managed and legacy Nodes use the same safe resolution path.
- Cherry/Royal paths reuse the selected Node's current public IPv4 as the SSH
  target, request the new provider-specific IP and transient root password,
  configure the server, then update that same Remnawave Node and DNS zones.
- Managed and legacy Node cards, including cards opened from critical alerts,
  expose **Переместить между Host**. The picker contains only enabled Hosts with
  a complete profile/inbound mapping from the Node's current panel.
- A Host move changes only the Node's active config profile and inbound through
  `PATCH /api/nodes`; its address is left untouched. UUIDs stay in transient
  server-side state. The Node and Host are fetched again at confirmation and a
  stale profile fingerprint aborts the operation safely.

Relevant recent commits:

```text
8837950 feat: offer hoster IP workflows from node alerts
e7ac3c5 fix: use fixed critical node online threshold
580b147 feat: start node IP changes from health cards
4165aa0 fix: repeat critical node alerts every 15 minutes
a8d7c62 feat: add panel-aware node health monitoring
64aa40e fix: always report DNS synchronization results
```

### Node-monitor environment

The server `.env` is ignored by Git and is not changed by `git pull`. Keep these
explicit values in the server `.env`:

```env
NODE_MONITOR_INTERVAL=5m
NODE_CRITICAL_ALERT_INTERVAL=15m
NODE_MONITOR_CONFIRMATIONS=2
NODE_CRITICAL_ONLINE_THRESHOLD=50
```

The obsolete `NODE_CRITICAL_ONLINE_FLOOR`,
`NODE_CRITICAL_ONLINE_RATIO`, and `NODE_CRITICAL_ONLINE_CAP` variables are no
longer read and may be removed.

After pulling on the server:

```sh
docker compose up -d --build
```

The full Go test suite and `go vet ./...` passed after the selected-Node hoster
workflow was added. Re-run both before further commits.

## Repository workflow

- Repository: `sasha33396/remnanode-setup-bot`
- Stable branch: `main`
- Each development prompt uses a new `feature/*` branch created from the latest
  `origin/main`.
- Merge the current feature through a pull request before starting the next
  prompt.
- If prompts are supplied out of sequence, stop and confirm the missing
  prerequisite first.
- `PROJECT.md` is intentionally local and ignored by Git. This document is the
  committed cross-device handoff.

To continue after cloning:

```sh
git fetch origin
git switch main
git pull --ff-only
```

For an existing unmerged feature, switch to its remote branch. For a new prompt,
create a new feature only after the preceding pull request has been merged.

## Project purpose

The application is a single Go service controlled through Telegram. It will
deploy VLESS Reality Remnawave Nodes to fresh Ubuntu/Debian VPS servers and then
add connected Nodes to `remnawave-cloudflare-nodes` DNS zones.

Business-critical rules:

- Node names are entered manually by the operator.
- `SNI_DOMAIN` is `host.address`; never use `host.sni`.
- Node profile values come from the selected Host inbound:
  - `activeConfigProfileUuid = host.inbound.configProfileUuid`
  - `activeInbounds = [host.inbound.configProfileInboundUuid]`
- Remnanode `SECRET_KEY` comes from `GET /api/keygen` field
  `response.pubKey`.
- Provision the VPS before creating the Remnawave Node.
- Never update DNS until Remnawave reports `isConnected == true`.
- DNS zone updates are complete-list replacements and therefore require a
  serialized read-modify-write operation.
- VLESS Reality Remnanode does not mount TLS certificates.
- Certificate lifecycle is centralized by SNI domain; Nodes only consume
  certificates.

## Security invariants

- Never persist or log the temporary VPS root password.
- Never log Telegram, Remnawave, DNS-balancer, or Cloudflare tokens.
- Never log SSH or certificate private keys.
- Do not bypass SSH host-key verification.
- Use trust on first use for fresh VPS hosts and persist the fingerprint against
  the deployment. Reject changed fingerprints.
- Secrets come from environment variables, Docker secrets, or mounted secret
  files.
- Every external operation uses `context.Context`, explicit timeouts, and safe
  errors.

## Completed stages

### PROMPT 1 — Go bootstrap

- Go module and package skeleton.
- Environment configuration validation.
- JSON structured logging.
- Signal-driven graceful shutdown.
- `/healthz` and `/readyz` endpoints.
- Dockerfile, Docker Compose with PostgreSQL, `.env.example`, and README.

### PROMPT 2 — PostgreSQL deployment state

- Deployment and deployment-step models.
- PostgreSQL repository interface and pgx implementation.
- Migrations for deployments and steps.
- State, step, recent-deployment, and unfinished-deployment operations.
- Unit tests plus an integration test gated by `TEST_DATABASE_URL`.

### PROMPT 3 — Remnawave API client

- Typed client based on the supplied Remnawave OpenAPI contract.
- Hosts, key generation, Nodes, Node creation/status refresh, and delete.
- Duplicate Node name/IP protection.
- Strict Host-to-Node profile mapping.
- Fake HTTP server tests for normal, invalid, unauthorized, timeout, duplicate,
  and mapping cases.

### PROMPT 4 — DNS-balancer client

- Merged to `main` through pull request #1.
- Typed `remnawave-cloudflare-nodes` client using `X-API-Key`.
- Exact FQDN matching from actual domain/zone configuration.
- Complete-list PATCH with duplicate prevention.
- Per-FQDN `ZoneLocker` abstraction with in-memory keyed locking.
- Concurrency tests proving simultaneous additions do not lose updates.

### PROMPT 5 — SSH client and VPS preflight

- Merged to `main` through pull request #2.
- Password and deployment-key authentication, idempotent `authorized_keys`
  installation, PostgreSQL-backed TOFU host-key verification, bounded SSH
  commands, and typed VPS preflight.

### PROMPT 6 — Idempotent VPS Provisioner

- Merged to `main` through pull request #3.

- Stable stage engine for `system`, `docker`, `sysctl`, `limits`, `firewall`,
  `fail2ban`, `remnanode`, `node_exporter`, `speedtest_exporter`, `logrotate`,
  `xray_sni`, and `healthcheck`.
- Persistent `RUNNING`/`COMPLETED`/`FAILED` progress and restart-safe resume
  using `ListDeploymentSteps`; completed stages are not repeated.
- Atomic managed-file updates over SSH. File contents, including Remnanode
  `SECRET_KEY`, travel through stdin and never appear in the command string.
- Managed sysctl, limits, systemd, fail2ban, Docker Compose, and logrotate files
  replace only deployer-owned files and do not append duplicate entries.
- UFW rules have stable comments and are added only when missing. Panel access
  to `2222` and metrics access to `9100`/`9200` come from typed IP config.
- Remnanode runs with host networking and `NODE_PORT=2222`; its only volume is
  the log directory, with no certificate mounts.
- Node Exporter downloads are checked against the release checksum manifest.
- The real external-certificate xray-sni adapter uses
  `https://github.com/sasha33396/sni-external`. `XRAY_SNI_REPO_URL` is
  configurable and defaults to that URL. `XRAY_SNI_REF` defaults to the pinned
  `v0.1.0-external` tag; `main`, `master`, and `HEAD` are rejected.
- The pinned tag exists and currently resolves to commit `7702d9f025fb`. The
  adapter clones without checkout, fetches only the configured tag/commit, and
  deploys a detached revision rather than an implicit branch HEAD.
- `.deployed-commit` is written atomically only after a successful Compose
  build. If checkout succeeds but the build fails, resume detects the missing
  marker and retries the pinned build instead of accepting an older container.
- xray-sni is installed in `/opt/xray-sni` as Compose service `caddy`, container
  `snisite`, with host networking.
- The adapter writes only `TLS_MODE=external`, `SNI_DOMAIN=host.address`, and
  `SNI_PORT=9443` to `/opt/xray-sni/.env`. It never writes `CF_API_TOKEN` or
  performs ACME, Let's Encrypt, Cloudflare, or DNS-01 operations.
- Centrally supplied certificate material is validated in Go before upload:
  PEM/leaf parsing, key parsing, public-key match, SAN hostname, NotBefore, and
  NotAfter. No caller-side filesystem paths are required.
- Host certificate paths are `/opt/xray-sni/certs/fullchain.pem` and
  `/opt/xray-sni/certs/privkey.pem`; container paths are
  `/certs/fullchain.pem` and `/certs/privkey.pem`. The directory is mounted as
  `./certs:/certs:ro` by the fork.
- Certificate files are uploaded as temporary files through SSH stdin and then
  activated with rollback copies retained until validation succeeds. Modes are
  `0644` for fullchain and `0600` for the private key, owned by `root:root`.
- Certificate-only changes use
  `docker exec snisite caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile`.
  Validation includes Compose config, running-container state, Caddy config,
  and a local TLS request to `https://<SNI_DOMAIN>:9443/health` with expected
  HTTP status `200` and explicit SNI resolution to `127.0.0.1`.
- Remnanode remains VLESS Reality and receives no Caddy certificates, `/certs`
  mounts, or Cloudflare credentials.
- Unit tests cover stage order, resume, second-run idempotency, failure
  propagation, safe persistence, Remnanode invariants, certificate validation,
  pinned Git/ref behavior, runtime failures, rollback safety, and secret
  transport.

The centralized Certificate Manager and production recovery layer are now part
of the runtime.

## Current stage

### PROMPT 9–10 — Certificate Manager and production hardening

Current branch:

```text
feature/certificate-manager-hardening
```

PROMPT 8 was merged through PR #5 (`e257ea6`). This branch adds:

- One protected, versioned central certificate store per normalized SNI with
  PostgreSQL fingerprint, serial, issuance/expiry, renewal, status, active
  version, and per-Node distribution metadata.
- ACME RFC 8555 DNS-01 issuance using a central Cloudflare client and protected
  persisted ACME account key. Cloudflare credentials never reach Nodes.
- In-process and PostgreSQL advisory locking prevents concurrent issuance for
  the same SNI.
- Renewal scheduling, expiry metrics, DNS-config-derived Node targets, bounded
  SSH distribution, and central activation only after complete distribution.
- Finalized xray-sni certificate-only activation: temporary uploads through SSH
  stdin, atomic file switch, Caddy reload, local TLS health validation, and
  automatic Node rollback.
- Startup classification of unfinished deployments without blind external
  retries; exact Remnawave/DNS inspection and `MANUAL_REVIEW` for ambiguity.
- Safe operator recovery commands for retry step, retry DNS, Remnawave recheck,
  cancellation, and persisted safe logs.
- Runtime dependency wiring in `cmd/deployer`, graceful shutdown, dependency
  readiness, Prometheus metrics, secret-redacting structured logging, and
  hardened Docker/Compose settings.
- The deployer container intentionally runs as UID 0 because local Compose
  file-backed secrets retain host ownership. The root filesystem is read-only;
  capabilities are dropped except `DAC_OVERRIDE` and `FOWNER` for protected
  secret/certificate-volume access.
- Tests cover cache hit, initial/concurrent issuance, renewal, invalid and
  mismatched material, partial distribution, rollback, recovery, metrics, and
  redaction in addition to the existing deployment suite.

## Local checks

PowerShell environments with restricted global Go caches can use:

```powershell
$env:GOCACHE = Join-Path (Get-Location) '.gocache'
$env:GOMODCACHE = Join-Path (Get-Location) '.gomodcache'
$env:GOPATH = Join-Path (Get-Location) '.gopath'

go test -count=1 ./...
go vet ./...
# Run gofmt -l on the Go files changed by the current branch.
git diff --check
```

Run the PostgreSQL integration test with a disposable database:

```powershell
$env:TEST_DATABASE_URL = 'postgres://user:password@localhost:5432/testdb?sslmode=disable'
go test -count=1 -v ./internal/repository/postgres
```

The integration test creates and removes an isolated schema. Do not point it at
a database where schema creation is prohibited.

## Remaining work after PROMPT 10

- Validate SSH and every provisioning stage against a disposable Ubuntu/Debian
  VPS before production use. Specifically smoke-test Docker builds on supported
  architectures, tag checkout, Caddy validation/reload, file ownership and
  modes, local TLS SNI routing, `/health` status 200, and failure recovery.
- Complete a real Cloudflare staging ACME issuance/renewal and disposable VPS
  distribution/rollback smoke test.
- Complete an encrypted PostgreSQL + certificate-store backup/restore exercise.
- Add production monitoring dashboards and alert rules.
- Run only one deployer replica until deployment execution locking is moved from
  process memory to a cross-instance primitive. Certificate issuance already
  uses PostgreSQL locking.
- Review the full checklist and remaining risks in `docs/OPERATIONS.md` before
  production use.
