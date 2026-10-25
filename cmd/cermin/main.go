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
//	cermin pairs [<old.csv>]
//
// writes a CSV to stdout, for measuring Resurfacing (#29): every pair each
// Thought shortlists for a Resurfacing, grouped by Thought, then by rank, with
// a wanted column to label. A pair labelled in old.csv, an earlier CSV from
// cermin pairs, keeps its label and has held_out n; any other has held_out y.
// It reads only CERMIN_DB_PATH and CERMIN_EMBED_MODEL, and makes no network
// call.
//
//	cermin eval resurfacing <labelled.csv>
//
// prints how each way of deciding which shortlisted Thoughts come back in a
// Resurfacing does against your labels in labelled.csv, a CSV from cermin pairs
// with every row labelled: numbers only, no Thought text. It reads the current
// Classifications from CERMIN_DB_PATH, and judges each shortlist with Claude,
// so it reads ANTHROPIC_API_KEY and, optionally, CERMIN_JUDGE_MODEL (default
// claude-sonnet-5-5).
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// defaultDBPath is the database file when CERMIN_DB_PATH is unset.
const defaultDBPath = "data/cermin.db"

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
		return writePairs(ctx, "")
	case len(args) == 2 && args[0] == "pairs":
		return writePairs(ctx, args[1])
	case len(args) == 3 && args[0] == "eval" && args[1] == "resurfacing":
		return evalResurfacing(ctx, args[2])
	default:
		return errors.New("usage: cermin serve | cermin import <file> | cermin pairs [<old.csv>]" +
			" | cermin eval resurfacing <labelled.csv>")
	}
}
