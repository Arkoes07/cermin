// Command cermin runs Cermin.
//
//	cermin serve
//
// captures and classifies Thoughts sent to the Telegram bot until it is
// interrupted. It reads TELEGRAM_BOT_TOKEN, TELEGRAM_OWNER_ID,
// ANTHROPIC_API_KEY and, optionally, CERMIN_DB_PATH (default data/cermin.db)
// and CERMIN_CLASSIFY_MODEL (default claude-sonnet-5-5) from the environment.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/arkoes07/cermin/internal/capture"
	"github.com/arkoes07/cermin/internal/claude"
	"github.com/arkoes07/cermin/internal/sqlite"
	"github.com/arkoes07/cermin/internal/telegram"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "capturing Thoughts", "db", dbPath, "classify_model", model)
	classifier := claude.New(apiKey, model)
	bot := capture.New(db, telegram.New(telegram.APIURL, token), ownerID, classifier.Classify)
	bot.Run(ctx)
	slog.Info("stopped")
	return db.Close()
}
