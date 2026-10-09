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

## Go development

Before any Go coding, review, debugging, troubleshooting, or setup task, load the `samber/cc-skills-golang@golang-how-to` skill first — it routes to whichever other Go skills the task needs.

## Required Go skills

The following Go skills from `samber/cc-skills-golang` MUST always be applied when working on this project. Load them at the start of every Go-related task, regardless of whether the user explicitly mentions them.

- `samber/cc-skills-golang@golang-code-style`
- `samber/cc-skills-golang@golang-data-structures`
- `samber/cc-skills-golang@golang-design-patterns`
- `samber/cc-skills-golang@golang-documentation`
- `samber/cc-skills-golang@golang-error-handling`
- `samber/cc-skills-golang@golang-modernize`
- `samber/cc-skills-golang@golang-naming`
- `samber/cc-skills-golang@golang-safety`
- `samber/cc-skills-golang@golang-security`
- `samber/cc-skills-golang@golang-testing`
- `samber/cc-skills-golang@golang-troubleshooting`
