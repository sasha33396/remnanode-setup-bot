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
	logger := logging.NewWithSecrets(os.Stdout, cfg.TelegramBotToken, cfg.RemnawaveToken, cfg.DNSBalancerToken, cfg.CloudflareAPIToken, cfg.DatabaseURL)
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

	registry := metrics.New()
	remnawaveClient, err := remnawave.NewClient(cfg.RemnawaveURL, cfg.RemnawaveToken, cfg.HTTPTimeout)
	if err != nil {
		logger.Error("Remnawave client initialization failed")
		return 1
	}
	remnawaveAPI := metrics.ObserveRemnawave(remnawaveClient, registry)
	dnsClient, err := dnsbalancer.NewClient(cfg.DNSBalancerURL, cfg.DNSBalancerToken, cfg.HTTPTimeout, nil)
	if err != nil {
		logger.Error("DNS client initialization failed")
		return 1
	}
	dnsAPI := metrics.ObserveDNS(dnsClient, registry)

	certificateStore, err := certmanager.NewFileStore(cfg.CertificateStorePath)
	if err != nil {
		logger.Error("certificate store initialization failed", slog.Any("error", err))
		return 1
	}
	accountKey, err := certmanager.LoadOrCreateAccountKey(filepath.Join(cfg.CertificateStorePath, "acme-account.key"))
	if err != nil {
		logger.Error("ACME account key initialization failed", slog.Any("error", err))
		return 1
	}
	cloudflare, err := certmanager.NewCloudflareDNS(cfg.CloudflareAPIToken, cfg.HTTPTimeout, cfg.DNSPropagationTimeout, cfg.DNSPropagationInterval)
	if err != nil {
		logger.Error("Cloudflare DNS challenge client initialization failed")
		return 1
	}
	issuer, err := certmanager.NewACMEIssuer(cfg.ACMEDirectoryURL, cfg.ACMEEmail, accountKey, cloudflare)
	if err != nil {
		logger.Error("ACME issuer initialization failed")
		return 1
	}
	targetResolver, err := certmanager.NewDNSDeploymentResolver(dnsAPI, repository)
	if err != nil {
		logger.Error("certificate target resolver initialization failed")
		return 1
	}
	distributor, err := certmanager.NewSSHDistributor(ssh, certmanager.SSHDistributorConfig{RepositoryURL: cfg.XraySNIRepoURL, Ref: cfg.XraySNIRef, CommandTimeout: cfg.SSHCommandTimeout, MaxConcurrent: cfg.MaxCertificateDistributions})
	if err != nil {
		logger.Error("certificate distributor initialization failed")
		return 1
	}
	certificateManager, err := certmanager.New(repository, certificateStore, issuer, repository, targetResolver, distributor, registry, certmanager.Config{RenewBefore: cfg.CertificateRenewBefore, IssueTimeout: cfg.CertificateIssueTimeout, RenewInterval: cfg.CertificateRenewInterval})
	if err != nil {
		logger.Error("certificate manager initialization failed")
		return 1
	}

	vps, err := orchestrator.NewSSHProvisioner(ssh, repository, orchestrator.SSHProvisionerConfig{RemnawaveAPIIP: cfg.RemnaAPIIP, MetricsIP: cfg.MetricsIP, Preflight: provisioner.Requirements{CommandTimeout: cfg.SSHCommandTimeout}, XrayRepository: cfg.XraySNIRepoURL, XrayRef: cfg.XraySNIRef, CommandTimeout: cfg.SSHCommandTimeout})
	if err != nil {
		logger.Error("VPS provisioner initialization failed")
		return 1
	}
	deploymentService, err := orchestrator.NewDeploymentService(repository, remnawaveAPI, dnsAPI, certificateManager, vps, orchestrator.Config{MaxConcurrentDeployments: cfg.MaxConcurrentDeployments, NodeConnectTimeout: cfg.NodeConnectTimeout, InitialPollBackoff: time.Second, MaxPollBackoff: 10 * time.Second, Observer: registry})
	if err != nil {
		logger.Error("deployment service initialization failed")
		return 1
	}
	recoveryService, err := recovery.New(repository, remnawaveAPI, dnsAPI)
	if err != nil {
		logger.Error("recovery service initialization failed")
		return 1
	}
	recoveryResults, err := recoveryService.RecoverUnfinished(startupCtx, 100)
	if err != nil {
		logger.Error("startup recovery failed", slog.Any("error", err))
		return 1
	}
	for _, result := range recoveryResults {
		logger.Info("unfinished deployment classified", slog.String("deployment_id", result.DeploymentID), slog.String("classification", string(result.Classification)))
	}

	application, err := orchestrator.NewTelegramApplicationWithRecovery(deploymentService, recoveryService)
	if err != nil {
		logger.Error("Telegram application initialization failed")
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
	group.Go(func() error { return certificateManager.Run(runCtx) })
	logger.Info("deployer started", slog.String("health_addr", cfg.HealthAddr), slog.Int("recovered_jobs", len(recoveryResults)))
	if err := group.Wait(); err != nil && ctx.Err() == nil {
		logger.Error("deployer stopped with an error", slog.Any("error", err))
		return 1
	}
	logger.Info("deployer stopped")
	return 0
}
