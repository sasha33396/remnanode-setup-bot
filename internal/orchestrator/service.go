package orchestrator

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"remnanode-setup-bot/internal/certmanager"
	"remnanode-setup-bot/internal/deployment"
	"remnanode-setup-bot/internal/provisioner"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/repository"
)

const (
	stepPreflight          = "preflight"
	stepPrepareCertificate = "prepare_certificate"
	stepProvisioning       = "provisioning"
	stepCreateNode         = "create_remnawave_node"
	stepWaitNode           = "wait_remnawave"
	stepAddDNS             = "add_dns"
	workflowStepCount      = 6
)

type execution struct {
	cancel context.CancelFunc
}

// DeploymentService owns ordering, durable transitions, idempotent resume and
// bounded concurrency for deployment workflows.
type DeploymentService struct {
	repository   DeploymentRepository
	remnawave    RemnawaveAPI
	dns          DNSAPI
	certificates CertificateProvider
	vps          VPSOperator
	config       Config
	semaphore    chan struct{}

	mu      sync.Mutex
	running map[string]*execution
}

func NewDeploymentService(repository DeploymentRepository, remnawaveAPI RemnawaveAPI, dns DNSAPI, certificateProvider CertificateProvider, vps VPSOperator, config Config) (*DeploymentService, error) {
	if repository == nil || remnawaveAPI == nil || dns == nil || certificateProvider == nil || vps == nil {
		return nil, errors.New("deployment service dependencies are required")
	}
	if strings.TrimSpace(config.PanelID) == "" {
		config.PanelID = "default"
	}
	if config.MaxConcurrentDeployments <= 0 {
		config.MaxConcurrentDeployments = 2
	}
	if config.NodeConnectTimeout <= 0 {
		config.NodeConnectTimeout = 5 * time.Minute
	}
	if config.InitialPollBackoff <= 0 {
		config.InitialPollBackoff = time.Second
	}
	if config.MaxPollBackoff <= 0 {
		config.MaxPollBackoff = 10 * time.Second
	}
	if config.InitialPollBackoff > config.MaxPollBackoff || config.MaxConcurrentDeployments > 100 {
		return nil, errors.New("invalid deployment service configuration")
	}
	return &DeploymentService{
		repository:   repository,
		remnawave:    remnawaveAPI,
		dns:          dns,
		certificates: certificateProvider,
		vps:          vps,
		config:       config,
		semaphore:    make(chan struct{}, config.MaxConcurrentDeployments),
		running:      make(map[string]*execution),
	}, nil
}

