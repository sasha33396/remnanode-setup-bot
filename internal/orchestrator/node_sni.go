package orchestrator

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"

	"remnanode-setup-bot/internal/provisioner"
	sshclient "remnanode-setup-bot/internal/ssh"
)

var ErrNodeSNISwitchFailed = errors.New("Node xray-sni switch failed")

type passwordSSHConnector interface {
	ConnectInitial(context.Context, string, *sshclient.InitialCredentials) (*sshclient.Connection, error)
}

type PasswordNodeSNISwitcher struct {
	ssh        passwordSSHConnector
	repository string
	ref        string
	timeout    time.Duration
}

func NewPasswordNodeSNISwitcher(ssh passwordSSHConnector, repository, ref string, timeout time.Duration) (*PasswordNodeSNISwitcher, error) {
	if ssh == nil || strings.TrimSpace(repository) == "" || strings.TrimSpace(ref) == "" || timeout <= 0 {
		return nil, ErrInvalidInput
	}
	return &PasswordNodeSNISwitcher{ssh: ssh, repository: repository, ref: ref, timeout: timeout}, nil
}

func (s *PasswordNodeSNISwitcher) SwitchNodeSNI(ctx context.Context, input NodeSNISwitchInput) error {
	address := input.Address.Unmap()
	if !address.IsValid() || !publicIP(address) || strings.TrimSpace(input.PreviousSNI) == "" || strings.TrimSpace(input.TargetSNI) == "" || len(input.Password) == 0 || len(input.PreviousCertificate.FullchainPEM) == 0 || len(input.PreviousCertificate.PrivateKeyPEM) == 0 || len(input.Certificate.FullchainPEM) == 0 || len(input.Certificate.PrivateKeyPEM) == 0 {
		return ErrInvalidInput
	}
	credentials := sshclient.NewInitialCredentials(address, "root", input.Password)
	defer credentials.Clear()
	connection, err := s.ssh.ConnectInitial(ctx, "node-sni:"+address.String(), credentials)
	if err != nil {
		return ErrNodeSNISwitchFailed
	}
	defer connection.Close()
	installType, err := detectXraySNIInstallType(ctx, connection, s.timeout)
	if err != nil {
		return ErrNodeSNISwitchFailed
	}
	if installType == "legacy" {
		if err := provisioner.SwitchLegacyXraySNI(ctx, connection, input.PreviousSNI, input.TargetSNI, s.timeout); err != nil {
			return ErrNodeSNISwitchFailed
		}
		return nil
	}
	current, err := provisioner.NewExternalXraySNIInstaller(connection, provisioner.ExternalXraySNIConfig{
		RepositoryURL: s.repository,
		Ref:           s.ref,
		SNIDomain:     input.PreviousSNI,
		Timeout:       s.timeout,
	}, input.PreviousCertificate)
	if err != nil {
		return ErrNodeSNISwitchFailed
	}
	if err := current.Validate(ctx); err != nil {
		current.Destroy()
		return ErrNodeSNISwitchFailed
	}
	current.Destroy()
	installer, err := provisioner.NewExternalXraySNIInstaller(connection, provisioner.ExternalXraySNIConfig{
		RepositoryURL: s.repository,
		Ref:           s.ref,
		SNIDomain:     input.TargetSNI,
		Timeout:       s.timeout,
	}, input.Certificate)
	if err != nil {
		return ErrNodeSNISwitchFailed
	}
	defer installer.Destroy()
	if err := installer.SwitchSNI(ctx, input.PreviousSNI); err != nil {
		return ErrNodeSNISwitchFailed
	}
	return nil
}

func detectXraySNIInstallType(ctx context.Context, runner sshclient.CommandRunner, timeout time.Duration) (string, error) {
	result, err := runner.Run(ctx, sshclient.CommandRequest{Command: `# xray-sni:detect-install
if [ -f /opt/xray-sni/docker-compose.yml ] || [ -f /opt/xray-sni/compose.yml ]; then
    printf managed
elif [ -f /root/xray-sni/docker-compose.yml ] || [ -f /root/xray-sni/compose.yml ]; then
    printf legacy
else
    exit 1
fi`, Timeout: timeout})
	if err != nil {
		return "", err
	}
	installType := strings.TrimSpace(result.Stdout)
	if installType != "managed" && installType != "legacy" {
		return "", ErrNodeSNISwitchFailed
	}
	return installType, nil
}

func nodeAddress(value string) (netip.Addr, error) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || !publicIP(address) {
		return netip.Addr{}, ErrInvalidInput
	}
	return address.Unmap(), nil
}

var _ NodeSNISwitcher = (*PasswordNodeSNISwitcher)(nil)
