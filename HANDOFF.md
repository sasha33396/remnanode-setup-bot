# Development handoff

This file contains safe, version-controlled context for continuing development
on another device. It must never contain tokens, passwords, private keys, or
other secret values.

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

Certificate Manager and certificate issuance/renewal remain outside this stage.

## Current stage

### PROMPT 8 — Deployment Orchestrator

Current branch:

```text
feature/deployment-orchestrator
```

PROMPT 7 was merged through PR #4 (`8d80962`). PROMPT 8 adds:

- `DeploymentService` with persisted transitions for `PREFLIGHT`,
  `PREPARING_CERTIFICATE`, `PROVISIONING`, `CREATING_REMNAWAVE_NODE`,
  `WAITING_REMNAWAVE`, `ADDING_TO_DNS`, and `COMPLETED`.
- Host validation and repeated Node name/address uniqueness checks before side
  effects.
- Certificate acquisition behind `CertificateProvider`, with a temporary static
  in-memory implementation.
- SSH/provisioner adapter using the existing preflight, deployment-key bootstrap,
  idempotent provisioning engine, and external-certificate xray-sni adapter.
- Remnawave Node creation only after provisioning succeeds; generated
  `SECRET_KEY`, port `2222`, Host-derived inbound/profile, immediate UUID
  persistence, and timeout/backoff connection polling.
- DNS update only after Remnawave reports the Node connected. DNS failures keep
  the healthy Node, persist `DNS_FAILED`, and support a DNS-only retry.
- In-process bounded deployment concurrency, per-deployment duplicate-run
  protection, resumability, and context cancellation.
- A Telegram `Application` adapter that keeps the Telegram package dependent on
  interfaces rather than SSH/API implementations.
- Integration-style fake tests for full success, provisioning and Remnawave
  failures, connection failure/timeout, DNS failure/retry, duplicate invocation,
  concurrency limits, and cancellation.

Runtime dependency construction and Telegram polling startup in `cmd/deployer`
remain outside this stage.

## Local checks

PowerShell environments with restricted global Go caches can use:

```powershell
$env:GOCACHE = Join-Path (Get-Location) '.gocache'
$env:GOMODCACHE = Join-Path (Get-Location) '.gomodcache'
$env:GOPATH = Join-Path (Get-Location) '.gopath'

go test -count=1 ./...
go vet ./...
gofmt -l cmd internal migrations
git diff --check
```

Run the PostgreSQL integration test with a disposable database:

```powershell
$env:TEST_DATABASE_URL = 'postgres://user:password@localhost:5432/testdb?sslmode=disable'
go test -count=1 -v ./internal/repository/postgres
```

The integration test creates and removes an isolated schema. Do not point it at
a database where schema creation is prohibited.

## Remaining work after PROMPT 8

- Validate SSH and every provisioning stage against a disposable Ubuntu/Debian
  VPS before production use. Specifically smoke-test Docker builds on supported
  architectures, tag checkout, Caddy validation/reload, file ownership and
  modes, local TLS SNI routing, `/health` status 200, and failure recovery.
- Implement the centralized Certificate Manager and pass its in-memory
  certificate material through `CertificateProvider`.
- Construct the repository, Remnawave, DNS, certificate, SSH/provisioner,
  orchestrator, and Telegram dependencies in `cmd/deployer`, then start the Bot
  API polling transport.
- Add startup recovery for resumable non-terminal deployments if automated
  recovery is desired; the service can already resume them when invoked.
