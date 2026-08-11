# Multi-panel Remnawave and certificate architecture

Status: first implementation slice completed on `feature/multi-panel-support`
(August 2026). Treat this file as the durable handoff/memory for future chats.

Implemented in the first slice:

- static `PANELS_JSON` configuration with environment-secret references;
- per-panel Remnawave, optional DNS, Cloudflare/ACME, certificate store and
  certificate lock instances;
- persisted `panel_id` ownership for deployments and certificate metadata;
- Telegram panel selection for Add Node and Change IP;
- explicit DNS `SKIPPED` behavior and inventory-based certificate targets for
  panels whose DNS mode is disabled;
- non-destructive read fallback for the default panel's pre-migration
  certificate files and ACME account key.

Not yet implemented: imported/external/node-local issuers, an interactive
safe import of legacy Nodes into inventory, and a panel-selection UI for the
certificate bootstrap recovery action.

## Current implementation snapshot

The code currently uses these concrete decisions:

- `PANELS_JSON` is the multi-panel configuration entry point. Tokens are not
  embedded in JSON; JSON contains environment-variable names whose values are
  resolved during startup.
- If `PANELS_JSON` is absent, the old single-panel variables are mapped to a
  compatibility panel with ID `default`.
- Every configured panel currently uses central ACME DNS-01 issuance with its
  own Cloudflare token and ACME account key. Per-panel alternative issuer
  modes remain future work.
- A panel's DNS-balancer mode is either `enabled` or `disabled`. Disabled DNS
  records `add_dns` as `SKIPPED` and is considered a successful deployment.
- The Add Node and Change IP wizards ask the operator to select a panel when
  more than one panel exists. A single configured panel is selected
  automatically for backwards-compatible operator UX.
- Change IP reads legacy Nodes directly from the selected Remnawave API. A
  deployment row is not required. Only a managed Node's persisted deployment
  IP is updated.
- Certificate database identity, distribution targets, filesystem paths and
  advisory locks are panel-scoped. The effective certificate identity is
  `(panel_id, normalized SNI)`.
- Migration `000006_multi_panel_scope` assigns all existing rows to
  `default`, then installs composite certificate keys and cross-panel foreign
  keys.
- New certificate files live below
  `/var/lib/deployer/certificates/<panel-id>/`. The `default` panel can read
  the old unscoped active files and reuses the old ACME account key when it
  exists, so rollout is non-destructive.
- For DNS-enabled panels, certificate recipients come only from that panel's
  DNS balancer and are matched to that panel's deployments. For DNS-disabled
  panels, only trusted managed deployments with a Node UUID and SSH host-key
  fingerprint are recipients; unknown legacy Nodes are not sent private-key
  material.
- Deployment retries and recovery resolve the panel from the persisted
  deployment rather than current Telegram state.

Before deploying this branch, apply migration `000006` and validate both its
up and down paths against a staging copy of PostgreSQL. Unit tests and `go
vet` pass, but the development workstation used for this slice did not have a
Docker daemon, so the migration has not yet been executed against a live
PostgreSQL instance.

This document preserves the agreed direction for continuing the design or
implementation in another Codex chat. It is the source of truth for the
multi-panel discussion.

## Context

The bot currently works with one Remnawave panel and one DNS-balancer client.
The intended extension is to support multiple independent Remnawave panels.
Each panel has its own DNS-balancer integration, or has no DNS balancing at
all. Certificates and their distribution targets must never be mixed between
panels, even when panel domains and subdomains are currently different.

The existing button-based operations should remain suitable for a
non-technical operator. New ordinary operations must not require Telegram
commands or internal UUIDs.

## Agreed operator flows

Adding a Node:

```text
Add Node
  -> select panel
  -> select a Host from that panel
  -> enter Node name
  -> enter VPS IP
  -> enter temporary password
  -> review and confirm
```

Changing a Node IP:

```text
Change IP
  -> select panel
  -> enter exact Node name or current IP
  -> review Node information and DNS zones
  -> Change / Cancel
  -> enter new public IP
  -> update the selected panel and its DNS integration, when enabled
```

Legacy Nodes must be discoverable directly through the selected Remnawave
panel even when they do not have a deployment row.

## Panel identity and configuration

Every panel requires a stable internal `panel_id`. A display name is only for
Telegram and must not be used as the durable identity.

Conceptual configuration:

