package provisioner

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	sshclient "remnanode-setup-bot/internal/ssh"
)

const (
	siteThemeScriptPath           = "/usr/local/sbin/node-theme-rotate"
	siteThemeConfigPath           = "/etc/node-theme.conf"
	DefaultSiteThemeRepositoryURL = "https://github.com/Mrvibecodic/node-templates.git"
	DefaultSiteThemeRef           = "845187fbee8fff72f66d1570af436438e859e40d"
)

var (
	ErrInvalidSiteThemeConfiguration = errors.New("invalid site theme configuration")
	ErrSiteThemeInstallationFailed   = errors.New("site theme installation failed")
	ErrSiteThemeValidationFailed     = errors.New("site theme validation failed")
	ErrSiteThemeUnsafeContent        = errors.New("site theme contains unsafe symbolic links")
	ErrSiteThemeActivationFailed     = errors.New("site theme Caddy or HTTP activation failed")
)

type SiteThemeConfig struct {
	RepositoryURL  string
	Ref            string
	RotationEvery  time.Duration
	RotationJitter time.Duration
	CommandTimeout time.Duration
}

type SiteThemeManager struct {
	runner sshclient.CommandRunner
	config SiteThemeConfig
}

func DefaultSiteThemeConfig(commandTimeout time.Duration) SiteThemeConfig {
	return SiteThemeConfig{
		RepositoryURL:  DefaultSiteThemeRepositoryURL,
		Ref:            DefaultSiteThemeRef,
		RotationEvery:  72 * time.Hour,
		RotationJitter: 12 * time.Hour,
		CommandTimeout: commandTimeout,
	}
}

func NewSiteThemeManager(runner sshclient.CommandRunner, config SiteThemeConfig) (*SiteThemeManager, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.RepositoryURL))
	if runner == nil || err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !validXrayPinnedRef(config.Ref) || config.RotationEvery <= 0 || config.RotationJitter < 0 || config.RotationJitter >= config.RotationEvery {
		return nil, ErrInvalidSiteThemeConfiguration
	}
	if config.CommandTimeout <= 0 {
		config.CommandTimeout = 5 * time.Minute
	}
	return &SiteThemeManager{runner: runner, config: config}, nil
}

func (m *SiteThemeManager) Inspect(ctx context.Context) (bool, error) {
	repository, _ := shellQuote(m.config.RepositoryURL)
	ref, _ := shellQuote(m.config.Ref)
	command := fmt.Sprintf(`# node-theme:inspect
set -eu
test -x /usr/local/sbin/node-theme-rotate
test -s /etc/node-theme.conf
test -s /var/lib/node-theme/state
test -s /opt/xray-sni/html/index.html
test -s /opt/xray-sni/html/.uniquify-manifest.txt
expected=$(sed -n 's/^index_sha256=//p' /var/lib/node-theme/state | head -n1)
actual=$(sha256sum /opt/xray-sni/html/index.html | cut -d' ' -f1)
test -n "$expected"
test "$actual" = "$expected"
. /etc/node-theme.conf
test "$REPOSITORY_URL" = %s
test "$REPOSITORY_REF" = %s
test "$(sed -n 's/^source_commit=//p' /var/lib/node-theme/state | head -n1)" = %s
systemctl is-enabled --quiet node-theme-rotate.timer
systemctl is-active --quiet node-theme-rotate.timer`, repository, ref, ref)
	_, err := m.runner.Run(ctx, sshclient.CommandRequest{Command: command, Timeout: m.config.CommandTimeout})
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return false, nil
}

func (m *SiteThemeManager) Ensure(ctx context.Context) error {
	if err := m.installManagedFiles(ctx); err != nil {
		return err
	}
	return m.run(ctx, "--ensure")
}

func (m *SiteThemeManager) Rotate(ctx context.Context) error {
	if err := m.installManagedFiles(ctx); err != nil {
		return err
	}
	return m.rotate(ctx)
}

func (m *SiteThemeManager) rotate(ctx context.Context) error {
	repository, _ := shellQuote(m.config.RepositoryURL)
	ref, _ := shellQuote(m.config.Ref)
	command := fmt.Sprintf(`# node-theme:rotate
set -eu
if [ -d /opt/node-templates/.git ] &&
   [ "$(git -C /opt/node-templates remote get-url origin)" = %s ] &&
   [ "$(git -C /opt/node-templates rev-parse HEAD)" = %s ] &&
   [ -x /opt/node-templates/uniquify-theme.sh ]; then
  %s --rotate-template --offline
else
  %s --rotate-template
fi`, repository, ref, siteThemeScriptPath, siteThemeScriptPath)
	result, err := m.runner.Run(ctx, sshclient.CommandRequest{Command: command, Timeout: m.config.CommandTimeout})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if result.ExitStatus == 20 {
		return ErrSiteThemeUnsafeContent
	}
	if result.ExitStatus == 21 {
		return ErrSiteThemeActivationFailed
	}
	return ErrSiteThemeInstallationFailed
}

