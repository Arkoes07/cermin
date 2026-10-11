# cermin

## Agent skills

### Issue tracker

Issues live in GitHub Issues on `arkoes07/cermin`, managed with the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

The five default triage labels: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `CONTEXT.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.

### Product

Scope, non-goals, success checks and milestones: `docs/product/PRD.md`. Check it before building anything new.

### Reading docs

Read only the sections a task needs, never a whole doc. `docs/design/high-level-design.md` and `docs/product/PRD.md` are long: list their headings with `grep -n '^#' <file>`, then read the matching section by line range. For ADRs, list `docs/adr/` and open only those whose title touches the area you're changing.

## Go development

Before any Go coding, review, debugging, troubleshooting, or setup task, load the `samber/cc-skills-golang@golang-how-to` skill first — it routes to whichever other Go skills the task needs.

Code layout: which package each kind of code goes in, and what it may import, is in §5.2 of `docs/design/high-level-design.md`. Read that section (not the whole doc) before adding a package or a type, or moving code between packages.

### Code style

These override `golang-code-style` and `golang-naming` where they differ.

- A call or signature stays on one line up to 120 columns, counting a tab as 4. Past that, one argument per line.
- Past 120, a `slog` call keeps `ctx` and the message on its first line, then one key/value pair per line. A query call keeps the arguments before the SQL on its first line, then gives the SQL and each later argument a line of its own.
- A function that works with several kinds of things is split into methods. Any other long function gets a blank line between its step groups.
- Name a function or method with a verb for what it does: `renderAck`, not `ackText`.
- A method that reads from the database starts with `Get`: `GetThoughts`. One that finds rows by an argument adds `By` and that argument: `GetThoughtByID(ctx, id)`.
- Exception: a method that makes no database or network call, and only returns a value its type already holds, is named for that value: `Kinds()`, `KeptTopics()`, `ReplyToID()`.
- A test helper that builds a fixture is named for it: `textUpdate`, `classificationsOf`.
- A query with 2 or more parameters binds them with `sql.Named` to `:name`.
- A store method selects rows and guards writes atomically. Domain rules live in `internal/cermin`.

## Required Go skills

The following Go skills from `samber/cc-skills-golang` MUST always be applied when working on this project. Load them at the start of every Go-related task, regardless of whether the user explicitly mentions them.

- `samber/cc-skills-golang@golang-code-style`
- `samber/cc-skills-golang@golang-error-handling`
- `samber/cc-skills-golang@golang-testing`