// Prepare validates immutable operator input, creates the durable deployment,
// and performs preflight before confirmation is shown.
func (s *DeploymentService) Prepare(ctx context.Context, input PrepareInput) (PreparedDeployment, error) {
	if (strings.TrimSpace(input.PanelID) != "" && input.PanelID != s.config.PanelID) || input.OperatorUserID <= 0 || strings.TrimSpace(input.HostID) == "" || remnawave.ValidateNodeName(input.NodeName) != nil || !publicIP(input.VPSIP) || len(input.Password) == 0 {
		return PreparedDeployment{}, ErrInvalidInput
	}
	host, profile, err := s.selectedHost(ctx, input.HostID)
	if ctx.Err() != nil {
		return PreparedDeployment{}, ctx.Err()
	}
	if err != nil {
		return PreparedDeployment{}, err
	}
	if err := s.checkUnique(ctx, strings.TrimSpace(input.NodeName), input.VPSIP, "", false); err != nil {
		if ctx.Err() != nil {
			return PreparedDeployment{}, ctx.Err()
		}
		return PreparedDeployment{}, err
	}

	created, err := s.repository.CreateDeployment(ctx, repository.CreateDeploymentParams{
		PanelID:                   s.config.PanelID,
		TelegramOperatorUserID:    input.OperatorUserID,
		SelectedRemnawaveHostUUID: host.UUID,
		SelectedHostRemark:        host.Remark,
		SNIDomain:                 profile.SNIDomain,
		NodeName:                  strings.TrimSpace(input.NodeName),
		TargetVPSIP:               input.VPSIP.Unmap(),
	})
	if err != nil {
		if ctx.Err() != nil {
			return PreparedDeployment{}, ctx.Err()
		}
		return PreparedDeployment{}, safeError("PERSISTENCE_CREATE_FAILED", "Could not create deployment state", ErrPersistenceFailed)
	}
	if s.config.Observer != nil {
		s.config.Observer.DeploymentCreated()
	}
	runCtx, release, err := s.beginExecution(ctx, created.ID)
	if err != nil {
		return PreparedDeployment{}, err
	}
	defer release()
	if err := s.acquire(runCtx); err != nil {
		return PreparedDeployment{}, err
	}
	defer s.releaseSlot()

	if err := s.beginStage(runCtx, created.ID, deployment.StatusPreflight, stepPreflight); err != nil {
		return PreparedDeployment{}, err
	}
	password := append([]byte(nil), input.Password...)
	result, preflightErr := s.vps.Preflight(runCtx, PreflightVPSInput{DeploymentID: created.ID, Address: input.VPSIP.Unmap(), Password: password})
	clear(password)
	if runCtx.Err() != nil {
		return PreparedDeployment{}, runCtx.Err()
	}
	if preflightErr != nil {
		return PreparedDeployment{}, s.failStage(runCtx, created.ID, stepPreflight, deployment.StatusFailed, "PREFLIGHT_FAILED", "VPS preflight could not be completed", ErrPreflightFailed)
	}
	if !result.Passed() {
		message := "VPS did not pass preflight"
		if len(result.FatalFailures) > 0 {
			message = safeText(result.FatalFailures[0].Message, message)
		}
		return PreparedDeployment{}, s.failStage(runCtx, created.ID, stepPreflight, deployment.StatusFailed, "PREFLIGHT_REJECTED", message, ErrPreflightFailed)
	}
	if err := s.completeStage(runCtx, created.ID, stepPreflight, "VPS preflight passed"); err != nil {
		return PreparedDeployment{}, err
	}

	created, err = s.repository.GetDeployment(runCtx, created.ID)
	if err != nil {
		if runCtx.Err() != nil {
			return PreparedDeployment{}, runCtx.Err()
		}
		return PreparedDeployment{}, safeError("PERSISTENCE_READ_FAILED", "Could not load prepared deployment", ErrPersistenceFailed)
	}
	prepared := PreparedDeployment{Deployment: created, ConfigProfileReadiness: true}
	if s.config.DNSDisabled {
		prepared.Warnings = append(prepared.Warnings, OperatorNotice{Code: "W-DNS-DISABLED", Message: "DNS-балансировка отключена для этой панели"})
	} else if match, zoneErr := s.dns.FindZone(runCtx, profile.SNIDomain); zoneErr == nil {
		prepared.DNSZone = match.FQDN
	}
	if runCtx.Err() != nil {
		return PreparedDeployment{}, runCtx.Err()
	}
	if readiness, readinessErr := s.certificates.Readiness(runCtx, profile.SNIDomain); readinessErr == nil {
		prepared.CertificateReadiness = readiness
	} else {
		prepared.CertificateReadiness = CertificateUnknown
	}
	if runCtx.Err() != nil {
		return PreparedDeployment{}, runCtx.Err()
	}
	for _, warning := range result.Warnings {
		if text := safeText(warning.Message, ""); text != "" {
			prepared.Warnings = append(prepared.Warnings, OperatorNotice{Code: warningCode(warning.Code), Message: warningMessage(warning.Code, text)})
		}
	}
	return prepared, nil
}

func warningMessage(code, fallback string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "DOCKER_NOT_INSTALLED":
		return "Docker не установлен — бот установит его автоматически"
	case "REMNANODE_EXISTS":
		return "Обнаружены существующие файлы или контейнеры Remnanode — конфигурация будет проверена и обновлена"
	case "XRAY_SNI_EXISTS":
		return "Обнаружена существующая установка Xray SNI — конфигурация будет проверена и обновлена"
	case "SSH_PORT_NOT_DETECTED":
		return "Порт 22 не найден в списке слушающих портов, хотя SSH-соединение работает"
	case "COMPONENT_INSPECTION_FAILED", "COMPONENT_INFO_INVALID":
		return "Не удалось полностью определить состояние установленного компонента — этап выполнит дополнительную проверку"
	case "CONTAINER_INSPECTION_FAILED":
		return "Не удалось полностью проверить существующие Docker-контейнеры"
	default:
		return fallback
	}
}

func warningCode(code string) string {
	code = strings.Trim(strings.ToUpper(strings.ReplaceAll(code, "_", "-")), "-")
	if code == "" {
		return "W-GENERAL"
	}
	if strings.HasPrefix(code, "W-") {
		return code
	}
	return "W-" + code
}

