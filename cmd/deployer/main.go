package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"remnanode-setup-bot/internal/config"
	"remnanode-setup-bot/internal/health"
	"remnanode-setup-bot/internal/logging"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := logging.New(os.Stdout)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration validation failed", slog.Any("error", err))
		return 1
	}
	logger.Info("configuration loaded")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := health.NewServer(cfg.HealthAddr, logger)
	logger.Info("deployer starting", slog.String("health_addr", cfg.HealthAddr))
	if err := server.Run(ctx); err != nil {
		logger.Error("deployer stopped with an error", slog.Any("error", err))
		return 1
	}

	logger.Info("deployer stopped")
	return 0
}
