# Remnanode Setup Bot

Production-oriented Go service for deploying Remnawave nodes through a Telegram
operator workflow. Configuration, persistence, external API clients, SSH
preflight, the idempotent VPS provisioner, and the Telegram presentation/state
layer are implemented; end-to-end deployment orchestration is intentionally not
wired yet.

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

The optional `HEALTH_ADDR` environment variable controls the local HTTP bind
address and defaults to `:8080`. Docker Compose sets it automatically.
For Compose, `DATABASE_URL` must use `postgres` as the database hostname.

## Run locally

Export all variables listed in `.env.example`, plus a PostgreSQL connection URL
in `DATABASE_URL` and the deployment key path in `DEPLOY_SSH_PRIVATE_KEY`, then:

```sh
go run ./cmd/deployer
```

The bootstrap validates the database URL but does not connect to PostgreSQL yet.

## Telegram operator UI

The `internal/telegram` package provides a Bot API long-polling transport and an
authorized, expiring Add Node wizard. It exposes application interfaces for
Hosts, duplicate checks, preflight, deployment progress, Nodes, and deployment
history. It never imports or directly invokes SSH or concrete API clients.

Temporary root passwords are deleted from Telegram when possible, retained only
in clearable in-memory wizard state, and never placed in callback data. The
current executable does not construct the application implementation or start
the Telegram polling loop; that remains part of end-to-end orchestration.

## Database migrations

Apply the migrations before using deployment persistence. With `psql` available:

```sh
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000001_deployments.up.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000002_deployment_ssh_host_key.up.sql
```

The down migration is provided for controlled rollback. It drops deployment
tables and must only be run intentionally:

```sh
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000002_deployment_ssh_host_key.down.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000001_deployments.down.sql
```

## Run with Docker Compose

After creating `.env` and the SSH key file referenced by it:

```sh
docker compose up --build
```

The Compose stack starts the deployer and PostgreSQL, waits for PostgreSQL's
healthcheck, and persists database data in the `postgres_data` volume. The health
endpoint is bound to localhost on `HEALTH_PORT` (port `8080` by default).

## Health probes

- `GET /healthz` — process liveness
- `GET /readyz` — readiness; returns success after the HTTP listener starts and
  switches to unavailable during graceful shutdown

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