// Deploy resumes a prepared deployment. Completed deployments are a no-op;
// DNS_FAILED deployments require RetryDNS.
func (s *DeploymentService) Deploy(ctx context.Context, input StartInput, progress ProgressSink) error {
	started := time.Now()
	defer func() {
		if s.config.Observer != nil {
			s.config.Observer.DeploymentDuration(time.Since(started))
		}
	}()
	if strings.TrimSpace(input.DeploymentID) == "" {
		return ErrInvalidInput
	}
	runCtx, release, err := s.beginExecution(ctx, input.DeploymentID)
	if err != nil {
		return err
	}
	defer release()
	if err := s.acquire(runCtx); err != nil {
		return err
	}
	defer s.releaseSlot()

	current, err := s.deploymentForPanel(runCtx, input.DeploymentID)
	if err != nil {
		if runCtx.Err() != nil {
			return runCtx.Err()
		}
		return safeError("PERSISTENCE_READ_FAILED", "Could not load deployment", ErrPersistenceFailed)
	}
	if !matchesStartInput(current, input) {
		return ErrInvalidInput
	}
	if current.Status == deployment.StatusCompleted {
		return nil
	}
	if current.Status == deployment.StatusDNSFailed {
		return ErrDNSRetryRequired
	}
	if current.Status == deployment.StatusFailed || current.Status == deployment.StatusCancelled || current.Status == deployment.StatusCreated {
		return ErrDeploymentNotRunnable
	}

	host, profile, err := s.selectedHost(runCtx, current.SelectedRemnawaveHostUUID)
	if runCtx.Err() != nil {
		return runCtx.Err()
	}
	if err != nil || normalizeDomain(profile.SNIDomain) != normalizeDomain(current.SNIDomain) {
		return s.failStage(runCtx, current.ID, current.CurrentStep, deployment.StatusFailed, "HOST_INVALID", "Selected Host is no longer deployable", ErrHostUnavailable)
	}
	allowRecovery := current.Status == deployment.StatusCreatingRemnawave || current.Status == deployment.StatusWaitingRemnawave || current.Status == deployment.StatusAddingToDNS
	if err := s.checkUnique(runCtx, current.NodeName, current.TargetVPSIP, nodeUUID(current), allowRecovery); err != nil {
		if runCtx.Err() != nil {
			return runCtx.Err()
		}
		return s.failStage(runCtx, current.ID, current.CurrentStep, deployment.StatusFailed, "DUPLICATE_NODE", "Node name or address is no longer unique", err)
	}

	if current.Status == deployment.StatusPreflight || current.Status == deployment.StatusPreparingCertificate || current.Status == deployment.StatusProvisioning {
		if err := s.prepareAndProvision(runCtx, &current, progress); err != nil {
			return err
		}
		current, err = s.repository.GetDeployment(runCtx, current.ID)
		if err != nil {
			if runCtx.Err() != nil {
				return runCtx.Err()
			}
			return safeError("PERSISTENCE_READ_FAILED", "Could not reload deployment", ErrPersistenceFailed)
		}
	}

	if current.Status == deployment.StatusCreatingRemnawave || current.RemnawaveNodeUUID == nil {
		if err := s.createNode(runCtx, &current, host, progress); err != nil {
			return err
		}
		current, err = s.repository.GetDeployment(runCtx, current.ID)
		if err != nil {
			if runCtx.Err() != nil {
				return runCtx.Err()
			}
			return safeError("PERSISTENCE_READ_FAILED", "Could not reload created Node", ErrPersistenceFailed)
		}
	}

	if current.Status == deployment.StatusWaitingRemnawave || current.Status == deployment.StatusCreatingRemnawave {
		if err := s.waitForHealthyNode(runCtx, &current, progress); err != nil {
			return err
		}
		current, err = s.repository.GetDeployment(runCtx, current.ID)
		if err != nil {
			if runCtx.Err() != nil {
				return runCtx.Err()
			}
			return safeError("PERSISTENCE_READ_FAILED", "Could not reload healthy Node", ErrPersistenceFailed)
		}
	}
	if current.Status == deployment.StatusAddingToDNS || current.Status == deployment.StatusWaitingRemnawave {
		return s.addDNS(runCtx, &current, progress)
	}
	return ErrDeploymentNotRunnable
}

