package provisioner

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSiteThemeManagerInstallsTimerAndRotatesTemplate(t *testing.T) {
	runner := &captureRunner{}
	manager, err := NewSiteThemeManager(runner, DefaultSiteThemeConfig(4*time.Minute))
	if err != nil {
		t.Fatalf("NewSiteThemeManager() error = %v", err)
	}
	if err := manager.Rotate(context.Background()); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	joined := ""
	for _, request := range runner.requests {
		joined += request.Command + "\n" + string(request.Stdin) + "\n"
	}
	for _, required := range []string{
		"target='/usr/local/sbin/node-theme-rotate'",
		"target='/etc/node-theme.conf'",
		"OnUnitActiveSec=259200s",
		"RandomizedDelaySec=43200s",
		"ExecStart=/usr/local/sbin/node-theme-rotate --rotate-seed --offline",
		"systemctl enable --now node-theme-rotate.timer",
		"/usr/local/sbin/node-theme-rotate --rotate-template --offline",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("managed commands do not contain %q", required)
		}
	}
}

func TestSiteThemeScriptBashSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	command := exec.Command("bash", "-n")
	command.Stdin = strings.NewReader(siteThemeScript)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("bash -n failed: %v: %s", err, output)
	}
}

func TestSiteThemeScriptUsesAtomicRollbackAndPinnedSource(t *testing.T) {
	for _, required := range []string{
		`git -C "$SOURCE" checkout --detach --force "$REPOSITORY_REF"`,
		`bash "$SOURCE/uniquify-theme.sh" --src "$SOURCE/$theme" --seed "$seed" --no-install-deps --no-zip`,
		`if [ "$MODE" = "--rotate-seed" ] && valid_theme "$current_theme"`,
		`for attempt in $(seq 1 15)`,
		`[ "$ready" = true ]`,
		`container_theme_path=$(docker inspect snisite`,
		`container_hash=$(docker exec snisite sha256sum`,
		`[ "$container_hash" = "$expected_hash" ]`,
		`if ! activate; then`,
		`mv "$BACKUP" "$ACTIVE"`,
	} {
		if !strings.Contains(siteThemeScript, required) {
			t.Fatalf("rotation script does not contain %q", required)
		}
	}
	if strings.Contains(siteThemeScript, `--cacert certs/fullchain.pem`) {
		t.Fatal("theme activation must not fail because the independent TLS certificate is expired")
	}
}

func TestSiteThemeManagerRejectsMovingRefAndUnsafeRepository(t *testing.T) {
	config := DefaultSiteThemeConfig(time.Minute)
	config.Ref = "main"
	if _, err := NewSiteThemeManager(&captureRunner{}, config); err == nil {
		t.Fatal("moving ref was accepted")
	}
	config = DefaultSiteThemeConfig(time.Minute)
	config.RepositoryURL = "https://user:secret@example.com/themes.git"
	if _, err := NewSiteThemeManager(&captureRunner{}, config); err == nil {
		t.Fatal("credential-bearing repository URL was accepted")
	}
}