```yaml
panels:
  - id: europe
    name: Europe
    remnawave:
      url: https://panel-europe.example
      token_secret: remnawave_europe_token
    dns:
      mode: enabled
      url: http://europe-dns-balancer:8741
      token_secret: europe_dns_token
    certificate:
      issuer: acme_dns01
      target_source: dns_balancer
      distribution: central_ssh

  - id: test
    name: Test
    remnawave:
      url: https://panel-test.example
      token_secret: remnawave_test_token
    dns:
      mode: disabled
    certificate:
      issuer: imported
      target_source: inventory
      distribution: central_ssh
```

The initial implementation should use a static configuration and Docker
secrets/environment references. Managing panel credentials through Telegram is
not recommended. Tokens must not be stored in plaintext database columns,
logs, callback data, or Telegram messages.

The application should build an isolated client bundle for each panel:

```text
panel_id
  -> Remnawave API client
  -> optional DNS-balancer client
  -> certificate policy and issuer
  -> certificate target resolver
```

## DNS policy per panel

Use an explicit mode instead of a boolean:

- `enabled`: DNS updates are required. Failure leaves the operation in a
  recoverable DNS-failed state.
- `disabled`: no DNS integration exists. DNS steps are explicitly skipped and
  this is not an error.
- `optional`: possible future mode, but not recommended initially because it
  can hide partial configuration.

An enabled panel may use only its own DNS-balancer URL and token. A disabled
panel must not fall back to another panel's balancer or to a global default.

For IP replacement, the bot searches all zones in the selected panel's
balancer containing the old IP. It preserves every other simple `ips:` or
advanced `nodes:` entry. If DNS is disabled, the confirmation must clearly say
that only Remnawave will be updated.

For Node deployment, `add_dns` is required when DNS is enabled. When disabled,
the step should be recorded as `SKIPPED` with an operator-safe explanation.

## Certificate model: three independent decisions

Certificate handling must separate:

1. `issuer`: how a certificate is obtained.
2. `target_source`: how recipient Nodes are selected.
3. `distribution`: how certificate material reaches those Nodes.

The absence of a DNS balancer does not imply the absence of DNS access for
ACME. A panel can have balancing disabled while using a restricted DNS API
credential only for `_acme-challenge` records.

### Issuer options

- `acme_dns01`: central ACME issuance using a panel/scope-specific DNS
  credential.
- `acme_http01` or another supported challenge: only where the infrastructure
  makes it safe and reliable.
- `imported`: an externally issued certificate and private key are safely
  imported, validated, versioned, and distributed by the bot.
- `external`: another system owns issuance and rotation; the bot integrates
  with its output.
- `node_local`: every VPS owns its own ACME client and rotation. The bot only
  validates/monitors it.

### Target source options

- `dns_balancer`: recipient IPs come from the selected panel's DNS-balancer
  zone.
- `inventory`: recipients come from the bot's trusted Node inventory.
- `remnawave`: recipients are read from the selected panel, but an additional
  unambiguous mapping from each Node to an SNI/certificate scope is still
  required.
- `manual`: recipients are explicitly assigned by an operator.
- `none`: the bot does not distribute certificate material.

### Distribution options

- `central_ssh`: the current central staged distribution and activation model.
- `external_agent`: a separate trusted agent performs installation.
- `manual`: the bot does not deliver material automatically.
- `node_local`: no central private key is distributed.

## Recommended certificate policies

For a panel with DNS balancing:

```text
issuer: acme_dns01 (or the existing central issuer)
target_source: dns_balancer
distribution: central_ssh
scope: panel_id + normalized SNI
```

For a panel without DNS balancing:

```text
issuer: acme_dns01 or imported
target_source: inventory
distribution: central_ssh
scope: panel_id + normalized SNI
```

The inventory option is preferred over `node_local` for this project because
it retains centralized validation, staged activation, rollback, and audit.

## Trusted inventory and legacy Nodes

Inventory entries conceptually contain:

```text
panel_id
remnawave_node_uuid
node_name
current_ip
ssh_host_key_fingerprint
deployment_key_identity
certificate_scope_id
management_status
```

New Nodes created by the bot can enter inventory automatically. A legacy Node
for an inventory-backed panel must be imported once:

1. Select its panel.
2. Select the exact Remnawave Node.
3. Assign its SNI/certificate scope.
4. Verify its SSH host-key fingerprint.
5. Verify/install the deployment public key.
6. Mark it managed and eligible for future certificate rotations.

Until this is complete, the Node remains `MANUAL_REVIEW` and cannot silently
receive central private-key material.

For DNS-backed scopes, unknown DNS targets retain the existing safe bootstrap
idea: known managed targets must validate successfully, while acknowledged
legacy targets remain excluded from later automatic distribution until they
are imported.

