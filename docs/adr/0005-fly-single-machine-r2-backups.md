# Fly.io, one always-on machine, backups on Cloudflare R2

Cermin runs on one always-on Fly.io machine (shared-cpu-1x, 256 MB, region `iad`). A 1 GB volume holds the SQLite file ([ADR-0003](0003-one-process-embedded-sqlite.md)). For about $2.34 a month, Fly takes over the work of running a server:
- it serves TLS on `*.fly.dev`, which the MCP endpoint needs ([ADR-0004](0004-remote-mcp-bearer-token.md));
- it builds and deploys the image remotely;
- it restarts the process and keeps logs and secrets.

Litestream replicates the database to a Cloudflare R2 bucket. R2 belongs to a different company from the host, so an account or billing problem at Fly can't take the backups down with the app. R2's free egress also makes restoring a copy onto a laptop for evaluation free.

`iad` is the cheapest region. Capture latency comes mostly from round trips to the AI APIs and Telegram, not from distance to the user. Moving to another region is just a restore from backup.

## Considered Options

- **GCP e2-micro (Always Free).** It costs $0 and has 1 GB of RAM, but it is a VM to run yourself:
  - OS patches and a systemd service;
  - TLS for MCP, which needs a domain and Caddy;
  - deploys by copying the binary.

  Free egress is capped at 1 GB a month, and only US regions qualify. It is the fallback if paying for hosting stops making sense; the binary and Litestream stay the same.
- **GCP Cloud Run, or any host that scales to zero.** No local disk, so SQLite doesn't fit.
- **AWS Lightsail ($5 with IPv4) or EC2 t4g.nano (about $7, including the $3.65 monthly charge for a public IPv4 address).** More cost for the same VM chores.
- **Railway Hobby ($5).** The easiest deploys, at double the cost.
- **Hetzner** (CX23 at $6.49, EU only; $17.99 in Singapore) and **Render** ($7 plus disk; the free tier sleeps and can't attach a disk).
- **Tigris for backups.** One command to set up on Fly, but it puts the backups on the same platform as the app.

## Consequences

- Fly has no free tier for new organisations, so a card stays on file.
- The code has nothing Fly-specific. Fly's settings live in `fly.toml` next to a generic Dockerfile, and the binary runs on any host with a disk and outbound internet.
