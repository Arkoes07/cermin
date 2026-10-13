// Command cermin runs Cermin.
//
//	cermin serve
//
// captures and classifies Thoughts sent to the Telegram bot until it is
// interrupted, and answers /healthz on port 8080. It reads TELEGRAM_BOT_TOKEN,
// TELEGRAM_OWNER_ID, ANTHROPIC_API_KEY and, optionally, CERMIN_DB_PATH
// (default data/cermin.db) and CERMIN_CLASSIFY_MODEL (default
// claude-sonnet-5-5) from the environment.
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

	"github.com/arkoes07/cermin/internal/capture"
	"github.com/arkoes07/cermin/internal/claude"
	"github.com/arkoes07/cermin/internal/sqlite"
	"github.com/arkoes07/cermin/internal/telegram"
)

// httpAddr is where serve answers HTTP; fly.toml's internal_port matches it.
const httpAddr = ":8080"

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	classifier := claude.New(apiKey, model)
	bot := capture.New(db, telegram.New(telegram.APIURL, token), ownerID, classifier.Classify)
	srv, err := startHTTP(bot)
	if err != nil {
		return errors.Join(err, db.Close())
	}

	slog.InfoContext(ctx, "capturing Thoughts", "db", dbPath, "classify_model", model)
	bot.Run(ctx)
	slog.Info("stopped")
	return errors.Join(srv.Close(), db.Close())
}

// startHTTP serves bot's health at /healthz on httpAddr until the server it
// returns is closed.
func startHTTP(bot *capture.Bot) (*http.Server, error) {
	ln, err := net.Listen("tcp", httpAddr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", httpAddr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", bot.ServeHealth)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("serving http", "err", err)
		}
	}()
	return srv, nil
}
