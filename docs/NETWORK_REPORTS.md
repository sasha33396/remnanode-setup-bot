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

**Адрес найден в конфигурации** means the primary address was found once in
live interfaces and in one candidate file. It does not certify which manager
owns the interface or readiness for automatic IP replacement. Generated
networkd files plus Netplan can legitimately produce multiple matches and
require review. DHCP, custom interfaces include paths, YAML aliases, complex
networkd matching and NetworkManager profile ownership are not resolved in
this first version. The report does not infer hosting providers, new prefixes
or gateways. Provider API integration and verified network-change adapters
are separate work.

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
