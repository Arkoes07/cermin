# Remote MCP over Streamable HTTP with a bearer token, no OAuth in v1

The Corpus lives on the server, so the MCP server is an HTTP endpoint (`/mcp`, Streamable HTTP) inside the Cermin process ([ADR-0003](0003-one-process-embedded-sqlite.md)). It is protected by one static bearer token over TLS, and its tools are read-only. Claude Code sends the header natively. Claude Desktop connects through the `mcp-remote` stdio bridge with the same header, because Desktop's connector settings support only OAuth or no auth for remote servers (static headers are a limited beta).

## Considered Options

- **OAuth with dynamic client registration.** It is what the claude.ai web and phone apps need. But it is days of work with no AI engineering in it, all for one user.
- **No auth, with the token in a secret URL.** It works in every Claude app without OAuth, but URLs leak through logs, history and screenshots. It stays the upgrade path if asking from the phone becomes worth that risk.
- **A local stdio server reading a copy of the database.** Nothing is exposed, but the copy is always stale and needs a sync step.

## Consequences

- Claude on the web and on the phone can't reach the Corpus in v1.
- The token is the only thing between the internet and the Corpus. It is long and random, kept as a deployment secret and in the clients' config, and rotated by changing the secret.
