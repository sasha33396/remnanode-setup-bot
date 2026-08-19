package provisioner

import (
	"context"
	"fmt"
	"strings"
	"time"

	sshclient "remnanode-setup-bot/internal/ssh"
)

const legacyXraySNIInstallDirectory = "/root/xray-sni"

// SwitchLegacyXraySNI switches a locklance/xray-sni installation that lets
// Caddy manage its certificate through the existing CF_API_TOKEN.
func SwitchLegacyXraySNI(ctx context.Context, runner sshclient.CommandRunner, previousDomain, targetDomain string, timeout time.Duration) error {
	previousDomain = strings.TrimSpace(previousDomain)
	targetDomain = strings.TrimSpace(targetDomain)
	if runner == nil || timeout <= 0 || !validSNIDomain(previousDomain) || !validSNIDomain(targetDomain) || strings.EqualFold(previousDomain, targetDomain) {
		return ErrInvalidXraySNIConfiguration
	}
	oldDomain, _ := shellQuote(previousDomain)
	newDomain, _ := shellQuote(targetDomain)
	command := fmt.Sprintf(`# xray-sni:switch-legacy-sni
set -eu
cd /root/xray-sni
test -f .env
test -f Caddyfile
test -f docker-compose.yml || test -f compose.yml
backup=.env.sni-switch-previous
test ! -e "$backup"
cp -p .env "$backup"
committed=false
rollback() {
    if [ -f "$backup" ]; then
        mv -f "$backup" .env
        docker compose up -d --remove-orphans >/dev/null
        old_domain=%s
        old_code=$(curl --silent --show-error --output /dev/null --write-out '%%{http_code}' --resolve "$old_domain:%d:127.0.0.1" "https://$old_domain:%d/health" || true)
        test "$old_code" = 200
    fi
}
trap 'if [ "$committed" != true ]; then rollback || true; fi' EXIT HUP INT TERM
new_domain=%s
temporary=$(mktemp .env.sni-new.XXXXXX)
awk -v domain="$new_domain" '
BEGIN { found=0 }
/^SNI_DOMAIN=/ { print "SNI_DOMAIN=\"" domain "\""; found=1; next }
{ print }
END { if (!found) print "SNI_DOMAIN=\"" domain "\"" }
' .env > "$temporary"
chmod --reference=.env "$temporary"
chown --reference=.env "$temporary"
mv -f "$temporary" .env
docker compose config >/dev/null
docker compose up -d --remove-orphans >/dev/null
new_code=000
attempt=0
while [ "$attempt" -lt 30 ]; do
    new_code=$(curl --silent --show-error --output /dev/null --write-out '%%{http_code}' --resolve "$new_domain:%d:127.0.0.1" "https://$new_domain:%d/health" || true)
    [ "$new_code" = 200 ] && break
    attempt=$((attempt + 1))
    sleep 5
done
test "$new_code" = 200
rm -f "$backup"
committed=true
trap - EXIT HUP INT TERM`, oldDomain, xraySNIPort, xraySNIPort, newDomain, xraySNIPort, xraySNIPort)
	_, err := runner.Run(ctx, sshclient.CommandRequest{Command: command, Timeout: timeout})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrXraySNIValidationFailed
	}
	return nil
}
