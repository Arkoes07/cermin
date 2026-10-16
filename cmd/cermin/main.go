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
//
//	cermin import <file>
//
// saves each message in the WhatsApp chat export file as a Thought, skipping
// those already imported, and prints how many it imported and skipped. It
// reads only CERMIN_DB_PATH. The catch-up of a running serve classifies them.
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
	"github.com/arkoes07/cermin/internal/cermin"
	"github.com/arkoes07/cermin/internal/chatexport"
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
	// defaultDBPath is the database file when CERMIN_DB_PATH is unset.
	defaultDBPath = "data/cermin.db"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("cermin failed", "err", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch {
	case len(args) == 1 && args[0] == "serve":
		return serve(ctx)
	case len(args) == 2 && args[0] == "import":
		return importChat(ctx, args[1])
	default:
		return errors.New("usage: cermin serve | cermin import <file>")
	}
}

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
	dbPath := cmp.Or(os.Getenv("CERMIN_DB_PATH"), defaultDBPath)
	model := cmp.Or(os.Getenv("CERMIN_CLASSIFY_MODEL"), claude.DefaultModel)
	litestreamSocket := os.Getenv("CERMIN_LITESTREAM_SOCKET")

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

// importChat saves each message in the WhatsApp chat export at path as a
// Thought, with WhatsApp as its source, the channel it was captured in. It
// skips those already imported, so it can be run again, and prints how many it
// imported and skipped.
func importChat(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	messages, err := chatexport.Parse(f)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}

	db, err := sqlite.Open(ctx, cmp.Or(os.Getenv("CERMIN_DB_PATH"), defaultDBPath))
	if err != nil {
		return err
	}
	now := time.Now()
	imported := 0
	for _, m := range messages {
		isNew, err := db.SaveImportedThought(ctx, cermin.Thought{
			Text:       m.Text,
			Source:     "whatsapp",
			ThoughtAt:  m.ThoughtAt,
			ReceivedAt: now,
		})
		if err != nil {
			return errors.Join(err, db.Close())
		}
		if isNew {
			imported++
		}
	}
	fmt.Printf("imported %d Thoughts, skipped %d already imported\n", imported, len(messages)-imported)
	return db.Close()
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