## Mandatory certificate isolation

A certificate must never be selected by `sni_domain` alone. Its durable scope
is at least:

```text
panel_id + normalized sni_domain
```

Prefer an internal immutable `certificate_scope_id` referenced by deployments
and inventory entries. Suggested invariants:

- `UNIQUE(panel_id, lower(sni_domain))` for certificate scopes.
- `UNIQUE(panel_id, remnawave_node_uuid)` for inventory Nodes.
- Every deployment stores `panel_id` and `certificate_scope_id`.
- Every certificate state/version stores `panel_id` and scope ID.
- Every distribution record stores panel ID, scope ID, version, and Node
  identity.
- Rotation locks use `panel_id + certificate_scope_id`.
- DNS clients, Remnawave clients, metrics, jobs, retries, bootstrap actions,
  and audit events retain panel context.
- Cross-panel distribution is rejected even if an IP, UUID, name, or SNI
  appears to match.
- There is no implicit global/default DNS or certificate fallback after the
  migration.

Filesystem storage must also be panel-scoped, for example:

```text
/var/lib/deployer/certificates/<panel-id>/<normalized-sni>/<version>/
/var/lib/deployer/certificates/<panel-id>/<normalized-sni>/current
```

Activation, rollback, retention, and cleanup operate only inside one scope.

## Persistence and migration direction

At minimum, deployments need a non-null `panel_id`. Certificate-related
tables need panel/scope ownership. Existing rows should be migrated to a
configured `default` panel before the column becomes non-null.

Host UUID and Node UUID are meaningful only together with `panel_id`:

```text
panel_id + remnawave_host_uuid
panel_id + remnawave_node_uuid
```

Retries must use the panel captured by the original deployment, not whichever
panel is currently selected in Telegram or first in configuration.

Configuration changes must not silently redirect historical jobs to another
panel. Unknown/removed panel IDs should become an explicit safe configuration
error requiring operator action.

## Safety and operator presentation

Every destructive confirmation should show the panel name. Certificate
operations should additionally show the SNI, source of recipients, number of
managed targets, and number of legacy/manual-review targets.

Example:

```text
Panel: Europe
SNI: fl-modx.webedg.net
Certificate source: ACME DNS-01
Recipient source: this panel's DNS balancer
Managed Nodes: 8
Legacy / manual review: 2
```

Internal panel IDs, Node UUIDs, deployment IDs, tokens, certificate paths, and
private-key details must not be exposed in ordinary Telegram UI or callback
data.

Multi-system changes should validate the selected panel and expected old
state, reject duplicate addresses inside that panel, and compensate completed
API updates when a later required update fails. No compensation may touch a
different panel.

## Suggested implementation order

1. Introduce panel configuration, validation, and an isolated client registry.
2. Add `panel_id` to deployments and migrate existing data to `default`.
3. Add panel selection to Add Node and Change IP Telegram wizards.
4. Route Remnawave and optional DNS operations strictly by deployment panel.
5. Add panel-scoped certificate scopes, filesystem layout, database ownership,
   locks, metrics, and migrations.
6. Implement the DNS-backed certificate target resolver per panel.
7. Implement trusted inventory and legacy import for panels without DNS.
8. Add migration tooling for existing certificate versions and active markers.
9. Add cross-panel isolation, retry, rollback, and legacy integration tests.
10. Update operational backup/restore and deployment documentation.

## Remaining decisions for the next discussion

Before extending the first slice, decide:

1. Whether `PANELS_JSON` remains the long-term configuration or is later
   replaced with YAML/database metadata plus secret references.
2. Which production panels have DNS balancing enabled and the endpoint/secret
   reference for each.
3. For each panel, whether issuance remains ACME DNS-01 or changes to
   certificate import or external management.
4. Whether the next release includes the button-based legacy inventory import
   or initially supports only new managed Nodes for inventory-backed scopes.
5. Whether to migrate old `default` certificate files into the new directory
   layout after successful rollout or retain the read fallback indefinitely.
6. How certificate bootstrap should present panel selection without requiring
   Telegram commands or exposing internal IDs.

## How to resume in another Codex chat

Ask Codex to read this file and the current implementation before proposing or
making changes:

```text
Read docs/MULTI_PANEL_CERTIFICATES.md and inspect the current project.
Continue from branch feature/multi-panel-support. First verify migration
000006 and the current tests. Preserve strict panel isolation. The next likely
work is the button-based legacy inventory import and explicit per-panel
certificate issuer modes; do not silently distribute certificate private keys
to unknown legacy Nodes.
```
