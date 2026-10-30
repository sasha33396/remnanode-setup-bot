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

type SSHProvisionerConfig struct {
	RemnawaveAPIIP netip.Addr
	MetricsIP      netip.Addr
	Preflight      provisioner.Requirements
	XrayRepository string
	XrayRef        string
	CommandTimeout time.Duration
}

// SSHProvisioner connects the orchestration VPS port to the verified SSH,
// preflight and idempotent provisioner implementations.
type SSHProvisioner struct {
	ssh    *sshclient.Client
	store  provisioner.StepStore
	config SSHProvisionerConfig
}

// provisioningPhaseError carries only a predefined operator-safe phase. The
// protected SSH/provisioner error is deliberately not retained or exposed.
type provisioningPhaseError struct{ message string }

func (e *provisioningPhaseError) Error() string { return e.message }

func provisioningPhase(message string) error { return &provisioningPhaseError{message: message} }

func safeProvisioningMessage(err error) string {
	var phase *provisioningPhaseError
	if errors.As(err, &phase) && strings.TrimSpace(phase.message) != "" {
		return phase.message
	}
	return "VPS provisioning failed"
}

type preflightPhaseError struct {
	code    string
	message string
}

func (e *preflightPhaseError) Error() string { return e.message }

func preflightFailure(code, message string) error {
	return &preflightPhaseError{code: code, message: message}
}

func safePreflightFailure(err error) (string, string) {
	var phase *preflightPhaseError
	if errors.As(err, &phase) && strings.TrimSpace(phase.code) != "" && strings.TrimSpace(phase.message) != "" {
		return phase.code, phase.message
	}
	return "PREFLIGHT_FAILED", "Не удалось завершить предварительную проверку VPS"
}

func safeInitialSSHFailure(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return preflightFailure("SSH_CONNECTION_TIMEOUT", "Истекло время подключения к SSH. Проверьте доступность TCP/22")
	}
	detail := strings.ToLower(err.Error())
	if strings.Contains(detail, "unable to authenticate") || strings.Contains(detail, "no supported methods remain") {
		return preflightFailure("SSH_AUTHENTICATION_FAILED", "SSH доступен, но пароль root отклонён или PasswordAuthentication отключён")
	}
	if strings.Contains(detail, "host key") || strings.Contains(detail, "fingerprint") {
		return preflightFailure("SSH_HOST_KEY_VERIFICATION_FAILED", "Не удалось проверить ключ SSH-хоста; проверьте, не был ли сервер переустановлен")
	}
	return preflightFailure("SSH_CONNECTION_FAILED", "Не удалось подключиться к SSH. Проверьте IP и доступность TCP/22")
}

func NewSSHProvisioner(ssh *sshclient.Client, store provisioner.StepStore, config SSHProvisionerConfig) (*SSHProvisioner, error) {
	if ssh == nil || store == nil || !config.RemnawaveAPIIP.IsValid() || !config.MetricsIP.IsValid() || strings.TrimSpace(config.XrayRepository) == "" || strings.TrimSpace(config.XrayRef) == "" {
		return nil, errors.New("invalid SSH provisioner configuration")
	}
	if config.CommandTimeout <= 0 {
		config.CommandTimeout = 5 * time.Minute
	}
	return &SSHProvisioner{ssh: ssh, store: store, config: config}, nil
}

func (v *SSHProvisioner) Preflight(ctx context.Context, input PreflightVPSInput) (provisioner.PreflightResult, error) {
	if strings.TrimSpace(input.DeploymentID) == "" || !input.Address.IsValid() || len(input.Password) == 0 {
		return provisioner.PreflightResult{}, ErrInvalidInput
	}
	credentials := sshclient.NewInitialCredentials(input.Address, "root", input.Password)
	defer credentials.Clear()
	connection, err := v.ssh.ConnectInitial(ctx, input.DeploymentID, credentials)
	if err != nil {
		return provisioner.PreflightResult{}, safeInitialSSHFailure(ctx, err)
	}
	defer connection.Close()
	requirements := v.config.Preflight
	requirements.AllowExistingInstallation = input.AllowExistingInstallation
	check, err := provisioner.NewPreflight(connection, requirements)
	if err != nil {
		return provisioner.PreflightResult{}, preflightFailure("PREFLIGHT_SETUP_FAILED", "Не удалось подготовить проверку VPS")
	}
	result, err := check.Run(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return result, preflightFailure("PREFLIGHT_TIMEOUT", "Проверка VPS по SSH превысила допустимое время")
		}
		return result, preflightFailure("PREFLIGHT_COMMAND_FAILED", "Не удалось выполнить проверочные команды на VPS")
	}
	if !result.Passed() {
		return result, nil
	}
	if err := v.ssh.InstallDeploymentPublicKey(ctx, connection); err != nil {
		return provisioner.PreflightResult{}, preflightFailure("DEPLOYMENT_KEY_INSTALL_FAILED", "SSH доступен, но не удалось установить deploy-key в /root/.ssh/authorized_keys")
	}
	return result, nil
}

func (v *SSHProvisioner) Provision(ctx context.Context, input ProvisionVPSInput, progress func(provisioner.Report)) error {
	if strings.TrimSpace(input.DeploymentID) == "" || !input.Address.IsValid() || strings.TrimSpace(input.SNIDomain) == "" || len(input.RemnanodeSecretKey) == 0 || len(input.Certificate.FullchainPEM) == 0 || len(input.Certificate.PrivateKeyPEM) == 0 {
		return ErrInvalidInput
	}
	connection, err := v.ssh.ConnectWithDeploymentKey(ctx, input.DeploymentID, input.Address, "root")
	if err != nil {
		return provisioningPhase("Deployment-key SSH connection failed")
	}
	defer connection.Close()

	config, err := provisioner.NewConfig(v.config.RemnawaveAPIIP, v.config.MetricsIP, input.RemnanodeSecretKey)
	if err != nil {
		return provisioningPhase("Provisioner configuration could not be created")
	}
	defer config.Destroy()
	xray, err := provisioner.NewExternalXraySNIInstaller(connection, provisioner.ExternalXraySNIConfig{
		RepositoryURL: v.config.XrayRepository,
		Ref:           v.config.XrayRef,
		SNIDomain:     input.SNIDomain,
		Timeout:       v.config.CommandTimeout,
	}, input.Certificate)
	if err != nil {
		return provisioningPhase("xray-sni adapter could not be initialized")
	}
	defer xray.Destroy()
	stages, err := provisioner.NewDefaultStages(connection, config, xray)
	if err != nil {
		return provisioningPhase("Provisioning stages could not be created")
	}
	engine, err := provisioner.NewEngine(v.store, stages)
	if err != nil {
		return provisioningPhase("Provisioner engine could not be initialized")
	}
	_, err = engine.RunWithProgress(ctx, input.DeploymentID, progress)
	if err != nil {
		return provisioningPhase("Provisioner engine failed; inspect stage logs")
	}
	return nil
}

var _ VPSOperator = (*SSHProvisioner)(nil)
