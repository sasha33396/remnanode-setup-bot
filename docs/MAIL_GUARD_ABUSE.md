# Mail Guard and abuse monitoring

## Mail Guard

The provisioning firewall stage installs an independent `inet smtp_guard`
nftables table. By default it drops TCP and UDP destination ports
`25,26,110,143,465,587,993,995,2525,4190,8025` in the `input`, `output`, and
`forward` hooks for IPv4 and IPv6. It does not flush the ruleset and does not
modify Docker or Remnanode tables. The apply script validates the complete
transaction with `nft -c` before committing it. `smtp-guard.service` restores
the table after reboot and starts before Docker.

`MAIL_GUARD_ENABLED` enables the feature and `MAIL_GUARD_PORTS` changes the
comma-separated port set without recompiling. Removing mail ports permits the
corresponding legitimate VPN traffic, while the default full list intentionally
prevents SMTP, submission, IMAP and POP3 use through the node.

New nodes receive the guard during the normal firewall stage. Existing managed
nodes can use **🛡 Синхронизировать защиту** on a node card, or the confirmed
bulk action. Legacy nodes request a one-time root password and first complete
the existing full adoption workflow; the password is cleared and never stored.
The Remna-to-DNS action is separate and unchanged.

## Access-log monitor

When `ABUSE_MONITOR_ENABLED=true`, the deployer polls managed nodes through the
existing pinned deploy-key SSH connection. It opens no new port and reads only
new bytes from `/var/log/remnanode/access.log`, at most 512 KiB per node and
poll. PostgreSQL stores inode/offset/partial-line checkpoints. Rename and
copy-truncate rotation are handled; `.1.gz` is used as bounded recovery input.
Event fingerprints make recovery and process restarts idempotent.

The first successful observation starts at the current end of file, so startup
does not emit alerts for an unbounded history. Events are retained for seven
days by default. Legacy-only nodes are not polled because the service does not
retain their password; after adoption they are monitored as managed nodes.

Mail-port attempts, destinations, `BLOCK` and `DIRECT` are always calculated
from each node's `access.log`. Unique client IP/ASN windows are obtained from
Remnawave v2 IP-control (`fetch-users-ips`) and its per-IP `lastSeen` value.
The panel token therefore needs both IP-control fetch/result permissions. If a
complete panel snapshot cannot be obtained, the monitor explicitly falls back
to access-log IP counts for that poll. Identity is scoped as `panel_id +
email/User ID`; equal IDs from different panels never mix.

`accepted` means Xray accepted the request and `DIRECT` is only a route choice.
Neither proves SMTP succeeded. `BLOCK` events remain attempts and contribute
to behavioural counts. Ordinary IMAP/993 use alone has zero risk weight. The
score explains its contributing signals and never calls a user a confirmed
spammer. No user is blocked or disabled automatically.

Optional local IP intelligence is a CSV selected by `ABUSE_IP_INTEL_FILE`:

```text
# CIDR,ASN,provider,category
203.0.113.0/24,AS64500,Example Hosting,HOSTING
```

Server-side IPinfo enrichment is optional. Set `IPINFO_TOKEN` and use
`IPINFO_TIER=lite` for the free unlimited Lite endpoint (ASN/provider), or
`IPINFO_TIER=core` for ASN type plus hosting/anonymous flags. Results are kept
in the existing PostgreSQL cache for seven days and failures safely fall back
to the local CSV. Without IPinfo or this file, ASN/category are `UNKNOWN`.
`ABUSE_ASN_OVERRIDES=AS64500=HOSTING,AS64501=PROXY` overrides either source.

Alerts go only to `TELEGRAM_ALLOWED_USERS`, label whether IP counts came from
Remnawave API or the log fallback, and add safe `https://ipinfo.io/<ip>` buttons
for the busiest client IPs. They also include Details, Statistics and Ignore
for 24 hours. Incidents are unique by panel, user and anomaly type. The default
cooldown is 60 minutes, with an immediate alert when risk increases. Incident
state survives restarts.

## Production update

Back up PostgreSQL, pull this branch, update `.env` from `.env.example`, and
apply migration `000009` before starting the new image:

```bash
cd /opt/remnanode-setup-bot
git fetch origin
git switch feature/mail-guard-abuse-monitor
git pull --ff-only origin feature/mail-guard-abuse-monitor
set -a; . ./.env; set +a
docker compose up -d postgres
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 < migrations/000009_abuse_monitor.up.sql
docker compose up -d --build
docker compose logs -f --tail=100 deployer
```

After startup, use the confirmed bulk protection action in Telegram. It is not
run automatically against the fleet. The old periodic low-online/recovery
Telegram sender is no longer started. Live counts, node grouping, metrics,
manual inventory and health checks remain available.
