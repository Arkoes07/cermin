// Command cermin runs Cermin.
//
//	cermin serve
//
// captures and classifies Thoughts sent to the Telegram bot until it is
// interrupted, answers /healthz on port 8080 and serves Prometheus metrics at
// /metrics on port 9091. It reads TELEGRAM_BOT_TOKEN, TELEGRAM_OWNER_ID,
// ANTHROPIC_API_KEY and, optionally, CERMIN_DB_PATH (default data/cermin.db),
// CERMIN_CLASSIFY_MODEL (default claude-sonnet-5-5) and
// CERMIN_LITESTREAM_SOCKET from the environment. With CERMIN_LITESTREAM_SOCKET
// set, it also watches the backups through Litestream's IPC socket there.
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
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/arkoes07/cermin/internal/backup"
	"github.com/arkoes07/cermin/internal/capture"
	"github.com/arkoes07/cermin/internal/claude"
	"github.com/arkoes07/cermin/internal/litestream"
	"github.com/arkoes07/cermin/internal/sqlite"
	"github.com/arkoes07/cermin/internal/telegram"
)

const (
	// httpAddr is where serve answers HTTP; fly.toml's internal_port matches it.
	httpAddr = ":8080"
	// metricsAddr is where serve answers Fly's metrics scraper; fly.toml's
	// [metrics] port matches it, and no service exposes it, so it is never
	// public.
	metricsAddr = ":9091"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("cermin failed", "err", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 || args[0] != "serve" {
		return errors.New("usage: cermin serve")
	}
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
	dbPath := cmp.Or(os.Getenv("CERMIN_DB_PATH"), "data/cermin.db")
	model := cmp.Or(os.Getenv("CERMIN_CLASSIFY_MODEL"), claude.DefaultModel)
	litestreamSocket := os.Getenv("CERMIN_LITESTREAM_SOCKET")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	classifier := claude.New(apiKey, model)
	tg := telegram.New(telegram.APIURL, token)
	bot := capture.New(db, tg, ownerID, classifier.Classify)

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
	slog.InfoContext(ctx, "capturing Thoughts", "db", dbPath, "classify_model", model)
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