func (s *DeploymentService) prepareAndProvision(ctx context.Context, current *deployment.Deployment, progress ProgressSink) error {
	emit(progress, Progress{Step: stepPrepareCertificate, Completed: 1, Total: workflowStepCount, SafeMessage: "Preparing certificate", Status: deployment.StepStatusRunning})
	if current.Status != deployment.StatusProvisioning {
		if err := s.beginStage(ctx, current.ID, deployment.StatusPreparingCertificate, stepPrepareCertificate); err != nil {
			return err
		}
	}
	material, err := s.certificates.Prepare(ctx, current.SNIDomain)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		code := "CERTIFICATE_UNAVAILABLE"
		switch {
		case errors.Is(err, certmanager.ErrIssuanceFailed):
			code = "CERTIFICATE_ISSUANCE_FAILED"
		case errors.Is(err, certmanager.ErrDistributionFailed):
			code = "CERTIFICATE_DISTRIBUTION_FAILED"
		case errors.Is(err, certmanager.ErrStorageFailed):
			code = "CERTIFICATE_STORAGE_FAILED"
		case errors.Is(err, certmanager.ErrPersistenceFailed):
			code = "CERTIFICATE_PERSISTENCE_FAILED"
		}
		message := certmanager.SafeMessage(err, "Certificate is not ready")
		return s.failStage(ctx, current.ID, stepPrepareCertificate, deployment.StatusFailed, code, message, ErrCertificateUnavailable)
	}
	defer material.Destroy()
	if current.Status != deployment.StatusProvisioning {
		if err := s.completeStage(ctx, current.ID, stepPrepareCertificate, "Certificate prepared"); err != nil {
			return err
		}
	}
	emit(progress, Progress{Step: stepPrepareCertificate, Completed: 2, Total: workflowStepCount, SafeMessage: "Certificate prepared", Status: deployment.StepStatusCompleted})
	emit(progress, Progress{Step: stepProvisioning, Completed: 2, Total: workflowStepCount, SafeMessage: "Provisioning VPS", Status: deployment.StepStatusRunning})
	if err := s.beginStage(ctx, current.ID, deployment.StatusProvisioning, stepProvisioning); err != nil {
		return err
	}
	secret, err := s.remnawave.GenerateSecretKey(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || strings.TrimSpace(secret) == "" {
		return s.failStage(ctx, current.ID, stepProvisioning, deployment.StatusFailed, "KEYGEN_FAILED", "Could not generate Remnanode key", ErrProvisioningFailed)
	}
	secretBytes := []byte(secret)
	secret = ""
	defer clear(secretBytes)
	err = s.vps.Provision(ctx, ProvisionVPSInput{
		DeploymentID:       current.ID,
		Address:            current.TargetVPSIP,
		SNIDomain:          current.SNIDomain,
		RemnanodeSecretKey: secretBytes,
		Certificate:        material,
	}, func(report provisioner.Report) {
		if s.config.Observer != nil {
			s.config.Observer.ProvisioningStepDuration(report.Name, report.Duration)
		}
		code := ""
		if report.Status == deployment.StepStatusFailed {
			code = "E-PROVISIONING-" + strings.ToUpper(strings.ReplaceAll(safeText(report.Name, "STAGE"), "_", "-"))
		}
		emit(progress, Progress{Step: "provisioning/" + safeText(report.Name, "stage"), Completed: 2, Total: workflowStepCount, SafeMessage: safeText(report.Summary, "Provisioning stage updated"), Status: report.Status, Code: code})
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return s.failStage(ctx, current.ID, stepProvisioning, deployment.StatusFailed, "PROVISIONING_FAILED", safeProvisioningMessage(err), ErrProvisioningFailed)
	}
	if err := s.completeStage(ctx, current.ID, stepProvisioning, "VPS provisioning completed"); err != nil {
		return err
	}
	emit(progress, Progress{Step: stepProvisioning, Completed: 3, Total: workflowStepCount, SafeMessage: "VPS provisioning completed", Status: deployment.StepStatusCompleted})
	return s.beginStage(ctx, current.ID, deployment.StatusCreatingRemnawave, stepCreateNode)
}

func (s *DeploymentService) createNode(ctx context.Context, current *deployment.Deployment, host remnawave.Host, progress ProgressSink) error {
	emit(progress, Progress{Step: stepCreateNode, Completed: 3, Total: workflowStepCount, SafeMessage: "Creating Remnawave Node", Status: deployment.StepStatusRunning})
	if current.Status != deployment.StatusCreatingRemnawave {
		if err := s.beginStage(ctx, current.ID, deployment.StatusCreatingRemnawave, stepCreateNode); err != nil {
			return err
		}
	}
	if current.RemnawaveNodeUUID == nil {
		nodes, err := s.remnawave.GetNodes(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return s.failStage(ctx, current.ID, stepCreateNode, deployment.StatusFailed, "NODE_DUPLICATE_CHECK_FAILED", "Could not verify Node uniqueness", ErrRemnawaveCreationFailed)
		}
		recovered, duplicateErr := recoverableNode(nodes, current.NodeName, current.TargetVPSIP)
		if duplicateErr != nil {
			return s.failStage(ctx, current.ID, stepCreateNode, deployment.StatusFailed, "DUPLICATE_NODE", "Node name or address is already in use", duplicateErr)
		}
		var node remnawave.Node
		if recovered != nil {
			node = *recovered
		} else {
			node, err = s.remnawave.CreateNode(ctx, remnawave.CreateNodeInput{Name: current.NodeName, Address: current.TargetVPSIP, Host: host})
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return s.failStage(ctx, current.ID, stepCreateNode, deployment.StatusFailed, "NODE_CREATE_FAILED", "Remnawave Node could not be created", ErrRemnawaveCreationFailed)
			}
		}
		if _, err := s.repository.SetRemnawaveNodeUUID(ctx, current.ID, node.UUID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return safeError("NODE_UUID_PERSIST_FAILED", "Node was created but its identifier could not be persisted", ErrPersistenceFailed)
		}
		current.RemnawaveNodeUUID = &node.UUID
	}
	if err := s.completeStage(ctx, current.ID, stepCreateNode, "Remnawave Node created"); err != nil {
		return err
	}
	emit(progress, Progress{Step: stepCreateNode, Completed: 4, Total: workflowStepCount, SafeMessage: "Remnawave Node created", Status: deployment.StepStatusCompleted})
	return s.beginStage(ctx, current.ID, deployment.StatusWaitingRemnawave, stepWaitNode)
}

