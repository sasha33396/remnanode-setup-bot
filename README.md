# Remnanode Setup Bot

Production-oriented Go service that will deploy Remnawave nodes through a
Telegram-controlled workflow. This bootstrap contains configuration, structured
logging, process lifecycle management, health probes, and package boundaries.
Deployment business logic is intentionally not implemented yet.

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

## Database migrations

Apply the migrations before using deployment persistence. With `psql` available:

```sh
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000001_deployments.up.sql
```

The down migration is provided for controlled rollback. It drops deployment
tables and must only be run intentionally:

```sh
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
