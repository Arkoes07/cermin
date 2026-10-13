# Cermin

Cermin (Indonesian for "mirror") turns the Thoughts I send to a Telegram bot into a Corpus that mirrors how I think. It classifies each Thought, resurfaces related past ones, points out Loops, and builds a Portrait of my thinking.

**Status:** week 1 of 4 (12–18 Oct). Capture and classification are live in Telegram, deployed to Fly.io and backed up to Cloudflare R2. Next: the restore drill (#4), which real Thoughts wait for, then metrics (#23) and the backup watch (#24).

- Product: [docs/product/PRD.md](docs/product/PRD.md)
- Vocabulary: [CONTEXT.md](CONTEXT.md)
- Design: [docs/design/high-level-design.md](docs/design/high-level-design.md)
- Decisions: [docs/adr/](docs/adr/)

## Run locally

With Go 1.26 and a dev bot (two pollers can't share one bot token):

```sh
set -a; . ./.env; set +a   # TELEGRAM_BOT_TOKEN, TELEGRAM_OWNER_ID, ANTHROPIC_API_KEY
go run ./cmd/cermin serve  # database at data/cermin.db unless CERMIN_DB_PATH is set
```

Thoughts are classified by `claude-sonnet-5-5` unless `CERMIN_CLASSIFY_MODEL` names another model. `go test -tags integration ./internal/claude/` checks the classifier against the real API. `curl -i localhost:8080/healthz` answers 200 while the last poll of Telegram finished under 2 minutes ago, 503 otherwise.

## Deploy

Every push and pull request runs `go vet` and `go test` in GitHub Actions. A push to `main` that passes them then deploys: `flyctl deploy --remote-only --ha=false` builds the [Dockerfile](Dockerfile) on Fly's builder and replaces the one machine of the app in [fly.toml](fly.toml). Inside it, Litestream starts `cermin serve`, first restoring the database from R2 if the volume has none, and replicates every change to R2 ([litestream.yml](litestream.yml)). Secrets are Fly secrets ([design doc §5.3](docs/design/high-level-design.md#53-configuration-and-secrets)); none is in this repo.

Built for one user, me. Outside pull requests are not accepted yet ([ADR-0001](docs/adr/0001-agpl-license.md)).

License: [AGPL-3.0](LICENSE).
