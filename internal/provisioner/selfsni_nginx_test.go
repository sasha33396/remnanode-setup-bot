package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	sshclient "remnanode-setup-bot/internal/ssh"
)

type selfSNIRunner struct {
	requests []sshclient.CommandRequest
}

func (r *selfSNIRunner) Run(_ context.Context, request sshclient.CommandRequest) (sshclient.CommandResult, error) {
	request.Stdin = append([]byte(nil), request.Stdin...)
	r.requests = append(r.requests, request)
	if strings.Contains(request.Command, "# selfsni:inspect") {
		return sshclient.CommandResult{Stdout: "ready"}, nil
	}
	return sshclient.CommandResult{}, nil
}

func TestSelfSNINginxInstallerKeepsCloudflareTokenOutOfCommand(t *testing.T) {
	runner := &selfSNIRunner{}
	token := []byte("top-secret-cloudflare-token")
	installer, err := NewSelfSNINginxInstaller(runner, SelfSNINginxConfig{
		CertificateDomain: "entry.example.com", Email: "ops@example.net", CloudflareToken: token, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer installer.Destroy()
	clear(token)
	if err := installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("requests = %d", len(runner.requests))
	}
	request := runner.requests[0]
	if strings.Contains(request.Command, "top-secret-cloudflare-token") || !strings.Contains(string(request.Stdin), "top-secret-cloudflare-token") {
		t.Fatal("Cloudflare token was not isolated to SSH stdin")
	}
	for _, marker := range []string{"python3-certbot-dns-cloudflare", "127.0.0.1:9443 ssl proxy_protocol", "--dns-cloudflare", "certbot.timer", "reality-live-fallback", "Technology that moves forward.", `<span class="pill">LIVE</span>`, `background:radial-gradient`, `chmod 0644 "$base/site/index.html"`} {
		if !strings.Contains(request.Command, marker) {
			t.Errorf("installer command is missing %q", marker)
		}
	}
	if strings.Contains(request.Command, "<h1>Welcome</h1>") || strings.Contains(request.Command, "The service is online.") {
		t.Fatal("installer still contains the simplified placeholder page")
	}
	if strings.Contains(request.Command, "%!") {
		t.Fatal("placeholder CSS was corrupted while formatting the remote command")
	}
	if !strings.Contains(request.Command, "domain='entry.example.com'") || strings.Contains(request.Command, "cover.example.net") {
		t.Fatal("installer did not keep the certificate Host domain separate from Reality serverNames")
	}
	if strings.Contains(installer.String(), "top-secret") || strings.Contains(installer.GoString(), "top-secret") {
		t.Fatal("installer formatting exposed the Cloudflare token")
	}
}

func TestSelfSNINginxInstallerRejectsInvalidConfiguration(t *testing.T) {
	runner := &selfSNIRunner{}
	for _, config := range []SelfSNINginxConfig{
		{CertificateDomain: "invalid", Email: "ops@example.net", CloudflareToken: []byte("token"), Timeout: time.Minute},
		{CertificateDomain: "entry.example.com", Email: "invalid", CloudflareToken: []byte("token"), Timeout: time.Minute},
		{CertificateDomain: "entry.example.com", Email: "ops@example.net", Timeout: time.Minute},
	} {
		if _, err := NewSelfSNINginxInstaller(runner, config); err == nil {
			t.Fatalf("accepted invalid configuration %#v", config)
		}
	}
}

func TestSelfSNINginxInspectRejectsUnreadableOrLegacyPlaceholder(t *testing.T) {
	runner := &selfSNIRunner{}
	installer, err := NewSelfSNINginxInstaller(runner, SelfSNINginxConfig{
		CertificateDomain: "entry.example.com", Email: "ops@example.net", CloudflareToken: []byte("token"), Timeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer installer.Destroy()

	if _, err := installer.Inspect(context.Background()); err != nil {
		t.Fatal(err)
	}
	command := runner.requests[0].Command
	for _, marker := range []string{`stat -c '%a'`, "= 644", "Technology that moves forward."} {
		if !strings.Contains(command, marker) {
			t.Errorf("inspection command is missing %q", marker)
		}
	}
}
