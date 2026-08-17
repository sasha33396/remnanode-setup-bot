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
Telegram user IDs. Each panel's `remnawave.api_ip` and the global `METRICS_IP`
must be literal IPv4 or IPv6 addresses. API endpoints require HTTP(S) URLs, and `DATABASE_URL` requires a
PostgreSQL URL.

`XRAY_SNI_REPO_URL` defaults to the external-certificate fork and must be a
credential-free HTTPS URL. `XRAY_SNI_REF` defaults to the pinned
`v0.1.0-external` tag; branch names such as `main` are rejected.

`ACME_EMAIL` is the ACME account contact. `CF_API_TOKEN` is used only by the
central DNS-01 client and is never copied to a Node. Certificate versions and
the ACME account key are stored under `CERTIFICATE_STORE_PATH`.

Multi-panel mode is configured in `config/panels.yml`. Copy
`config/panels.yml.example`, edit the panel URLs and set
`PANELS_CONFIG_FILE=/etc/remnanode-setup-bot/panels.yml` in `.env`. Each entry
has a stable lowercase `id`, display `name`, Remnawave URL, API IP and token-environment
reference, its own Cloudflare token reference, and a DNS configuration whose
mode is `enabled` or `disabled`. Token values stay in `.env` and are never
embedded in YAML. `PANELS_JSON` remains supported for backwards compatibility,
but cannot be combined with the YAML file. When neither source is configured,
the legacy variables, including `REMNA_API_IP`, create one panel named `Default` with ID `default`.

The optional `HEALTH_ADDR` environment variable controls the local HTTP bind
address and defaults to `:8080`. Docker Compose sets it automatically.
For Compose, `DATABASE_URL` must use `postgres` as the database hostname.

## Run locally

Export all variables listed in `.env.example`, plus a PostgreSQL connection URL
in `DATABASE_URL` and the deployment key path in `DEPLOY_SSH_PRIVATE_KEY`, then:

```sh
go run ./cmd/deployer
```

Migrations `000001` through `000006` must be applied first. Startup verifies
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

The **Сменить IP** menu contains three workflows:

- **Панель + DNS-балансировка** updates the Remnawave Node and every matching
  DNS-balancer zone. It supports both Nodes created by this bot and legacy
  Nodes that exist only in Remnawave.
- **Смена IP на Cherry (сервер)** performs the operating-system step from the
  Cherry IP helper: it connects to the server with a transient root password,
  adds an already assigned floating IPv4 address live, and persists it in
  netplan. The existing network addresses and routes are not removed. A backup
  is created before writing netplan and a failed generate/apply restores it.
  The wizard then asks whether the Node already exists in Remnawave. For an
  existing managed or legacy Node it obtains the current server IP from the
  selected panel and updates the Node plus matching DNS zones after the server
  is ready. If the Node has not been added yet, the server IP is entered
  manually and only the server network configuration is changed.
  This workflow does not order or assign an address through the Cherry API; the
  floating IP must already be assigned to that server in Cherry Servers.
- **Смена IP на Royal (сервер)** connects through the current IPv4, locates the
  matching netplan interface, replaces only that IPv4 address while preserving
  its prefix and all IPv6 configuration, and changes the default IPv4 gateway
  to the new address's `x.x.x.1`. It backs up and validates netplan before an
  asynchronous apply, then treats the operation as successful only after SSH
  is verified through the new address. As with Cherry, an existing managed or
  legacy Remnawave Node and its DNS zones can be updated automatically, or the
  server-only path can be used before the Node is added to Remnawave.

The **Развёртывания** list is actionable. Opening a deployment shows only the
buttons valid for its durable state: safe logs, step/DNS retry, Remnawave
recheck, cancellation, or certificate repair. Certificate repair obtains the
deployment UUID and SNI from PostgreSQL, acknowledges reviewed legacy DNS
targets, and resumes the failed certificate step without requiring commands or
manual database queries.

### Live deployment checklist and operator codes

Telegram renders deployment progress as one continuously updated checklist.
The six durable workflow stages remain visible, while the VPS provisioning
stage expands into its component stages (`system`, `docker`, `firewall`,
`remnanode`, exporters, `logrotate`, `xray_sni`, and `healthcheck`). The symbols
have a strict meaning:

- `⬜` has not started;
- `🔄` is currently running;
- `✅` completed successfully (a previously satisfied idempotent stage also
  uses this symbol and says that it was already configured);
- `⚠️` completed or can continue, but the operator should read the bracketed
  warning code and parenthesized explanation;
- `❌` is a blocking failure. The checklist keeps the failed stage, safe error
  code, safe message, and points to the deployment card actions.