func (m *SiteThemeManager) Validate(ctx context.Context) error {
	ready, err := m.Inspect(ctx)
	if err != nil {
		return err
	}
	if !ready {
		return ErrSiteThemeValidationFailed
	}
	return nil
}

func (m *SiteThemeManager) run(ctx context.Context, mode string) error {
	command := "# node-theme:rotate\n" + siteThemeScriptPath + " " + mode
	_, err := m.runner.Run(ctx, sshclient.CommandRequest{Command: command, Timeout: m.config.CommandTimeout})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrSiteThemeInstallationFailed
}

func (m *SiteThemeManager) installManagedFiles(ctx context.Context) error {
	repository, _ := shellQuote(m.config.RepositoryURL)
	ref, _ := shellQuote(m.config.Ref)
	config := []byte(fmt.Sprintf("REPOSITORY_URL=%s\nREPOSITORY_REF=%s\n", repository, ref))
	timer := []byte(fmt.Sprintf(`[Unit]
Description=Rotate xray-sni static site fingerprints every three days

[Timer]
OnUnitActiveSec=%s
RandomizedDelaySec=%s
Persistent=true

[Install]
WantedBy=timers.target
`, systemdDuration(m.config.RotationEvery), systemdDuration(m.config.RotationJitter)))
	service := []byte(`[Unit]
Description=Rotate xray-sni static site fingerprints
After=docker.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/node-theme-rotate --rotate-seed --offline
`)
	remote := &remoteStage{runner: m.runner, timeout: m.config.CommandTimeout}
	files := []managedFile{
		{path: siteThemeScriptPath, mode: "0750", content: []byte(siteThemeScript)},
		{path: siteThemeConfigPath, mode: "0600", content: config},
		{path: "/etc/systemd/system/node-theme-rotate.service", mode: "0644", content: service},
		{path: "/etc/systemd/system/node-theme-rotate.timer", mode: "0644", content: timer},
	}
	for _, file := range files {
		if err := remote.writeFile(ctx, file); err != nil {
			return ErrSiteThemeInstallationFailed
		}
	}
	command := "# node-theme:timer\nsystemctl daemon-reload && systemctl enable --now node-theme-rotate.timer && systemctl restart node-theme-rotate.timer"
	if _, err := m.runner.Run(ctx, sshclient.CommandRequest{Command: command, Timeout: m.config.CommandTimeout}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrSiteThemeInstallationFailed
	}
	return nil
}

func systemdDuration(value time.Duration) string {
	seconds := int64(value / time.Second)
	return fmt.Sprintf("%ds", seconds)
}

