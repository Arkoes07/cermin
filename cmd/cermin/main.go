// Command cermin runs Cermin.
//
//	cermin serve
//
// captures, classifies and embeds Thoughts sent to the Telegram bot until it
// is interrupted, answers /healthz on port 8080 and serves Prometheus metrics
// at /metrics on port 9091. It reads TELEGRAM_BOT_TOKEN, TELEGRAM_OWNER_ID,
// ANTHROPIC_API_KEY, CLOUDFLARE_ACCOUNT_ID, CLOUDFLARE_API_TOKEN and,
// optionally, CERMIN_DB_PATH (default data/cermin.db), CERMIN_CLASSIFY_MODEL
// (default claude-sonnet-5-5), CERMIN_EMBED_MODEL (default
// @cf/qwen/qwen3-embedding-0.6b) and CERMIN_LITESTREAM_SOCKET from the
// environment. With CERMIN_LITESTREAM_SOCKET set, it also watches the backups
// through Litestream's IPC socket there.
//
//	cermin import <file>
//
// saves each message in the WhatsApp chat export file as a Thought, skipping
// those already imported, and prints how many it imported and skipped. It
// reads only CERMIN_DB_PATH. The catch-up of a running serve classifies and
// embeds them.
//
//	cermin pairs
//
// writes a CSV to stdout, for calibrating the Resurfacing cut-off (ADR-0010):
// each Thought that has an eligible earlier Thought, paired with its best one,
// best score first, with an empty wanted column to label. It reads only
// CERMIN_DB_PATH and CERMIN_EMBED_MODEL, and makes no network call.
package main

import (
	"cmp"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
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
	"github.com/arkoes07/cermin/internal/workersai"
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
	case len(args) == 1 && args[0] == "pairs":
		return writePairs(ctx)
	default:
		return errors.New("usage: cermin serve | cermin import <file> | cermin pairs")
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

// writePairs writes to stdout, as CSV, each Thought that has an eligible
// earlier Thought, paired with its best one, best score first, with both
// Thought dates in WIB and an empty wanted column for you to label.
func writePairs(ctx context.Context) error {
	db, err := sqlite.Open(ctx, cmp.Or(os.Getenv("CERMIN_DB_PATH"), defaultDBPath))
	if err != nil {
		return err
	}
	model := cmp.Or(os.Getenv("CERMIN_EMBED_MODEL"), workersai.DefaultModel)
	corpus, err := db.GetThoughtsWithEmbeddingByModel(ctx, model)
	if err := errors.Join(err, db.Close()); err != nil {
		return err
	}

	type pair struct {
		thought cermin.Thought
		best    cermin.ScoredThought
	}
	pairs := []pair{}
	// ponytail: ranks the whole Corpus for each Thought, O(n²); fine for a
	// one-off on a few thousand Thoughts.
	for _, t := range corpus {
		if ranked := cermin.RankEligible(t, corpus); len(ranked) > 0 {
			pairs = append(pairs, pair{thought: t.Thought, best: ranked[0]})
		}
	}
	slices.SortStableFunc(pairs, func(a, b pair) int {
		return cmp.Compare(b.best.Score, a.best.Score)
	})

	rows := [][]string{
		{"score", "thought_id", "past_id", "thought_date", "past_date", "thought_text", "past_text", "wanted"},
	}
	for _, p := range pairs {
		rows = append(rows, []string{
			strconv.FormatFloat(p.best.Score, 'f', 4, 64),
			strconv.FormatInt(p.thought.ID, 10),
			strconv.FormatInt(p.best.ID, 10),
			p.thought.ThoughtAt.In(cermin.WIB).Format(time.DateOnly),
			p.best.ThoughtAt.In(cermin.WIB).Format(time.DateOnly),
			p.thought.Text,
			p.best.Text,
			"",
		})
	}
	if err := csv.NewWriter(os.Stdout).WriteAll(rows); err != nil {
		return fmt.Errorf("writing pairs: %w", err)
	}
	// On stderr, so that an empty CSV, from a copy with no vectors or the
	// wrong model, says why.
	fmt.Fprintf(os.Stderr, "wrote %d pairs from %d Thoughts with a vector from %s\n", len(pairs), len(corpus), model)
	return nil
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
