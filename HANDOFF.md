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

## Current stage

### PROMPT 5 — SSH client and VPS preflight

Current branch:

```text
feature/ssh-preflight
```

Implemented but not yet merged at the time this handoff was written:

- Initial password SSH authentication with a redacted, clearable in-memory
  credential type.
- Backend deployment-key authentication.
- Idempotent installation of the backend public key into root
  `authorized_keys`.
- TOFU host-key verification. First fingerprints are committed only after a
  successful SSH handshake; subsequent changes fail with `ErrHostKeyChanged`.
- PostgreSQL-backed fingerprint persistence through migration
  `000002_deployment_ssh_host_key` and the repository `HostKeyStore` adapter.
- Bounded SSH command execution with cancellation, timeout, exit status, and
  stdout/stderr limits.
- Typed VPS preflight for root identity, Ubuntu/Debian version, architecture,
  CPU, available RAM, free disk, Docker, existing Remnanode/xray-sni artifacts,
  and TCP ports `22`, `443`, `2222`, `9100`, `9200`, `9443`.
- Separate warning and fatal-failure collections.
- Unit tests for credential redaction, host-key verification, output bounds,
  parsing, and preflight policy.

Full provisioning and deployment orchestration are intentionally not part of
this stage.

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

## Remaining work after PROMPT 5

- Validate SSH behavior against a disposable real VPS.
- Wire repository, SSH, and preflight into deployment orchestration when a later
  prompt explicitly requests it.
- Implement provisioning stages only when requested.
- Implement Telegram flow only when requested.
- Preserve the persistent deployment state machine and safe error fields during
  future orchestration work.
