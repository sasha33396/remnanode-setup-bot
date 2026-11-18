# Leastload IP replacement

The web console exposes **Замена IP leastload**. Select a panel, check Nodes,
enter their new IPv4 addresses or import `old_IP -> new_IP` lines (maximum 50),
then preview the changes. Confirm that the provider has already assigned the
new addresses to the intended servers before applying the stored plan.

Supported targets have exactly one hidden Host pointing only to that Node,
a dedicated DNS hostname with exactly one Cloudflare A record, and a managed
deployment with a pinned SSH host key. Host names are not used to infer a
load-balancing strategy. The current version changes direct Cloudflare records;
the existing DNS-balancer bulk workflow remains separate.

The first version supports one static Netplan file, an unambiguous Ethernet
section matched to the live interface, and ordinary routing rules. It preserves
additional IPv4 addresses, IPv6, private interfaces and the Xray profile.
Profiles referencing the replaced address in `sendThrough` or `listen`, YAML
anchors, address references in policy/routes and ambiguous configurations are
blocked. Unknown provider gateway/mask conventions are never guessed.

## Three modes

- **Already configured and live**: connect to the new IP using the same pinned
  SSH identity; verify persistent configuration and addresses/routes; update
  only Remnawave, Cloudflare and the deployment address. Keep the old IP.
- **Configured but not live**: do not add a duplicate or remove the old IP.
  Apply the existing Netplan through the same rollback/verification mechanism.
- **Not configured**: replace only the old address scalar in the selected
  Ethernet address list, preserving its prefix and every other YAML setting.
  This mode requires an explicit administrator network rule below.

The preview re-reads the server; it does not rely on an old inventory snapshot.
Before applying, the plan is rechecked and its file hash/mode/interface must
still agree. Unstarted plans expire after 30 minutes; re-prepare an expired or
rolled-back plan. IPv4 swaps/cycles and duplicate source/target mappings are
not supported.

## Approved network rules

Set `LEASTLOAD_NETWORK_RULES` in the bot's `.env`. Compose already supplies
that file as the deployer's `env_file`. The format is a semicolon-separated
list of `old-CIDR>new-CIDR` pairs. A rule explicitly asserts that replacement
addresses in the target range use the existing prefix, gateway and routes:

```dotenv
# Documentation example only; use provider-confirmed real ranges.
LEASTLOAD_NETWORK_RULES=192.0.2.0/24>198.51.100.0/24
```

Do not configure ranges merely because they share a hosting company name.
An empty value blocks replacement of unconfigured IPs. Already-configured
addresses do not require a network rule. The tool does not allocate an IP or
call hosting-provider assignment APIs, and cannot prove provider ownership
from Netplan alone.

## Recovery

Each operation and per-node stage are stored in PostgreSQL. Open a saved
operation in the web console and press **Выполнить готовые / продолжить** to
resume after interruption. Only the original operator can execute it.
Nodes are processed sequentially; failed Nodes do not stop unrelated items.
Cross-instance advisory locking serializes these jobs. Another unfinished
operation for a managed deployment prevents a new replacement on it.

Before mutation, the server receives a private backup and worker under
`/var/lib/remnanode-ip/<operation-item-id>/`. An independent systemd timer is
armed for 180 seconds **before** the network worker starts. A physical-server
flock prevents overlapping workers. The worker validates the original hash,
runs bounded `netplan generate`/`netplan apply`, then waits for verification.
Confirmation and rollback compete for an atomic decision file; a late SSH
confirmation cannot reverse a rollback decision. Do not manually stop these
units or edit the network during an operation.

The bot verifies the new SSH endpoint with the pinned key, the exact saved
Netplan hash, preservation of additional addresses, static/default routes and
routing rules. It then confirms the remote commit. If connection/verification
fails, rollback restores the file and applies it. An unconfirmed/unknown state
never advances to panel/DNS changes. A rollback failure requires manual
recovery through the provider console; automatic rollback is not a guarantee
against host or provider failures.

After the server is confirmed, the bot updates the Node address, patches only
the Cloudflare A record content and saves the deployment SSH address. It does
not enable disabled Nodes, recreate Hosts, change UUIDs, tags, profiles,
inbounds or squads. Failure after server commit leaves the job resumable and
does not roll back to an obsolete address. The network report is refreshed
when available. Completion records configuration updates; it is not a guarantee
that every client has refreshed its DNS cache or the panel has reconnected.

Backups can contain sensitive configuration and remain root-only on the
server for manual recovery. The database contains structured plans and hashes,
not raw Netplan content, passwords or private keys.

## Upgrade and first run

With migrations 000001–000010 already applied:

```sh
docker compose exec -T postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1' < migrations/000011_leastload_ip_jobs.up.sql
docker compose up -d --build deployer
```

First use a single test Node with provider-console access, then expand the
batch. Unit tests cover planning, preservation, stale plans, rollback scheduling,
partial DNS retry and API access controls. They do not replace a real Linux
Netplan/systemd test on the provider's network.
