# One Go process with an embedded SQLite database

Cermin runs as a single Go binary. The Telegram bot, the background workers, the MCP endpoint and the scheduler share one process, which owns one SQLite file (WAL mode, pure-Go driver) on a persistent volume. Litestream streams that file to object storage as the backup. At about 5 Thoughts a day (~2,000 a year), one writer is plenty. Reads are in-process, and similarity search is a brute-force scan of vectors held in memory, which takes about a millisecond at this size. For evaluation, the whole Corpus can be restored onto a laptop as one file. The only cost is the volume.

## Considered Options

- **Two binaries, one for the bot and one for the MCP server.** A volume attaches to one machine. A separate MCP process would therefore need one of three things: a supervisor on the same machine, an HTTP API on the bot to call, or a networked database. All are extra moving parts for a small read-only endpoint. Go's HTTP server recovers a panic per request, so an MCP bug can't take Capture down. The code is split by package, so extracting the MCP server later is a refactor.
- **Managed Postgres (Neon or Supabase free tiers).** It brings managed backups, point-in-time restore and pgvector, and would allow serverless hosting. Against it:
  - free tiers suspend or pause when idle, which means cold starts;
  - paid plans break the $10/month budget (Supabase Pro is $25);
  - every query crosses the network;
  - another company holds the Corpus.
- **Self-hosted Postgres on the same machine.** Same features without the vendor, but it adds a second server process to configure, upgrade, vacuum and back up. It also needs more memory, so a bigger machine. Nothing at this scale needs it.
- **Turso (hosted libSQL).** SQLite dialect with native vector search, but it adds another vendor and a network hop. Its Go driver also needs CGO for local replicas.

## Consequences

- Cermin is pinned to one machine. There is no horizontal scaling, and each deploy causes a few seconds of downtime; Telegram holds updates meanwhile, so nothing is lost.
- Backups are only as good as the Litestream setup, so week 1 includes a restore drill. Litestream 0.5 has no client-side encryption, so backups are only as private as their bucket.
- Going multi-user (the gate in [ADR-0002](0002-single-user-business-deferred.md)) probably means moving to Postgres. That cost is accepted.
