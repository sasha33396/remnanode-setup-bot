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

## SelfSteal and SelfSNI

The operator selects the fallback type before the squad and Host settings:

- **SelfSteal** preserves the existing Caddy/xray-sni provisioning and uses the
  Host FQDN as its SNI.
- **SelfSNI** requests a second, different full FQDN for Reality
  `serverNames`. The Host address still receives the direct DNS-only
  Cloudflare A record pointing to the Node ingress IP. The separate Reality
  SNI is issued on the Node through Certbot DNS-01 and served by an NGINX
  container on `127.0.0.1:9443` with PROXY protocol and TLS 1.3.

SelfSNI stores the panel Cloudflare credential only in a root-only Certbot
credentials file on the Node so `certbot.timer` can renew the certificate.
The token is sent to SSH over stdin and is never embedded in a command,
deployment record, callback, or log. SelfSNI certificates are deliberately
excluded from the central Caddy certificate audit/distribution workflow.

The generated SelfSNI inbound uses `xver: 1`, target
`127.0.0.1:9443`, a generated X25519 private key and a random 16-hex-character
short ID. TCP port 25, private networks/domains, and BitTorrent remain routed
to `BLOCK`; the optional egress IP is written to `DIRECT.sendThrough`.

The Config Profile name is derived from the Host FQDN using Remnawave's allowed
characters. The final DNS label is removed and remaining dots become
underscores: `gmb.wey-land.net` becomes `gmb_wey-land`. The Node name is
requested separately in the wizard.
Node names may include a country flag, for example `🇵🇼 Palau-01` or
`🇷🇺 RU-01`.
Because Remnawave limits Config Profile names, the derived name must contain
2–30 characters.

## Remnawave prerequisites

Before running the wizard, create or retain the client-visible virtual Host in
the selected Remnawave panel. It must be enabled, visible, have the same
uppercase tag entered in the wizard, and have an Xray JSON template selected.
The wizard reads Hosts through the Remnawave API, deduplicates eligible tags,
and presents those tags as buttons instead of asking the operator to type one.
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
