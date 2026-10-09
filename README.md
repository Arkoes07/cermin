# Cermin

Cermin (Indonesian for "mirror") turns the Thoughts I send to a Telegram bot into a Corpus that mirrors how I think. It classifies each Thought, resurfaces related past ones, points out Loops, and builds a Portrait of my thinking.

**Status:** week 0 of 4. Product docs are done; technical design in progress. Next, week 1 (12–18 Oct): capture and classification live in Telegram.

- Product: [docs/product/PRD.md](docs/product/PRD.md)
- Vocabulary: [CONTEXT.md](CONTEXT.md)
- Design: [docs/design/high-level-design.md](docs/design/high-level-design.md)
- Decisions: [docs/adr/](docs/adr/)

## Run locally

With Go 1.26 and a dev bot (two pollers can't share one bot token):

```sh
set -a; . ./.env; set +a   # TELEGRAM_BOT_TOKEN, TELEGRAM_OWNER_ID
go run ./cmd/cermin serve  # database at data/cermin.db unless CERMIN_DB_PATH is set
```

Built for one user, me. Outside pull requests are not accepted yet ([ADR-0001](docs/adr/0001-agpl-license.md)).

License: [AGPL-3.0](LICENSE).
