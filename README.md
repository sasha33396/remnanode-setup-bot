# Remnanode Setup Bot

Production-oriented Go service for deploying Remnawave nodes through a Telegram
operator workflow. Configuration, persistence, external API clients, SSH
preflight, the idempotent VPS provisioner, and the Telegram presentation/state
layer are implemented. The deployment orchestration layer connects those
components through interfaces and persists every important workflow transition.
The executable wires PostgreSQL, certificate management, recovery, Telegram,
health checks, and metrics into one runtime.

## Requirements

- Go 1.24 or newer for local development
- Docker Engine with Docker Compose for container startup

## Configuration

Copy `.env.example` to `.env` and replace every placeholder. The deployment SSH
private key is supplied as a read-only Compose secret; set
`DEPLOY_SSH_PRIVATE_KEY` to its path on the Docker host. Never commit `.env`
or the `secrets/` directory.

`TELEGRAM_ALLOWED_USERS` accepts comma-, semicolon-, or space-separated positive
Telegram user IDs. `REMNA_API_IP` and `METRICS_IP` must be literal IPv4 or IPv6
addresses. API endpoints require HTTP(S) URLs, and `DATABASE_URL` requires a
PostgreSQL URL.

`XRAY_SNI_REPO_URL` defaults to the external-certificate fork and must be a
credential-free HTTPS URL. `XRAY_SNI_REF` defaults to the pinned
`v0.1.0-external` tag; branch names such as `main` are rejected.

`ACME_EMAIL` is the ACME account contact. `CF_API_TOKEN` is used only by the
central DNS-01 client and is never copied to a Node. Certificate versions and
the ACME account key are stored under `CERTIFICATE_STORE_PATH`.

The optional `HEALTH_ADDR` environment variable controls the local HTTP bind
address and defaults to `:8080`. Docker Compose sets it automatically.
For Compose, `DATABASE_URL` must use `postgres` as the database hostname.

## Run locally

Export all variables listed in `.env.example`, plus a PostgreSQL connection URL
in `DATABASE_URL` and the deployment key path in `DEPLOY_SSH_PRIVATE_KEY`, then:

```sh
go run ./cmd/deployer
```

Migrations `000001` through `000004` must be applied first. Startup verifies
PostgreSQL and the required schema before starting Telegram.

## Telegram operator UI

The `internal/telegram` package provides a Bot API long-polling transport and an
authorized, expiring Add Node wizard. It exposes application interfaces for
Hosts, duplicate checks, preflight, deployment progress, Nodes, and deployment
history. It never imports or directly invokes SSH or concrete API clients.

Temporary root passwords are deleted from Telegram when possible, retained only
in clearable in-memory wizard state, and never placed in callback data. The
orchestration package supplies the Telegram `Application` adapter. The current
executable starts authorized Telegram long polling and graceful shutdown.

## Deployment orchestration

`internal/orchestrator` implements the resumable deployment workflow from
preflight through certificate preparation, VPS provisioning, Remnawave Node
creation and connection polling, DNS update, and completion. Remnawave Node
creation is gated on successful provisioning, and DNS is gated on a connected
Node.

Deployments are bounded by an in-process concurrency limit and honor context
cancellation. A DNS failure preserves the healthy Remnawave Node, records
`DNS_FAILED`, and can be resumed with the DNS-only retry operation. Certificate
material is obtained only through the centralized Certificate Manager.

The manager enforces one active certificate per normalized SNI, obtains and
renews certificates with ACME DNS-01 through Cloudflare, stores protected
immutable versions, and distributes renewed material to DNS-configured Nodes.
Node activation follows the pinned xray-sni external contract and rolls back on
reload or TLS health failure.

## Database migrations

Apply the migrations before using deployment persistence. With `psql` available:

```sh
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000001_deployments.up.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000002_deployment_ssh_host_key.up.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000003_certificate_manager.up.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000004_production_recovery.up.sql
```

The down migration is provided for controlled rollback. It drops deployment
tables and must only be run intentionally:

```sh
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000004_production_recovery.down.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000003_certificate_manager.down.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000002_deployment_ssh_host_key.down.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000001_deployments.down.sql
```

## Run with Docker Compose

After creating `.env` and the SSH key file referenced by it:

```sh
docker compose up --build
```

The Compose stack persists PostgreSQL in `postgres_data` and protected
certificate versions/account key in `certificate_store`. The deployer runs as
root so a root-owned `0600` file-backed Compose secret works without host UID
coordination. Its root filesystem remains read-only; all capabilities are
dropped except `DAC_OVERRIDE` and `FOWNER`, which also permit reuse of volumes
created by the earlier non-root image. Health and metrics are bound to localhost
on `HEALTH_PORT`.

## Health probes

- `GET /healthz` — process liveness
- `GET /readyz` — PostgreSQL/schema readiness and graceful-shutdown state
- `GET /metrics` — Prometheus-compatible operational metrics

Example:

```sh
curl http://127.0.0.1:8080/readyz
```

## Development checks

```sh
go fmt ./...
go test ./...
go vet ./...
```

Repository integration tests require a reachable disposable PostgreSQL database.
They create and remove an isolated schema inside it:

```sh
TEST_DATABASE_URL="postgres://user:password@localhost:5432/testdb?sslmode=disable" \
  go test -v ./internal/repository/postgres
```

Without `TEST_DATABASE_URL`, the PostgreSQL integration test is skipped while
unit tests still run.

The service handles `SIGINT` and `SIGTERM`, marks itself unready, and gracefully
stops its HTTP server before exiting.

See [Production operations](docs/OPERATIONS.md) for installation, configuration,
backup, disaster recovery, upgrades, troubleshooting, the readiness checklist,
and explicitly documented remaining risks.