func (s *DeploymentService) waitForHealthyNode(ctx context.Context, current *deployment.Deployment, progress ProgressSink) error {
	emit(progress, Progress{Step: stepWaitNode, Completed: 4, Total: workflowStepCount, SafeMessage: "Waiting for Remnawave Node connection", Status: deployment.StepStatusRunning})
	if current.RemnawaveNodeUUID == nil {
		return s.failStage(ctx, current.ID, stepWaitNode, deployment.StatusFailed, "NODE_UUID_MISSING", "Remnawave Node identifier is missing", ErrNodeConnectionFailed)
	}
	if current.Status != deployment.StatusWaitingRemnawave {
		if err := s.beginStage(ctx, current.ID, deployment.StatusWaitingRemnawave, stepWaitNode); err != nil {
			return err
		}
	}
	node, err := s.pollNode(ctx, *current.RemnawaveNodeUUID, progress)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		var connectionError *nodeConnectionError
		if errors.As(err, &connectionError) {
			return s.failStage(ctx, current.ID, stepWaitNode, deployment.StatusFailed, connectionError.code, connectionError.message, connectionError.kind)
		}
		return s.failStage(ctx, current.ID, stepWaitNode, deployment.StatusFailed, "NODE_WAIT_FAILED", "Could not read Remnawave Node status", ErrNodeConnectionFailed)
	}
	if !node.IsConnected {
		return s.failStage(ctx, current.ID, stepWaitNode, deployment.StatusFailed, "NODE_NOT_CONNECTED", "Remnawave Node is not connected", ErrNodeConnectionFailed)
	}
	if err := s.completeStage(ctx, current.ID, stepWaitNode, "Remnawave Node connected"); err != nil {
		return err
	}
	emit(progress, Progress{Step: stepWaitNode, Completed: 5, Total: workflowStepCount, SafeMessage: "Remnawave Node connected", Status: deployment.StepStatusCompleted})
	return s.beginStage(ctx, current.ID, deployment.StatusAddingToDNS, stepAddDNS)
}

func (s *DeploymentService) addDNS(ctx context.Context, current *deployment.Deployment, progress ProgressSink) error {
	emit(progress, Progress{Step: stepAddDNS, Completed: 5, Total: workflowStepCount, SafeMessage: "Updating DNS balancing", Status: deployment.StepStatusRunning})
	if current.RemnawaveNodeUUID == nil {
		return ErrDeploymentNotRunnable
	}
	if current.Status != deployment.StatusAddingToDNS {
		if err := s.beginStage(ctx, current.ID, deployment.StatusAddingToDNS, stepAddDNS); err != nil {
			return err
		}
	}
	if s.config.DNSDisabled {
		summary := "DNS balancing is disabled for this panel"
		if _, err := s.repository.RecordDeploymentStep(ctx, repository.RecordStepParams{DeploymentID: current.ID, Name: stepAddDNS, Status: deployment.StepStatusSkipped, SafeSummary: &summary}); err != nil {
			return safeError("DNS_SKIP_PERSIST_FAILED", "Could not persist skipped DNS step", ErrPersistenceFailed)
		}
		if err := s.setState(ctx, current.ID, deployment.StatusCompleted, "completed", "", ""); err != nil {
			return err
		}
		emit(progress, Progress{Step: stepAddDNS, Completed: workflowStepCount, Total: workflowStepCount, SafeMessage: "DNS-балансировка отключена для этой панели", Status: deployment.StepStatusSkipped, Code: "W-DNS-DISABLED"})
		return nil
	}
	node, err := s.remnawave.GetNode(ctx, *current.RemnawaveNodeUUID)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || !node.IsConnected {
		return s.failStage(ctx, current.ID, stepAddDNS, deployment.StatusFailed, "NODE_NOT_HEALTHY", "DNS was not changed because the Node is not connected", ErrNodeConnectionFailed)
	}
	if _, err := s.dns.AddIP(ctx, current.SNIDomain, current.TargetVPSIP); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return s.failStage(ctx, current.ID, stepAddDNS, deployment.StatusDNSFailed, "DNS_UPDATE_FAILED", "Node is healthy but DNS update failed", ErrDNSUpdateFailed)
	}
	if err := s.completeStage(ctx, current.ID, stepAddDNS, "DNS updated"); err != nil {
		return err
	}
	if err := s.setState(ctx, current.ID, deployment.StatusCompleted, "completed", "", ""); err != nil {
		return err
	}
	emit(progress, Progress{Step: stepAddDNS, Completed: workflowStepCount, Total: workflowStepCount, SafeMessage: "DNS updated", Status: deployment.StepStatusCompleted})
	return nil
}

// RetryDNS retries only the DNS side effect and never deletes or recreates a
// healthy Remnawave Node.
func (s *DeploymentService) RetryDNS(ctx context.Context, deploymentID string, progress ProgressSink) error {
	runCtx, release, err := s.beginExecution(ctx, deploymentID)
	if err != nil {
		return err
	}
	defer release()
	if err := s.acquire(runCtx); err != nil {
		return err
	}
	defer s.releaseSlot()
	current, err := s.deploymentForPanel(runCtx, deploymentID)
	if err != nil {
		if runCtx.Err() != nil {
			return runCtx.Err()
		}
		return safeError("PERSISTENCE_READ_FAILED", "Could not load deployment", ErrPersistenceFailed)
	}
	if current.Status == deployment.StatusCompleted {
		return nil
	}
	if current.Status != deployment.StatusDNSFailed && current.Status != deployment.StatusAddingToDNS {
		return ErrDeploymentNotRunnable
	}
	if err := s.beginStage(runCtx, current.ID, deployment.StatusAddingToDNS, stepAddDNS); err != nil {
		return err
	}
	current.Status = deployment.StatusAddingToDNS
	return s.addDNS(runCtx, &current, progress)
}

