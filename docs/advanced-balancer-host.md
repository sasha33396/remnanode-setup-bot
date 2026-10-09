# Advanced Balancer Host

The Telegram **Add balancer** workflow provisions one physical balancer host
as independent Remnawave resources:

- one Xray JSON config profile and Reality inbound;
- one Node bound only to that profile and inbound;
- one hidden Host bound to that Node, profile, and inbound;
- one inbound association in the selected internal squad;
- one unproxied Cloudflare A record for the Host FQDN.

The direct Cloudflare record is intentionally not added to the DNS-balancer
service.

## Remnawave prerequisites

Before running the wizard, create or retain the client-visible virtual Host in
the selected Remnawave panel. It must be enabled, visible, have the same
uppercase tag entered in the wizard, and have an Xray JSON template selected.
The template should use `sameTagAsRecipient` for tag-based balancing. In an
Xray balancer selector, `"proxy"` includes tags beginning with `proxy`, while
`"proxy-"` excludes an exact tag named only `proxy`.

The bot rejects preparation when it cannot find the required visible same-tag
recipient configuration, when the FQDN/IP is already in use, or when Cloudflare
already has a conflicting A record. These checks happen before the durable
deployment is started.

## Address model

In one-IP mode, the entered IPv4 is the SSH address, Remnawave Node address,
and Cloudflare A-record value. The generated `DIRECT` outbound has no
`sendThrough` field.

In two-IP mode, the ingress IPv4 remains the SSH, Node, and A-record address.
The separate egress IPv4 is written only to `outbounds[].sendThrough`; clients
never receive it as the Host address.

## Durable workflow and retries

The deployment persists every external identifier before advancing through
certificate preparation, VPS provisioning, config-profile creation, Node
creation, Cloudflare A-record creation, hidden Host creation, and internal
squad association. Repeated calls recover resources by their deterministic
identity and avoid duplicate A records, Hosts, and squad inbound entries.

Failures are logged with `panel_id`, `deployment_id`, step, safe error code,
and safe message. Secrets and the transient root password are not logged or
stored. A failed progress message contains **Open card and retry step**; the
deployment card exposes the valid retry action, so an operator does not need
to query PostgreSQL for a step UUID.

After a process restart, an interrupted advanced stage is converted to a safe
retryable failure. The same deployment card can then resume the idempotent
stage.