Operator-facing codes are stable, contain only uppercase ASCII letters,
digits, and hyphens, and never include secrets or raw command output. `W-...`
means a non-blocking warning; `E-...` means deployment stopped. Persisted
underscore error codes are displayed with an `E-` prefix and hyphens, for
example `PROVISIONING_FAILED` becomes `E-PROVISIONING-FAILED`.

Common warning codes:

| Code | Meaning / action |
| --- | --- |
| `W-DNS-DISABLED` | DNS balancing is disabled for the selected panel; no action is required if intentional. |
| `W-DOCKER-NOT-INSTALLED` | Docker is absent and will be installed automatically. |
| `W-REMNANODE-EXISTS` | Existing Remnanode files or containers will be inspected and reconciled. |
| `W-XRAY-SNI-EXISTS` | Existing Xray SNI state will be inspected and reconciled. |
| `W-SSH-PORT-NOT-DETECTED` | SSH works, but port 22 was not visible in the listening-port inspection. |
| `W-COMPONENT-INSPECTION-FAILED` / `W-COMPONENT-INFO-INVALID` | Existing component state could not be fully identified; watch the corresponding provisioning stage. |
| `W-CONTAINER-INSPECTION-FAILED` | Existing Docker containers could not be fully inspected. |

Common blocking error families:

| Codes | Operator action |
| --- | --- |
| `E-PREFLIGHT-FAILED`, `E-PREFLIGHT-REJECTED`, `E-HOST-INVALID` | Check VPS reachability/requirements or select a valid Host, then start again. |
| `E-CERTIFICATE-*` | Open the deployment card and use certificate repair or safe logs before retrying. |
| `E-KEYGEN-FAILED`, `E-PROVISIONING-FAILED`, `E-PROVISIONING-<COMPONENT>` | Inspect the marked VPS component, open safe logs, and retry the failed provisioning step. |
| `E-DUPLICATE-NODE`, `E-NODE-CREATE-FAILED`, `E-NODE-UUID-*` | Resolve the Remnawave Node conflict/persistence problem, then retry. |
| `E-NODE-CONNECTION-*`, `E-NODE-WAIT-FAILED`, `E-NODE-NOT-CONNECTED` | Check Remnanode connectivity and use the Remnawave recheck action. |
| `E-DNS-*`, `E-NODE-NOT-HEALTHY` | Keep the healthy Node, fix DNS/health, and use the dedicated DNS retry or recheck action. |
| `E-*-PERSIST-FAILED`, `E-PERSISTENCE-*`, `E-STATE-PERSIST-FAILED`, `E-STEP-PERSIST-FAILED` | Check PostgreSQL/service health before retrying; durable state could not be safely recorded. |

Unknown internal failures are deliberately collapsed to
`E-DEPLOYMENT-FAILED`; arbitrary error strings are never sent to Telegram.

### Deployment history and detailed safe logs

The **Развёртывания** screen uses the same operator model after the live run
has finished. The recent list is localized and shows, for every Node, the
panel, human-readable state, current workflow stage, `N/6` progress, last
update time, and a safe error code when one exists. Raw database statuses such
as `PROVISIONING` and `COMPLETED` are not shown to the operator.

The deployment card additionally shows the target VPS IP, created/started/
updated/completed timestamps, total duration, current stage, progress, and the
durable safe error code/message. The **Подробный журнал** view renders every
persisted workflow and provisioning component as a separate entry with:

- `✅`, `🔄`, `⚠️`, `❌`, `⛔`, or `⬜` state;
- localized main/component name;
- explicit result summary;
- warning or error code where applicable;
- start and completion timestamps plus elapsed duration;
- refresh and back-to-card buttons for an active deployment.

Detailed logs remain deliberately safe: they contain only persisted allow-
listed summaries and codes. Raw SSH/API output, root passwords, internal UUIDs,
and certificate SNI values are never rendered in the history or log view.

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

The manager enforces one active certificate per panel and normalized SNI, obtains and
renews certificates with a panel-scoped ACME account and Cloudflare credential, stores protected
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
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000005_certificate_legacy_targets.up.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000006_multi_panel_scope.up.sql
```

The down migration is provided for controlled rollback. It drops deployment
tables and must only be run intentionally:

```sh
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000006_multi_panel_scope.down.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000005_certificate_legacy_targets.down.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000004_production_recovery.down.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000003_certificate_manager.down.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000002_deployment_ssh_host_key.down.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f migrations/000001_deployments.down.sql
```

## Run with Docker Compose

After creating `.env`, copying `config/panels.yml.example` to
`config/panels.yml`, and creating the SSH key file referenced by `.env`:

```sh
docker compose up --build
```

The Compose stack persists PostgreSQL in `postgres_data` and protected
certificate versions/account keys in panel-specific directories inside `certificate_store`. The deployer runs as
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
