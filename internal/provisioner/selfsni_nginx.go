package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	sshclient "remnanode-setup-bot/internal/ssh"
)

var (
	ErrInvalidSelfSNIConfiguration = errors.New("invalid SelfSNI configuration")
	ErrSelfSNIInspectionFailed     = errors.New("SelfSNI inspection failed")
	ErrSelfSNIInstallationFailed   = errors.New("SelfSNI installation failed")
	ErrSelfSNIValidationFailed     = errors.New("SelfSNI validation failed")
)

type SelfSNINginxConfig struct {
	CertificateDomain string
	Email             string
	CloudflareToken   []byte
	Timeout           time.Duration
}

type SelfSNINginxInstaller struct {
	runner sshclient.CommandRunner
	config SelfSNINginxConfig
}

func NewSelfSNINginxInstaller(runner sshclient.CommandRunner, config SelfSNINginxConfig) (*SelfSNINginxInstaller, error) {
	config.CertificateDomain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(config.CertificateDomain), "."))
	config.Email = strings.TrimSpace(config.Email)
	if runner == nil || !validSelfSNIDomain(config.CertificateDomain) || len(config.CloudflareToken) == 0 || config.Timeout <= 0 {
		return nil, ErrInvalidSelfSNIConfiguration
	}
	address, err := mail.ParseAddress(config.Email)
	if err != nil || !strings.EqualFold(address.Address, config.Email) {
		return nil, ErrInvalidSelfSNIConfiguration
	}
	config.CloudflareToken = append([]byte(nil), config.CloudflareToken...)
	return &SelfSNINginxInstaller{runner: runner, config: config}, nil
}

func (a *SelfSNINginxInstaller) Destroy() {
	clear(a.config.CloudflareToken)
	a.config.CloudflareToken = nil
}

func (*SelfSNINginxInstaller) String() string {
	return "SelfSNINginxInstaller{CloudflareToken:[REDACTED]}"
}
func (*SelfSNINginxInstaller) GoString() string {
	return "SelfSNINginxInstaller{CloudflareToken:[REDACTED]}"
}

func (a *SelfSNINginxInstaller) Inspect(ctx context.Context) (Inspection, error) {
	domain, _ := shellQuote(a.config.CertificateDomain)
	tokenHash := sha256.Sum256(a.config.CloudflareToken)
	hash, _ := shellQuote(hex.EncodeToString(tokenHash[:]))
	command := fmt.Sprintf(`# selfsni:inspect
set -eu
domain=%s
expected_hash=%s
ready=true
command -v certbot >/dev/null 2>&1 || ready=false
systemctl is-enabled --quiet certbot.timer || ready=false
test "$(cat /opt/reality-live-fallback/.selfsni-domain 2>/dev/null || true)" = "$domain" || ready=false
test "$(cat /opt/reality-live-fallback/.cloudflare-token.sha256 2>/dev/null || true)" = "$expected_hash" || ready=false
test -s "/etc/letsencrypt/live/$domain/fullchain.pem" || ready=false
test -s "/etc/letsencrypt/live/$domain/privkey.pem" || ready=false
openssl x509 -checkend 86400 -noout -in "/etc/letsencrypt/live/$domain/fullchain.pem" >/dev/null 2>&1 || ready=false
test "$(stat -c '%%a' /opt/reality-live-fallback/site/index.html 2>/dev/null || true)" = 644 || ready=false
grep -Fq 'Technology that moves forward.' /opt/reality-live-fallback/site/index.html 2>/dev/null || ready=false
docker inspect -f '{{.State.Running}}' reality-live-fallback 2>/dev/null | grep -qx true || ready=false
if [ "$ready" = true ]; then printf ready; else printf pending; fi`, domain, hash)
	result, err := a.runner.Run(ctx, sshclient.CommandRequest{Command: command, Timeout: a.config.Timeout})
	if err != nil {
		return Inspection{}, ErrSelfSNIInspectionFailed
	}
	return Inspection{Satisfied: strings.TrimSpace(string(result.Stdout)) == "ready", Summary: "SelfSNI Certbot and NGINX are configured"}, nil
}

