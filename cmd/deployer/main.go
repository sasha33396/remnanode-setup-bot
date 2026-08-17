package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"remnanode-setup-bot/internal/certmanager"
	"remnanode-setup-bot/internal/cherryip"
	"remnanode-setup-bot/internal/config"
	"remnanode-setup-bot/internal/dnsbalancer"
	"remnanode-setup-bot/internal/health"
	"remnanode-setup-bot/internal/logging"
	"remnanode-setup-bot/internal/metrics"
	"remnanode-setup-bot/internal/orchestrator"
	"remnanode-setup-bot/internal/provisioner"
	"remnanode-setup-bot/internal/recovery"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/repository/postgres"
	"remnanode-setup-bot/internal/royalip"
	sshclient "remnanode-setup-bot/internal/ssh"
	"remnanode-setup-bot/internal/telegram"
)

func main() { os.Exit(run()) }

func run() int {
	bootstrapLogger := logging.New(os.Stdout)
	cfg, err := config.Load()
	if err != nil {
		bootstrapLogger.Error("configuration validation failed", slog.Any("error", err))
		return 1
	}
	secrets := []string{cfg.TelegramBotToken, cfg.RemnawaveToken, cfg.DNSBalancerToken, cfg.CloudflareAPIToken, cfg.DatabaseURL}
	for _, panel := range cfg.Panels {
		secrets = append(secrets, panel.RemnawaveToken, panel.DNSBalancerToken, panel.CloudflareAPIToken)
	}
	logger := logging.NewWithSecrets(os.Stdout, secrets...)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startupCtx, cancelStartup := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStartup()
	pool, err := postgres.Open(startupCtx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("database startup failed", slog.Any("error", err))
		return 1
	}
	defer pool.Close()
	repository := postgres.New(pool)
	if err := repository.CheckSchema(startupCtx); err != nil {
		logger.Error("database schema check failed", slog.Any("error", err))
		return 1
	}

	deploymentKey, err := os.ReadFile(cfg.DeploySSHPrivateKey)
	if err != nil {
		logger.Error("deployment SSH key could not be read")
		return 1
	}
	signer, err := sshclient.ParseDeploymentPrivateKey(deploymentKey)
	clear(deploymentKey)
	if err != nil {
		logger.Error("deployment SSH key is invalid")
		return 1
	}
	ssh, err := sshclient.NewClient(repository, signer, cfg.SSHConnectTimeout, cfg.SSHCommandTimeout, 1<<20, 1<<20)
	if err != nil {
		logger.Error("SSH client initialization failed")
		return 1
	}
	serverIPSSH, err := sshclient.NewClient(sshclient.NewMemoryHostKeyStore(), signer, cfg.SSHConnectTimeout, cfg.SSHCommandTimeout, 1<<20, 1<<20)
	if err != nil {
		logger.Error("server IP SSH client initialization failed")
		return 1
	}
	cherryIPService, err := cherryip.New(serverIPSSH, cfg.SSHCommandTimeout)
	if err != nil {
		logger.Error("Cherry IP service initialization failed")
		return 1
	}
	royalIPService, err := royalip.New(serverIPSSH, cfg.SSHCommandTimeout)
	if err != nil {
		logger.Error("Royal IP service initialization failed")
		return 1
	}

	registry := metrics.New()
	distributor, err := certmanager.NewSSHDistributor(ssh, certmanager.SSHDistributorConfig{RepositoryURL: cfg.XraySNIRepoURL, Ref: cfg.XraySNIRef, CommandTimeout: cfg.SSHCommandTimeout, MaxConcurrent: cfg.MaxCertificateDistributions})
	if err != nil {
		logger.Error("certificate distributor initialization failed")
		return 1
	}

	certificateManagers := make([]*certmanager.Manager, 0, len(cfg.Panels))
	panelApplications := make([]orchestrator.PanelApplicationConfig, 0, len(cfg.Panels))
	recoveredJobs := 0
	for _, panel := range cfg.Panels {
		vps, provisionerErr := orchestrator.NewSSHProvisioner(ssh, repository, orchestrator.SSHProvisionerConfig{RemnawaveAPIIP: panel.RemnawaveAPIIP, MetricsIP: cfg.MetricsIP, Preflight: provisioner.Requirements{CommandTimeout: cfg.SSHCommandTimeout}, XrayRepository: cfg.XraySNIRepoURL, XrayRef: cfg.XraySNIRef, CommandTimeout: cfg.SSHCommandTimeout})
		if provisionerErr != nil {
			logger.Error("VPS provisioner initialization failed", slog.String("panel_id", panel.ID))
			return 1
		}
		remnawaveClient, clientErr := remnawave.NewClient(panel.RemnawaveURL, panel.RemnawaveToken, cfg.HTTPTimeout)
		if clientErr != nil {
			logger.Error("Remnawave client initialization failed", slog.String("panel_id", panel.ID))
			return 1
		}
		remnawaveAPI := metrics.ObserveRemnawave(remnawaveClient, registry)
		var dnsAPI orchestrator.DNSAPI = dnsbalancer.Disabled{}
		if panel.DNSMode == config.DNSModeEnabled {
			dnsClient, dnsErr := dnsbalancer.NewClient(panel.DNSBalancerURL, panel.DNSBalancerToken, cfg.HTTPTimeout, nil)
			if dnsErr != nil {
				logger.Error("DNS client initialization failed", slog.String("panel_id", panel.ID))
				return 1
			}
			dnsAPI = metrics.ObserveDNS(dnsClient, registry)
		}
		panelStorePath := filepath.Join(cfg.CertificateStorePath, panel.ID)
		certificateStore, storeErr := certmanager.NewFileStore(panelStorePath)
		if storeErr != nil {
			logger.Error("certificate store initialization failed", slog.String("panel_id", panel.ID))
			return 1
		}
		var certificateStorage certmanager.Store = certificateStore
		accountKeyPath := filepath.Join(panelStorePath, "acme-account.key")
		if panel.ID == "default" {
			legacyStore, legacyErr := certmanager.NewFileStore(cfg.CertificateStorePath)
			if legacyErr != nil {
				logger.Error("legacy certificate store initialization failed")
				return 1
			}
			fallback, fallbackErr := certmanager.NewFallbackStore(certificateStore, legacyStore)
			if fallbackErr != nil {
				logger.Error("certificate store fallback initialization failed")
				return 1
			}
			certificateStorage = fallback
			legacyAccountKey := filepath.Join(cfg.CertificateStorePath, "acme-account.key")
			if _, statErr := os.Stat(legacyAccountKey); statErr == nil {
				accountKeyPath = legacyAccountKey
			}
		}
		accountKey, keyErr := certmanager.LoadOrCreateAccountKey(accountKeyPath)
		if keyErr != nil {
			logger.Error("ACME account key initialization failed", slog.String("panel_id", panel.ID))
			return 1
		}
		cloudflare, cfErr := certmanager.NewCloudflareDNS(panel.CloudflareAPIToken, cfg.HTTPTimeout, cfg.DNSPropagationTimeout, cfg.DNSPropagationInterval)
		if cfErr != nil {
			logger.Error("Cloudflare client initialization failed", slog.String("panel_id", panel.ID))
			return 1
		}
		issuer, issuerErr := certmanager.NewACMEIssuer(cfg.ACMEDirectoryURL, cfg.ACMEEmail, accountKey, cloudflare)
		if issuerErr != nil {
			logger.Error("ACME issuer initialization failed", slog.String("panel_id", panel.ID))
			return 1
		}
		scopedRepository, scopeErr := certmanager.NewScopedRepository(panel.ID, repository)
		if scopeErr != nil {
			logger.Error("certificate repository scope failed", slog.String("panel_id", panel.ID))
			return 1
		}
		scopedLocker, lockErr := certmanager.NewScopedLocker(panel.ID, repository)
		if lockErr != nil {
			logger.Error("certificate lock scope failed", slog.String("panel_id", panel.ID))
			return 1
		}
		var targetResolver certmanager.TargetResolver
		if panel.DNSMode == config.DNSModeEnabled {
			targetResolver, err = certmanager.NewPanelDNSDeploymentResolver(panel.ID, dnsAPI, repository)
		} else {
			targetResolver, err = certmanager.NewInventoryResolver(panel.ID, repository)
		}
		if err != nil {
			logger.Error("certificate target resolver initialization failed", slog.String("panel_id", panel.ID))
			return 1
		}
		certificateManager, managerErr := certmanager.New(scopedRepository, certificateStorage, issuer, scopedLocker, targetResolver, distributor, registry, certmanager.Config{RenewBefore: cfg.CertificateRenewBefore, IssueTimeout: cfg.CertificateIssueTimeout, RenewInterval: cfg.CertificateRenewInterval})
		if managerErr != nil {
			logger.Error("certificate manager initialization failed", slog.String("panel_id", panel.ID))
			return 1
		}
		certificateManagers = append(certificateManagers, certificateManager)
		deploymentService, serviceErr := orchestrator.NewDeploymentService(repository, remnawaveAPI, dnsAPI, certificateManager, vps, orchestrator.Config{PanelID: panel.ID, DNSDisabled: panel.DNSMode == config.DNSModeDisabled, MaxConcurrentDeployments: cfg.MaxConcurrentDeployments, NodeConnectTimeout: cfg.NodeConnectTimeout, InitialPollBackoff: time.Second, MaxPollBackoff: 10 * time.Second, Observer: registry})
		if serviceErr != nil {
			logger.Error("deployment service initialization failed", slog.String("panel_id", panel.ID))
			return 1
		}
		recoveryService, recoveryErr := recovery.NewForPanelWithDNSMode(repository, remnawaveAPI, dnsAPI, panel.ID, panel.DNSMode == config.DNSModeDisabled)
		if recoveryErr != nil {
			logger.Error("recovery service initialization failed", slog.String("panel_id", panel.ID))
			return 1
		}
		results, recoverErr := recoveryService.RecoverUnfinished(startupCtx, 100)
		if recoverErr != nil {
			logger.Error("startup recovery failed", slog.String("panel_id", panel.ID))
			return 1
		}
		for _, result := range results {
			logger.Info("unfinished deployment classified", slog.String("panel_id", panel.ID), slog.String("deployment_id", result.DeploymentID), slog.String("classification", string(result.Classification)))
		}
		recoveredJobs += len(results)
		panelApplications = append(panelApplications, orchestrator.PanelApplicationConfig{ID: panel.ID, Name: panel.Name, DNSEnabled: panel.DNSMode == config.DNSModeEnabled, Service: deploymentService, Recovery: recoveryService})
	}

	application, err := orchestrator.NewMultiPanelTelegramApplication(panelApplications)
	if err != nil {
		logger.Error("Telegram application initialization failed")
		return 1
	}
	if err := application.SetCherryIPService(cherryIPService); err != nil {
		logger.Error("Cherry IP application initialization failed")
		return 1
	}
	if err := application.SetRoyalIPService(royalIPService); err != nil {
		logger.Error("Royal IP application initialization failed")
		return 1
	}
	bot, err := telegram.NewBotAPI(cfg.TelegramBotToken, cfg.TelegramPollTimeout)
	if err != nil {
		logger.Error("Telegram transport initialization failed")
		return 1
	}
	controller, err := telegram.NewController(cfg.TelegramAllowedUsers, application, bot, cfg.TelegramSessionTTL)
	if err != nil {
		logger.Error("Telegram controller initialization failed")
		return 1
	}
	nodePolicy := telegram.NodePolicy{
		CriticalOnlineThreshold: cfg.NodeCriticalOnlineThreshold,
	}
	controller.SetNodePolicy(nodePolicy)
	nodeMonitor, err := telegram.NewNodeMonitor(cfg.TelegramAllowedUsers, application, bot, cfg.NodeMonitorInterval, cfg.NodeCriticalAlertInterval, cfg.NodeMonitorConfirmations, nodePolicy)
	if err != nil {
		logger.Error("node monitor initialization failed")
		return 1
	}
	defer func() { controller.Close(); controller.Wait() }()

	healthServer := health.NewServerWithOptions(cfg.HealthAddr, logger, func(checkCtx context.Context) error {
		if err := pool.Ping(checkCtx); err != nil {
			return errors.New("database unavailable")
		}
		return repository.CheckSchema(checkCtx)
	}, registry)
	group, runCtx := errgroup.WithContext(ctx)
	group.Go(func() error { return healthServer.Run(runCtx) })
	group.Go(func() error { return bot.Run(runCtx, controller) })
	group.Go(func() error { return nodeMonitor.Run(runCtx) })
	for _, manager := range certificateManagers {
		manager := manager
		group.Go(func() error { return manager.Run(runCtx) })
	}
	logger.Info("deployer started", slog.String("health_addr", cfg.HealthAddr), slog.Int("panels", len(cfg.Panels)), slog.Int("recovered_jobs", recoveredJobs))
	if err := group.Wait(); err != nil && ctx.Err() == nil {
		logger.Error("deployer stopped with an error", slog.Any("error", err))
		return 1
	}
	logger.Info("deployer stopped")
	return 0
}