// RetryFailedStep retries only stages whose side effects have an idempotent
// inspection/recovery contract. Preflight cannot be retried because its
// temporary password is intentionally not persisted.
func (s *DeploymentService) RetryFailedStep(ctx context.Context, deploymentID string, progress ProgressSink) error {
	current, err := s.deploymentForPanel(ctx, deploymentID)
	if err != nil {
		return safeError("PERSISTENCE_READ_FAILED", "Could not load deployment", ErrPersistenceFailed)
	}
	if current.Status != deployment.StatusFailed {
		return ErrDeploymentNotRunnable
	}
	var resume deployment.Status
	switch current.CurrentStep {
	case stepPrepareCertificate:
		resume = deployment.StatusPreflight
	case stepProvisioning:
		resume = deployment.StatusProvisioning
	case stepCreateNode:
		resume = deployment.StatusCreatingRemnawave
	case stepWaitNode:
		resume = deployment.StatusWaitingRemnawave
	case stepAddDNS:
		if err := s.setState(ctx, current.ID, deployment.StatusDNSFailed, stepAddDNS, "DNS_RETRY_REQUIRED", "DNS retry requires a healthy Node"); err != nil {
			return err
		}
		return s.RetryDNS(ctx, current.ID, progress)
	default:
		return ErrDeploymentNotRunnable
	}
	if err := s.setState(ctx, current.ID, resume, current.CurrentStep, "", ""); err != nil {
		return err
	}
	return s.Deploy(ctx, StartInput{DeploymentID: current.ID, OperatorUserID: current.TelegramOperatorUserID, HostID: current.SelectedRemnawaveHostUUID, NodeName: current.NodeName, VPSIP: current.TargetVPSIP}, progress)
}

func (s *DeploymentService) BootstrapCertificate(ctx context.Context, sni string, operatorUserID int64) (string, error) {
	bootstrapper, ok := s.certificates.(CertificateBootstrapper)
	if !ok {
		return "", safeError("CERTIFICATE_BOOTSTRAP_UNAVAILABLE", "Certificate bootstrap is unavailable", ErrCertificateUnavailable)
	}
	result, err := bootstrapper.Bootstrap(ctx, sni, operatorUserID)
	if err != nil {
		message := safeText(err.Error(), "Certificate bootstrap could not be completed")
		return message, safeError("CERTIFICATE_BOOTSTRAP_FAILED", message, ErrCertificateUnavailable)
	}
	return "Certificate " + result.Version + " activated for " + result.SNI +
		"; managed targets: " + strconv.Itoa(result.ManagedTargets) +
		"; acknowledged legacy targets: " + strconv.Itoa(result.AcknowledgedLegacyIPs), nil
}

// SafeLogs returns persisted summaries only. Raw SSH/API output and secrets are
// never part of the repository contract.
func (s *DeploymentService) SafeLogs(ctx context.Context, deploymentID string) ([]SafeLogEntry, error) {
	current, err := s.deploymentForPanel(ctx, deploymentID)
	if err != nil {
		return nil, safeError("PERSISTENCE_READ_FAILED", "Could not load deployment", ErrPersistenceFailed)
	}
	steps, err := s.repository.ListDeploymentSteps(ctx, deploymentID)
	if err != nil {
		return nil, safeError("PERSISTENCE_READ_FAILED", "Could not load safe deployment logs", ErrPersistenceFailed)
	}
	result := make([]SafeLogEntry, 0, len(steps)+1)
	for _, step := range steps {
		summary := ""
		if step.SafeSummary != nil {
			summary = safeText(*step.SafeSummary, "")
		}
		if step.ErrorMessage != nil {
			summary = safeText(*step.ErrorMessage, summary)
		}
		result = append(result, SafeLogEntry{Step: safeText(step.Name, "step"), Status: string(step.Status), Summary: summary})
	}
	if current.SafeErrorMessage != nil {
		result = append(result, SafeLogEntry{Step: safeText(current.CurrentStep, "workflow"), Status: string(current.Status), Summary: safeText(*current.SafeErrorMessage, "")})
	}
	return result, nil
}

// Cancel marks a deployment cancelled and signals an in-process active run.
func (s *DeploymentService) Cancel(ctx context.Context, deploymentID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	active := s.running[deploymentID]
	if active != nil {
		active.cancel()
	}
	s.mu.Unlock()
	current, err := s.deploymentForPanel(ctx, deploymentID)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return safeError("PERSISTENCE_READ_FAILED", "Could not load deployment", ErrPersistenceFailed)
	}
	if current.Status == deployment.StatusCompleted || current.Status == deployment.StatusCancelled {
		return nil
	}
	return s.setState(ctx, deploymentID, deployment.StatusCancelled, "cancelled", "CANCELLED", "Deployment cancelled by operator")
}

