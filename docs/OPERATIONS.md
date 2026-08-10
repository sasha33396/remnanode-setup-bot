# Production operations

## Installation

1. Install Docker Engine with Compose v2 and provide PostgreSQL 17.
2. Copy `.env.example` to `.env`, replace every placeholder, and keep it outside
   source control with owner-only permissions.
3. Generate a dedicated Ed25519 deployment SSH key and set the host path in
   `DEPLOY_SSH_PRIVATE_KEY`.
4. Grant the Cloudflare token only DNS record read/edit access to SNI zones.
5. Apply migrations `000001` through `000005` in numeric order.
6. Run `docker compose up -d --build`; verify `/healthz`, `/readyz`, `/metrics`,
   container health, and Telegram `/start`.

Use the ACME staging directory for the first end-to-end validation to avoid
production rate limits. Switch to production only after DNS-01, issuance, Node
reload, health validation, and rollback have been smoke-tested.

## Configuration

Required secrets are `TELEGRAM_BOT_TOKEN`, `REMNAWAVE_TOKEN`,
`DNS_BALANCER_TOKEN`, `CF_API_TOKEN`, the password in `DATABASE_URL`, and the
deployment SSH private key. They must not appear in arguments, logs, Telegram,
or xray-sni `.env` files.

Operational controls include deployment/distribution concurrency, HTTP/SSH/API
timeouts, ACME propagation timeouts, renewal windows, pinned xray-sni source,
and `CERTIFICATE_STORE_PATH`. The certificate store must be persistent and
owner-only.

## Deployment workflow

The durable order is `PREFLIGHT`, `PREPARING_CERTIFICATE`, `PROVISIONING`,
`CREATING_REMNAWAVE_NODE`, `WAITING_REMNAWAVE`, `ADDING_TO_DNS`, `COMPLETED`.
Provisioning must finish before Node creation, and DNS must not change until
Remnawave reports the Node connected.

One normalized SNI has one centrally active certificate. New versions are
issued and validated, stored immutably, distributed to all DNS-configured Nodes,
and only then made centrally active. Each Node receives temporary files over
SSH stdin, atomically switches the pair, reloads Caddy, and performs a local TLS
`/health` request. Failed health restores the previous Node files. Partial
distribution leaves the previous central active version unchanged, records
each Node result, and best-effort redistributes the previous version to Nodes
that already accepted the failed candidate.

## Operator recovery actions

Authorized Telegram users can use:

```text
/retry_step <deployment-uuid>
/retry_dns <deployment-uuid>
/recheck <deployment-uuid>
/cancel_deployment <deployment-uuid>
/logs <deployment-uuid>
/bootstrap_certificate <sni-domain> CONFIRM
/replace_ip <deployment-uuid> <new-public-ip> CONFIRM
```

`/retry_step` is allowed only for stages with an idempotent recovery contract.
Preflight cannot be retried because its temporary root password is not persisted.
`/logs` returns only safe persisted summaries, never raw SSH/API output.

`/bootstrap_certificate` is an explicit one-time transition for an SNI whose
DNS pool contains legacy Nodes that do not yet have a verified deployer SSH
identity. It activates the newest valid staged certificate without creating a
new ACME order, requires every already-managed target to accept the candidate,
and records each unknown DNS IP as `LEGACY_ACKNOWLEDGED` with the authorizing
Telegram user ID. Acknowledged legacy targets are excluded from later central
distribution until they are imported; managed target failures still block
activation. Use this command only after comparing the DNS pool with Remnawave.

`/replace_ip` is allowed only for a completed deployment with a persisted
Remnawave Node UUID. It updates the Node address, waits for reconnection,
replaces the old simple IP in the matching DNS-balancer zone without changing
other IPs, and only then updates the deployment's canonical VPS address. The
same command safely resumes partial completion. Advanced `nodes:` zones are
rejected for manual review rather than rewritten as simple `ips:`.

At startup, unfinished jobs are inspected without repeating external effects.
Existing Remnawave Nodes are matched by exact name and IP, DNS is read before a
retry is offered, and ambiguous states become `MANUAL_REVIEW`.

## Backup requirements

PostgreSQL and `certificate_store` form one recovery set. The store contains
private keys, all retained versions, active markers, and the ACME account key.

For a consistent backup:

1. Stop the deployer so no issuance or activation is in flight.
2. Create an encrypted `pg_dump` of the application database.
3. Create an encrypted archive/snapshot of the complete certificate volume,
   preserving ownership and modes.
