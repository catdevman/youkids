package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/catdevman/youkids/internal/config"
	"github.com/catdevman/youkids/internal/db"
	"github.com/catdevman/youkids/internal/notify"
	"github.com/catdevman/youkids/internal/plex"
	"github.com/catdevman/youkids/internal/web"
	"github.com/catdevman/youkids/internal/worker"
	"github.com/catdevman/youkids/internal/ytdlp"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := config.Load()

	if err := os.MkdirAll(cfg.MediaDir, 0o755); err != nil {
		logger.Error("failed to create media directory", "dir", cfg.MediaDir, "err", err)
		os.Exit(1)
	}

	store, err := db.Open(cfg.DBPath())
	if err != nil {
		logger.Error("failed to open database", "path", cfg.DBPath(), "err", err)
		os.Exit(1)
	}
	defer store.Close()

	defaults := db.DefaultSettings(cfg.SyncCron, cfg.BaseURL, cfg.PlexURL, cfg.PlexToken, cfg.PlexSectionID)
	defaults.PlexLabels = cfg.PlexLabels
	yt := ytdlp.NewRunner(cfg.YTDLPBin, cfg.CookiesFile)
	plexClient := plex.NewClient()
	notifier := notify.NewNotifier()

	mgr := worker.NewManager(store, defaults, cfg.MediaDir, yt, plexClient, notifier, logger)
	if err := mgr.Start(context.Background()); err != nil {
		logger.Error("failed to start background worker", "err", err)
		os.Exit(1)
	}
	defer mgr.Stop()

	srv := web.NewServer(store, defaults, cfg.MediaDir, cfg.Password, yt, plexClient, notifier, mgr, logger)
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("YouKids server listening", "addr", cfg.Addr, "media_dir", cfg.MediaDir, "data_dir", cfg.DataDir)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", "err", err)
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("shutting down gracefully")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
}