func (s *DeploymentService) pollNode(ctx context.Context, uuid string, progress ProgressSink) (remnawave.Node, error) {
	pollCtx, cancel := context.WithTimeout(ctx, s.config.NodeConnectTimeout)
	defer cancel()
	backoff := s.config.InitialPollBackoff
	for {
		node, err := s.remnawave.GetNode(pollCtx, uuid)
		if err == nil {
			if node.IsConnected {
				return node, nil
			}
			if !node.IsConnecting {
				message := "Remnawave Node stopped connecting"
				if node.LastStatusMessage != nil {
					message = safeText(*node.LastStatusMessage, message)
				}
				return remnawave.Node{}, &nodeConnectionError{code: "NODE_CONNECTION_REJECTED", message: message, kind: ErrNodeConnectionFailed}
			}
			emit(progress, Progress{Step: stepWaitNode, Completed: 4, Total: workflowStepCount, SafeMessage: "Waiting for Remnawave Node connection", Status: deployment.StepStatusRunning})
		}
		timer := time.NewTimer(backoff)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return remnawave.Node{}, ctx.Err()
			}
			return remnawave.Node{}, &nodeConnectionError{code: "NODE_CONNECTION_TIMEOUT", message: "Remnawave Node did not connect before timeout", kind: ErrNodeConnectionTimeout}
		case <-timer.C:
		}
		backoff *= 2
		if backoff > s.config.MaxPollBackoff {
			backoff = s.config.MaxPollBackoff
		}
	}
}

type nodeConnectionError struct {
	code    string
	message string
	kind    error
}

func (e *nodeConnectionError) Error() string { return e.message }

func (s *DeploymentService) selectedHost(ctx context.Context, hostID string) (remnawave.Host, remnawave.DeploymentProfile, error) {
	hosts, err := s.remnawave.GetHosts(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return remnawave.Host{}, remnawave.DeploymentProfile{}, ctx.Err()
		}
		return remnawave.Host{}, remnawave.DeploymentProfile{}, safeError("HOSTS_UNAVAILABLE", "Remnawave Hosts are unavailable", ErrHostUnavailable)
	}
	for _, host := range hosts {
		if host.UUID != hostID {
			continue
		}
		if host.IsDisabled {
			break
		}
		profile, profileErr := remnawave.DeploymentProfileFromHost(host)
		if profileErr != nil {
			break
		}
		return host, profile, nil
	}
	return remnawave.Host{}, remnawave.DeploymentProfile{}, ErrHostUnavailable
}

func (s *DeploymentService) checkUnique(ctx context.Context, name string, address netip.Addr, ownUUID string, allowRecovery bool) error {
	nodes, err := s.remnawave.GetNodes(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return safeError("NODE_LIST_UNAVAILABLE", "Could not verify Node uniqueness", ErrRemnawaveCreationFailed)
	}
	for _, node := range nodes {
		if ownUUID != "" && node.UUID == ownUUID {
			continue
		}
		nodeIP, _ := netip.ParseAddr(strings.TrimSpace(node.Address))
		nameMatches := strings.TrimSpace(node.Name) == strings.TrimSpace(name)
		addressMatches := nodeIP.IsValid() && nodeIP.Unmap() == address.Unmap()
		if allowRecovery && ownUUID == "" && nameMatches && addressMatches {
			continue
		}
		if nameMatches && addressMatches {
			return errors.Join(ErrDuplicateNodeName, ErrDuplicateNodeAddress)
		}
		if nameMatches {
			return ErrDuplicateNodeName
		}
		if addressMatches {
			return ErrDuplicateNodeAddress
		}
	}
	return nil
}

func recoverableNode(nodes []remnawave.Node, name string, address netip.Addr) (*remnawave.Node, error) {
	var recovered *remnawave.Node
	for index := range nodes {
		nodeIP, _ := netip.ParseAddr(strings.TrimSpace(nodes[index].Address))
		nameMatches := strings.TrimSpace(nodes[index].Name) == strings.TrimSpace(name)
		addressMatches := nodeIP.IsValid() && nodeIP.Unmap() == address.Unmap()
		if nameMatches && addressMatches {
			if recovered != nil {
				return nil, errors.Join(ErrDuplicateNodeName, ErrDuplicateNodeAddress)
			}
			recovered = &nodes[index]
			continue
		}
		if nameMatches {
			return nil, ErrDuplicateNodeName
		}
		if addressMatches {
			return nil, ErrDuplicateNodeAddress
		}
	}
	return recovered, nil
}

func (s *DeploymentService) beginStage(ctx context.Context, deploymentID string, status deployment.Status, step string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.setState(ctx, deploymentID, status, step, "", ""); err != nil {
		return err
	}
	_, err := s.repository.RecordDeploymentStep(ctx, repository.RecordStepParams{DeploymentID: deploymentID, Name: step, Status: deployment.StepStatusRunning, SafeSummary: stringPtr("started")})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return safeError("STEP_PERSIST_FAILED", "Could not persist deployment step", ErrPersistenceFailed)
	}
	return nil
}