4. Record the image digest, migration level, and pinned xray-sni revision.
5. Test restoring both artifacts in isolation.

Never back up only PostgreSQL or only the certificate store. Protect backups as
production private keys and test retention/deletion procedures.

## Disaster recovery

1. Restore PostgreSQL and the certificate store on an isolated replacement.
2. Restore store directory mode `0700`, key modes `0600`, and deployer UID/GID.
3. Restore the deployment SSH key from its separate secret backup.
4. Apply only migrations newer than the recorded backup level.
5. Start with Telegram blocked at the network boundary; verify readiness,
   fingerprints/active markers, and Remnawave/DNS connectivity.
6. Review every startup `MANUAL_REVIEW` classification before explicit retry.
7. Re-enable Telegram and monitor error/renewal metrics.

If only the store is lost, Nodes keep installed certificates but central renewal
and rollback are unsafe until restore/reissuance. If PostgreSQL is lost, do not
infer deployment ownership solely from DNS; restore the database first.

## Upgrade procedure

1. Compare release notes and the pinned xray-sni external contract.
2. Back up PostgreSQL and certificate storage as one set.
3. Stop the deployer and apply migrations in numeric order.
4. Deploy one image by digest; verify readiness, recovery, metrics, and Telegram.
5. Run a staging certificate renewal/distribution smoke test.
6. Keep the previous image and backup through a renewal window.

Do not run destructive down migrations merely to roll back an application image.

## Troubleshooting

- `readyz` is 503: check PostgreSQL and migrations `000001`–`000005`.
- ACME fails: check email, directory, Cloudflare scope, zone discovery, TXT
  propagation, CAA, and clock.
- One Node fails distribution: inspect its SSH fingerprint, pinned xray-sni
  revision, certificate modes, `snisite`, Caddy config, and local `9443/health`.
- `DNS_FAILED`: recheck Remnawave, inspect DNS-balancer, then use `/retry_dns`;
  never delete the healthy Node as cleanup.
- `MANUAL_REVIEW`: compare safe steps with Remnawave/DNS. Do not guess by editing
  the database.
- Low `certificate_expiry_days`: inspect renewal failures immediately.

## Production-readiness checklist

- [x] Secrets are excluded from normal APIs and structured logs.
- [x] Cloudflare credentials remain central and never reach Nodes.
- [x] Certificate/key/SAN/time validation occurs before storage/use.
- [x] Versions are immutable, protected, metadata-backed, and rollback-capable.
- [x] Per-SNI issuance locking works in process and through PostgreSQL.
- [x] Distribution is bounded, per-Node recorded, atomic, health-checked, and
  locally rolled back on failure.
- [x] Deployment ordering, cancellation, duplicate protection, and DNS-only
  retry are enforced.
- [x] Startup recovery inspects Remnawave/DNS and marks ambiguity for review.
- [x] Health/readiness, metrics, graceful shutdown, and Docker hardening exist.
- [x] Tests cover cache, issuance, concurrency, renewal, invalid/mismatched
  material, partial failure, rollback, orchestration, recovery, and redaction.
- [ ] Complete a disposable VPS + Cloudflare staging ACME smoke test.
- [ ] Complete an encrypted backup/restore exercise.
- [ ] Add production alert rules and dashboards.

## Remaining risks

- Fake tests cannot prove real Cloudflare, Let's Encrypt, Remnawave,
  DNS-balancer, or VPS behavior; staging smoke tests remain mandatory.
- Deployment duplicate-run locking is process-local. Run one deployer replica;
  only certificate issuance has PostgreSQL cross-instance locking.
- Filesystem active markers and PostgreSQL cannot share one transaction.
  Compensation and marker repair exist, but a crash between writes may need
  review.
- Filesystem permissions protect keys; application-level encryption is absent.
  Use encrypted disks/volumes and backups.
- Telegram poll offsets are not persisted; restart can replay an update.
- DNS-balancer has no automatic remove/rollback operation by design.
- Migrations are operator-applied; startup detects but does not apply them.
- ACME propagation uses the host resolver and may need split-DNS tuning.
- `/metrics` has no application authentication; keep it local/protected.
- The deployer container runs as UID 0 to consume ordinary root-owned `0600`
  file-backed Compose secrets. Its filesystem is read-only and capabilities are
  restricted, but access to the Docker host and secret source paths remains a
  security boundary.