func (a *SelfSNINginxInstaller) Install(ctx context.Context) error {
	domain, _ := shellQuote(a.config.CertificateDomain)
	email, _ := shellQuote(a.config.Email)
	tokenHash := sha256.Sum256(a.config.CloudflareToken)
	hash, _ := shellQuote(hex.EncodeToString(tokenHash[:]))
	command := fmt.Sprintf(`# selfsni:install
set -eu
domain=%s
email=%s
token_hash=%s
IFS= read -r cf_token
test -n "$cf_token"
command -v docker >/dev/null 2>&1
docker info >/dev/null 2>&1
base=/opt/reality-live-fallback
if docker container inspect reality-live-fallback >/dev/null 2>&1; then
    mounts="$(docker inspect -f '{{range .Mounts}}{{printf "%%s:%%s\n" .Source .Destination}}{{end}}' reality-live-fallback)"
    printf '%%s\n' "$mounts" | grep -Fxq "$base:/etc/nginx/custom"
    printf '%%s\n' "$mounts" | grep -Fxq '/etc/letsencrypt:/etc/letsencrypt'
    test "$(docker inspect -f '{{.HostConfig.NetworkMode}}' reality-live-fallback)" = host
elif command -v ss >/dev/null 2>&1 && ss -H -lnt 'sport = :9443' | grep -q .; then
    exit 18
fi
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y certbot python3-certbot-dns-cloudflare ca-certificates
certbot plugins 2>&1 | grep -q dns-cloudflare
credentials="/root/.secrets/certbot/cloudflare-$domain.ini"
install -d -m 0755 "$base/site"
install -d -m 0700 /root/.secrets/certbot
umask 077
printf 'dns_cloudflare_api_token = %%s\n' "$cf_token" > "$credentials"
chmod 0600 "$credentials"
printf '%%s\n' "$domain" > "$base/.selfsni-domain"
printf '%%s\n' "$token_hash" > "$base/.cloudflare-token.sha256"
chmod 0600 "$base/.selfsni-domain" "$base/.cloudflare-token.sha256"
unset cf_token

cat > "$base/site/index.html" <<HTML
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="theme-color" content="#0b1220">
  <meta name="description" content="$domain — digital services and infrastructure">
  <title>$domain</title>
  <style>
    * { box-sizing: border-box; }
    body { margin:0; min-height:100vh; display:grid; place-items:center; padding:24px;
      background:radial-gradient(circle at 80%% 15%%,#17325a 0,transparent 45%%),#0b1220;
      color:#eef4ff; font:16px/1.65 system-ui,-apple-system,Segoe UI,sans-serif; }
    main { width:min(790px,100%%); padding:clamp(28px,6vw,64px); background:#131f33e6;
      border:1px solid #30435e; border-radius:24px; box-shadow:0 24px 80px #0005; }
    header { display:flex; justify-content:space-between; align-items:center; gap:18px;
      color:#a7cfff; font-weight:700; letter-spacing:.06em; }
    .pill { border:1px solid #375278; border-radius:999px; padding:5px 12px; font-size:.75rem; }
    h1 { font-size:clamp(2.3rem,6vw,3.6rem); line-height:1.12; letter-spacing:-.04em; margin:48px 0 20px; }
    p { max-width:58ch; color:#b9c9de; }
    .small { color:#7ea8dd; font-size:.85rem; margin-top:36px; overflow-wrap:anywhere; }
    footer { margin-top:50px; padding-top:18px; border-top:1px solid #30435e;
      font-size:.85rem; color:#8da4c3; }
  </style>
</head>
<body>
  <main>
    <header><span>$domain</span><span class="pill">LIVE</span></header>
    <h1>Technology that moves forward.</h1>
    <p>Simple, reliable digital experiences. Infrastructure and thoughtful engineering for a connected world.</p>
    <div class="small">$domain</div>
    <footer>&copy; $(date +%%Y) $domain</footer>
  </main>
</body>
</html>
HTML
chmod 0644 "$base/site/index.html"

policy=--keep-until-expiring
renewal="/etc/letsencrypt/renewal/$domain.conf"
if [ -f "$renewal" ] && ! grep -Eq '^authenticator = dns-cloudflare[[:space:]]*$' "$renewal"; then
    policy=--force-renewal
fi
certbot certonly --non-interactive --agree-tos --email "$email" \
    --dns-cloudflare --dns-cloudflare-credentials "$credentials" \
    --dns-cloudflare-propagation-seconds 90 --cert-name "$domain" -d "$domain" "$policy"

cat > "$base/nginx.conf" <<EOF
worker_processes auto;
events { worker_connections 4096; }
http {
    include /etc/nginx/mime.types;
    default_type application/octet-stream;
    server_tokens off;
    access_log /var/log/nginx/access.log;
    error_log /var/log/nginx/error.log warn;
    server {
        listen 127.0.0.1:9443 ssl proxy_protocol default_server;
        http2 on;
        server_name $domain;
        ssl_certificate /etc/letsencrypt/live/$domain/fullchain.pem;
        ssl_certificate_key /etc/letsencrypt/live/$domain/privkey.pem;
        ssl_protocols TLSv1.3;
        ssl_session_cache shared:SSL:10m;
        ssl_session_timeout 10m;
        root /etc/nginx/custom/site;
        index index.html;
        add_header X-Content-Type-Options nosniff always;
        location = /health { access_log off; default_type text/plain; return 200 'ok'; }
        location / { try_files \$uri \$uri/ /index.html; }
        location ~ /\. { return 404; }
    }
}
EOF

cat > "$base/docker-compose.yml" <<'YAML'
services:
  nginx:
    image: nginx:stable-alpine
    container_name: reality-live-fallback
    restart: unless-stopped
    network_mode: host
    volumes:
      - .:/etc/nginx/custom:ro
      - /etc/letsencrypt:/etc/letsencrypt:ro
    command: ["nginx", "-c", "/etc/nginx/custom/nginx.conf", "-g", "daemon off;"]
YAML

install -d -m 0755 /etc/letsencrypt/renewal-hooks/deploy
cat > /etc/letsencrypt/renewal-hooks/deploy/90-reality-live-fallback.sh <<HOOK
#!/bin/sh
set -eu
test "\${RENEWED_LINEAGE:-}" = "/etc/letsencrypt/live/$domain" || exit 0
docker exec reality-live-fallback nginx -t -c /etc/nginx/custom/nginx.conf
docker exec reality-live-fallback nginx -s reload -c /etc/nginx/custom/nginx.conf
HOOK
chmod 0755 /etc/letsencrypt/renewal-hooks/deploy/90-reality-live-fallback.sh
systemctl enable --now certbot.timer
cd "$base"
docker compose config >/dev/null
docker run --rm --network none -v "$base:/etc/nginx/custom:ro" -v /etc/letsencrypt:/etc/letsencrypt:ro nginx:stable-alpine nginx -t -c /etc/nginx/custom/nginx.conf
docker compose up -d --remove-orphans
`, domain, email, hash)
	stdin := append(append([]byte(nil), a.config.CloudflareToken...), '\n')
	defer clear(stdin)
	if _, err := a.runner.Run(ctx, sshclient.CommandRequest{Command: command, Stdin: stdin, Timeout: a.config.Timeout}); err != nil {
		return ErrSelfSNIInstallationFailed
	}
	return nil
}

