package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/arkoes07/cermin/internal/backup"
	"github.com/arkoes07/cermin/internal/capture"
	"github.com/arkoes07/cermin/internal/claude"
	"github.com/arkoes07/cermin/internal/litestream"
	"github.com/arkoes07/cermin/internal/sqlite"
	"github.com/arkoes07/cermin/internal/telegram"
	"github.com/arkoes07/cermin/internal/workersai"
)

const (
	// httpAddr is where serve answers HTTP; fly.toml's internal_port matches it.
	httpAddr = ":8080"
	// metricsAddr is where serve answers Fly's metrics scraper; fly.toml's
	// [metrics] port matches it, and no service exposes it, so it is never
	// public.
	metricsAddr = ":9091"
)

// serve runs the bot until ctx is cancelled.
func serve(ctx context.Context) error {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return errors.New("missing TELEGRAM_BOT_TOKEN")
	}
	ownerID, err := strconv.ParseInt(os.Getenv("TELEGRAM_OWNER_ID"), 10, 64)
	if err != nil {
		return fmt.Errorf("parsing TELEGRAM_OWNER_ID: %w", err)
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return errors.New("missing ANTHROPIC_API_KEY")
	}
	cfAccountID := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	if cfAccountID == "" {
		return errors.New("missing CLOUDFLARE_ACCOUNT_ID")
	}
	cfToken := os.Getenv("CLOUDFLARE_API_TOKEN")
	if cfToken == "" {
		return errors.New("missing CLOUDFLARE_API_TOKEN")
	}
	dbPath := cmp.Or(os.Getenv("CERMIN_DB_PATH"), defaultDBPath)
	model := cmp.Or(os.Getenv("CERMIN_CLASSIFY_MODEL"), claude.DefaultModel)
	embedModel := cmp.Or(os.Getenv("CERMIN_EMBED_MODEL"), workersai.DefaultModel)
	litestreamSocket := os.Getenv("CERMIN_LITESTREAM_SOCKET")

	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	classifier := claude.New(apiKey, model)
	embedder := workersai.New(workersai.APIURL, cfAccountID, cfToken, embedModel)
	tg := telegram.New(telegram.APIURL, token)
	bot := capture.New(db, tg, ownerID, classifier.Classify, embedder.Embed, embedder.Model())

	health := http.NewServeMux()
	health.HandleFunc("GET /healthz", bot.ServeHealth)
	healthSrv, err := startHTTP(httpAddr, health)
	if err != nil {
		return errors.Join(err, db.Close())
	}

	metrics := http.NewServeMux()
	metrics.Handle("GET /metrics", promhttp.Handler())
	metricsSrv, err := startHTTP(metricsAddr, metrics)
	if err != nil {
		return errors.Join(err, healthSrv.Close(), db.Close())
	}

	if litestreamSocket != "" {
		slog.InfoContext(ctx, "watching backups", "litestream_socket", litestreamSocket)
		go backup.New(litestream.New(litestreamSocket, dbPath), tg, ownerID).Run(ctx)
	}
	slog.InfoContext(ctx, "capturing Thoughts", "db", dbPath, "classify_model", model, "embed_model", embedModel)
	bot.Run(ctx)
	slog.Info("stopped")
	return errors.Join(healthSrv.Close(), metricsSrv.Close(), db.Close())
}

// startHTTP serves h on addr until the server it returns is closed.
func startHTTP(addr string, h http.Handler) (*http.Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("serving http", "addr", addr, "err", err)
		}
	}()
	return srv, nil
}
