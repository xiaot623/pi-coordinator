package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/xiaot/pi-coordinator/internal/app"
	"github.com/xiaot/pi-coordinator/internal/config"
	"github.com/xiaot/pi-coordinator/internal/source/telegram"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, paths, err := config.Load()
	if err != nil {
		if errors.Is(err, config.ErrConfigMissing) {
			fmt.Fprintf(os.Stderr, "config missing: created template at %s\n", paths.ConfigPath)
			fmt.Fprintln(os.Stderr, "fill telegram.bot_token, telegram.group_chat_id, and telegram.allowed_users, then run again")
			return
		}
		logger.Error("load config", "error", err)
		os.Exit(1)
	}

	a, err := app.New(cfg, paths, logger)
	if err != nil {
		logger.Error("initialize app", "error", err)
		os.Exit(1)
	}
	defer a.Close()

	// Start config hot-reloading
	go watchConfig(ctx, a, paths.ConfigPath, logger)

	bot := telegram.NewBot(a)
	if err := bot.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("run bot", "error", err)
		os.Exit(1)
	}
}

func watchConfig(ctx context.Context, a *app.App, path string, logger *slog.Logger) {
	err := config.Watch(ctx, path, func(cfg config.Config, err error) {
		if err != nil {
			logger.Warn("config reload failed", "error", err)
			return
		}
		a.UpdateConfig(cfg)
		logger.Info("config hot-reloaded successfully")
	})
	if err != nil {
		logger.Warn("failed to watch config", "error", err)
	}
}