func (s *DeploymentService) completeStage(ctx context.Context, deploymentID, step, summary string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.repository.RecordDeploymentStep(ctx, repository.RecordStepParams{DeploymentID: deploymentID, Name: step, Status: deployment.StepStatusCompleted, SafeSummary: stringPtr(safeText(summary, "completed"))})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return safeError("STEP_PERSIST_FAILED", "Could not persist deployment step", ErrPersistenceFailed)
	}
	return nil
}

func (s *DeploymentService) failStage(ctx context.Context, deploymentID, step string, status deployment.Status, code, message string, kind error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	message = safeText(message, "Deployment step failed")
	if s.config.Observer != nil {
		s.config.Observer.DeploymentFailed()
	}
	var persistErr error
	if strings.TrimSpace(step) != "" {
		if _, err := s.repository.RecordDeploymentStep(ctx, repository.RecordStepParams{DeploymentID: deploymentID, Name: step, Status: deployment.StepStatusFailed, SafeSummary: stringPtr("failed"), ErrorMessage: stringPtr(message)}); err != nil {
			persistErr = err
		}
	}
	if _, err := s.repository.UpdateDeploymentState(ctx, deploymentID, repository.UpdateDeploymentStateParams{Status: status, CurrentStep: stepOrFallback(step), SafeErrorCode: stringPtr(code), SafeErrorMessage: stringPtr(message)}); err != nil {
		persistErr = errors.Join(persistErr, err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if persistErr != nil {
		return safeError("FAILURE_PERSIST_FAILED", "Deployment failed and its state could not be persisted", ErrPersistenceFailed)
	}
	return safeError(code, message, kind)
}

func (s *DeploymentService) setState(ctx context.Context, deploymentID string, status deployment.Status, step, code, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	params := repository.UpdateDeploymentStateParams{Status: status, CurrentStep: stepOrFallback(step)}
	if code != "" {
		params.SafeErrorCode = stringPtr(code)
	}
	if message != "" {
		params.SafeErrorMessage = stringPtr(safeText(message, "Deployment operation failed"))
	}
	if _, err := s.repository.UpdateDeploymentState(ctx, deploymentID, params); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return safeError("STATE_PERSIST_FAILED", "Could not persist deployment state", ErrPersistenceFailed)
	}
	return nil
}

func (s *DeploymentService) deploymentForPanel(ctx context.Context, deploymentID string) (deployment.Deployment, error) {
	item, err := s.repository.GetDeployment(ctx, deploymentID)
	if err != nil {
		return deployment.Deployment{}, err
	}
	if item.PanelID != "" && item.PanelID != s.config.PanelID {
		return deployment.Deployment{}, repository.ErrNotFound
	}
	return item, nil
}

func (s *DeploymentService) beginExecution(parent context.Context, deploymentID string) (context.Context, func(), error) {
	if strings.TrimSpace(deploymentID) == "" {
		return nil, nil, ErrInvalidInput
	}
	s.mu.Lock()
	if _, exists := s.running[deploymentID]; exists {
		s.mu.Unlock()
		return nil, nil, ErrDeploymentAlreadyRunning
	}
	ctx, cancel := context.WithCancel(parent)
	entry := &execution{cancel: cancel}
	s.running[deploymentID] = entry
	s.mu.Unlock()
	return ctx, func() {
		cancel()
		s.mu.Lock()
		if s.running[deploymentID] == entry {
			delete(s.running, deploymentID)
		}
		s.mu.Unlock()
	}, nil
}

func (s *DeploymentService) acquire(ctx context.Context) error {
	select {
	case s.semaphore <- struct{}{}:
		if s.config.Observer != nil {
			s.config.Observer.ActiveDeployment(1)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *DeploymentService) releaseSlot() {
	<-s.semaphore
	if s.config.Observer != nil {
		s.config.Observer.ActiveDeployment(-1)
	}
}

func matchesStartInput(current deployment.Deployment, input StartInput) bool {
	return current.ID == input.DeploymentID && current.TelegramOperatorUserID == input.OperatorUserID &&
		current.SelectedRemnawaveHostUUID == input.HostID && current.NodeName == strings.TrimSpace(input.NodeName) &&
		current.TargetVPSIP == input.VPSIP.Unmap()
}

func publicIP(address netip.Addr) bool {
	if !address.IsValid() {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsMulticast() {
		return false
	}
	for _, prefix := range []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("2001:db8::/32"),
	} {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func nodeUUID(value deployment.Deployment) string {
	if value.RemnawaveNodeUUID == nil {
		return ""
	}
	return *value.RemnawaveNodeUUID
}

func safeError(code, message string, kind error) error {
	return &SafeError{Code: code, SafeMessage: safeText(message, "Deployment operation failed"), Kind: kind}
}

func safeText(value, fallback string) string {
	value = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(value))
	if value == "" {
		value = fallback
	}
	for len(value) > 200 {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}

func stringPtr(value string) *string { return &value }

func stepOrFallback(step string) string {
	if strings.TrimSpace(step) == "" {
		return "workflow"
	}
	return step
}

func emit(sink ProgressSink, progress Progress) {
	if sink != nil {
		sink(progress)
	}
}
