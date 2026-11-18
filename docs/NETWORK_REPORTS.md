# Read-only network reports

The web console's **Настройки сети** button opens persisted network reports.
**Проверить все ноды** starts a background scan; **Проверить снова** refreshes
one node. Results survive application restarts. The timestamp is the last
attempt, not a guarantee that the server still has this configuration.

The collector uses the existing deployment SSH key and pinned server identity.
It never adopts legacy nodes, installs packages, writes remote files, changes
addresses, reconciles firewall rules, or restarts networking. Legacy nodes
without managed access are recorded as skipped, including nodes behind hidden
Hosts. Host visibility and balancing strategy do not affect selection.

Collected evidence:

- live IPv4/IPv6 addresses, interfaces and prefixes; the primary address is
  matched against the Node address in the selected Remnawave panel;
- IPv4/IPv6 routes from all tables and policy rules;
- active networkd, NetworkManager and networking services;
- candidate Netplan, interfaces/interfaces.d, networkd and NetworkManager
  configuration paths, including generated files under /run/systemd/network;
- exact primary-address matches in Netplan, interfaces and networkd files.

Remote `ip` must support JSON output. Each command is bounded to 15 seconds,
each node to 90 seconds. Scans are sequential and one scan runs per application
instance. Configuration inspection is limited to 64 candidate files.
Only structured evidence is stored; raw configuration contents and
NetworkManager profile contents are not persisted. NetworkManager profiles
are not read at all because they can contain secrets.

Version 2 calls the panel address **Адрес Node**, never the server's base IP.
Other addresses on its interface are shown separately and must be preserved.
Live routes retain address family, flags (including onlink), metric, scope,
protocol and type. Rules retain address family. Netplan evidence includes
section ID, match name/MAC, set-name, renderer, DHCP flags, addresses, gateways
and configured routes with on-link, table and metric. Only allowlisted fields
are stored; authentication settings are excluded.

Generated `/run/systemd/network/10-netplan-*.network` files are linked to a
Netplan source only when section ID, addresses/prefixes and the observed
interface agree with exactly one source. Exact name and MAC matches are
supported, including set-name renames. Permanent-MAC matches are corroborated
only when they agree with the observed MAC; other cases require review.
Conflicting sources and orphan generated files still require review.
**Конфигурация адреса Node найдена** is evidence, not certification of manager
ownership or readiness for automatic IP replacement. Merged Netplan files,
driver/glob matching, DHCP-only addresses, custom interfaces includes and
NetworkManager ownership are not resolved. The report does not infer hosting
providers or parameters for a new IP.

Old reports remain readable and are marked for re-scan. Version 2 uses the
existing JSON report column: no additional database migration is needed.

## Upgrade

Apply migration 000010 before starting the new application (assuming 000001
through 000009 are already applied):

```sh
docker compose exec -T postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1' < migrations/000010_network_reports.up.sql
docker compose up -d --build deployer
```

Report failures and skips are isolated per node. A persistence error stops the
job; already saved reports remain available. An unsuccessful probe replaces
that node's previous report with the latest failure, so old evidence is not
presented as a successful fresh scan.