func (a *SelfSNINginxInstaller) Validate(ctx context.Context) error {
	domain, _ := shellQuote(a.config.CertificateDomain)
	command := fmt.Sprintf(`# selfsni:validate
set -eu
domain=%s
cd /opt/reality-live-fallback
docker compose config >/dev/null
docker exec reality-live-fallback nginx -t -c /etc/nginx/custom/nginx.conf >/dev/null
systemctl is-active --quiet certbot.timer
openssl x509 -checkend 86400 -noout -in "/etc/letsencrypt/live/$domain/fullchain.pem"
openssl x509 -in "/etc/letsencrypt/live/$domain/fullchain.pem" -noout -ext subjectAltName | grep -Fq "DNS:$domain"
curl --fail --silent --show-error --max-time 15 --haproxy-protocol --resolve "$domain:9443:127.0.0.1" "https://$domain:9443/health" | grep -qx ok`, domain)
	if _, err := a.runner.Run(ctx, sshclient.CommandRequest{Command: command, Timeout: a.config.Timeout}); err != nil {
		return ErrSelfSNIValidationFailed
	}
	return nil
}

func validSelfSNIDomain(value string) bool {
	if len(value) < 4 || len(value) > 253 || !strings.Contains(value, ".") {
		return false
	}
	labels := strings.Split(value, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

var _ XraySNIInstaller = (*SelfSNINginxInstaller)(nil)