const siteThemeScript = `#!/usr/bin/env bash
set -Eeuo pipefail
umask 022

MODE="${1:---ensure}"
OFFLINE="${2:-}"
. /etc/node-theme.conf

SOURCE=/opt/node-templates
XRAY=/opt/xray-sni
ACTIVE=$XRAY/html
DATA=/var/lib/node-theme
STATE=$DATA/state
PENDING=$DATA/pending
LOCK=/run/lock/node-theme-rotate.lock
BACKUP=$XRAY/.theme-previous
THEMES=(endless-verify esports-stream-template levelup-hub playza-game-catalog rybaliti-2.0 screenwire-digest vibrai-photo-editor worldzoo-stream-template)

mkdir -p "$DATA" /run/lock
exec 9>"$LOCK"
flock -n 9 || exit 0

if [ -d "$BACKUP" ]; then
  rm -rf "$ACTIVE"
  mv "$BACKUP" "$ACTIVE"
  cd "$XRAY"
  docker compose up -d --force-recreate caddy
fi
find "$XRAY" -maxdepth 1 -type d -name '.theme-staging.*' -exec rm -rf -- {} +

valid_theme() {
  local value="$1" item
  for item in "${THEMES[@]}"; do [ "$item" = "$value" ] && return 0; done
  return 1
}

sync_source() {
  if [ "$OFFLINE" = "--offline" ]; then
    test -d "$SOURCE/.git"
    test "$(git -C "$SOURCE" remote get-url origin)" = "$REPOSITORY_URL"
    test "$(git -C "$SOURCE" rev-parse HEAD)" = "$REPOSITORY_REF"
    test -x "$SOURCE/uniquify-theme.sh"
    return
  fi
  if [ ! -e "$SOURCE" ]; then git clone --no-checkout -- "$REPOSITORY_URL" "$SOURCE"; fi
  test -d "$SOURCE/.git"
  git -C "$SOURCE" remote set-url origin "$REPOSITORY_URL"
  git -C "$SOURCE" fetch --force origin "$REPOSITORY_REF"
  git -C "$SOURCE" checkout --detach --force "$REPOSITORY_REF"
  git -C "$SOURCE" clean -fdx
  test "$(git -C "$SOURCE" rev-parse HEAD)" = "$REPOSITORY_REF"
  test -x "$SOURCE/uniquify-theme.sh"
}

state_value() {
  if [ -f "$2" ]; then sed -n "s/^$1=//p" "$2" | head -n1; fi
  return 0
}
current_theme="$(state_value theme "$STATE")"

if [ "$MODE" = "--ensure" ] && [ "$(state_value source_commit "$STATE")" = "$REPOSITORY_REF" ] && [ -s "$ACTIVE/index.html" ] && [ -s "$ACTIVE/.uniquify-manifest.txt" ]; then
  expected="$(state_value index_sha256 "$STATE")"
  actual="$(sha256sum "$ACTIVE/index.html" | cut -d' ' -f1)"
  if [ -n "$expected" ] && [ "$expected" = "$actual" ]; then exit 0; fi
fi

sync_source

pending_mode="$(state_value mode "$PENDING")"
theme="$(state_value theme "$PENDING")"
seed="$(state_value seed "$PENDING")"
if [ "$pending_mode" != "$MODE" ] || ! valid_theme "$theme" || ! [[ "$seed" =~ ^[a-f0-9]{32}$ ]]; then
  if [ "$MODE" = "--rotate-seed" ] && valid_theme "$current_theme"; then
    theme="$current_theme"
  else
    candidates=()
    for item in "${THEMES[@]}"; do
      [ "$MODE" = "--rotate-template" ] && [ "$item" = "$current_theme" ] && continue
      [ -d "$SOURCE/$item" ] && candidates+=("$item")
    done
    [ "${#candidates[@]}" -gt 0 ]
    theme="${candidates[$((16#$(od -An -N2 -tx2 /dev/urandom | tr -d ' ') % ${#candidates[@]}))]}"
  fi
  seed="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
  temporary=$(mktemp "$DATA/pending.XXXXXX")
  printf 'mode=%s\ntheme=%s\nseed=%s\n' "$MODE" "$theme" "$seed" > "$temporary"
  chmod 0600 "$temporary"
  mv -f "$temporary" "$PENDING"
fi

valid_theme "$theme"
test -d "$SOURCE/$theme"
staging=$(mktemp -d "$XRAY/.theme-staging.XXXXXX")
cleanup() { rm -rf "$staging"; }
trap cleanup EXIT

bash "$SOURCE/uniquify-theme.sh" --src "$SOURCE/$theme" --seed "$seed" --no-install-deps --no-zip -o "$staging/site"
test -s "$staging/site/index.html"
test -s "$staging/site/.uniquify-manifest.txt"
if find "$staging/site" -type l | grep -q .; then exit 20; fi
find "$staging/site" -type d -exec chmod 0755 {} +
find "$staging/site" -type f -exec chmod 0644 {} +
if [ ! -f "$staging/site/robots.txt" ]; then printf 'User-agent: *\nDisallow: /\n' > "$staging/site/robots.txt"; fi

rm -rf "$BACKUP"
had_active=false
if [ -d "$ACTIVE" ]; then mv "$ACTIVE" "$BACKUP"; had_active=true; fi
mv "$staging/site" "$ACTIVE"

activate() {
  cd "$XRAY"
  docker compose up -d --force-recreate caddy
  docker exec snisite caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
  expected_hash=$(sha256sum "$ACTIVE/index.html" | cut -d' ' -f1)
  container_theme_path=$(docker inspect snisite --format '{{range .Mounts}}{{if eq .Source "/opt/xray-sni/html"}}{{.Destination}}{{end}}{{end}}')
  test -n "$container_theme_path"
  ready=false
  for attempt in $(seq 1 15); do
    running=$(docker inspect snisite --format '{{.State.Running}}' 2>/dev/null || true)
    container_hash=$(docker exec snisite sha256sum "$container_theme_path/index.html" 2>/dev/null | cut -d' ' -f1 || true)
    if [ "$running" = true ] && [ "$container_hash" = "$expected_hash" ]; then
      ready=true
      break
    fi
    sleep 2
  done
  [ "$ready" = true ]
}

if ! activate; then
  rm -rf "$ACTIVE"
  if [ "$had_active" = true ]; then mv "$BACKUP" "$ACTIVE"; cd "$XRAY"; docker compose up -d --force-recreate caddy || true; fi
  exit 21
fi
rm -rf "$BACKUP"
index_hash=$(sha256sum "$ACTIVE/index.html" | cut -d' ' -f1)
manifest_hash=$(sha256sum "$ACTIVE/.uniquify-manifest.txt" | cut -d' ' -f1)
temporary=$(mktemp "$DATA/state.XXXXXX")
printf 'theme=%s\nseed=%s\nsource_commit=%s\nindex_sha256=%s\nmanifest_sha256=%s\nactivated_at=%s\n' "$theme" "$seed" "$REPOSITORY_REF" "$index_hash" "$manifest_hash" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$temporary"
chmod 0644 "$temporary"
mv -f "$temporary" "$STATE"
rm -f "$PENDING"
`
