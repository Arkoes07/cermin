# Domain types in `internal/cermin`, with imports pointing inward

Cermin's code follows clean architecture's dependency rule, without its layer directories. The glossary types and the rules about them that need no I/O live in `internal/cermin`, which imports only the standard library. That covers Thought, Kind, Classification, candidate Topics, and which candidate Topics a Thought keeps. Adapters (`sqlite`, `claude`, `telegram`) may import `internal/cermin` and nothing else in the module. Use cases (`capture`) import both. `cmd/cermin` builds every part with its constructor. [Section 5.2 of the design](../design/high-level-design.md#52-code-layout) lists the packages and where new code goes.

Adapters are named after what they talk to, so the glossary's words stay free for the domain and the use cases. That renamed `store` to `sqlite` and `classify` to `claude`.

Before this, the types lived in the SQLite adapter, so the Claude adapter imported the database package to name a Thought, and the keep rule lived in the Claude adapter. Weeks 2 to 4 add Retrieval, Resurfacing, Ask, MCP and evaluation, which would all have imported the SQLite adapter for the same reason.

## Considered Options

- **Full clean architecture.** `entity/`, `usecase/`, `adapter/` and `infrastructure/` directories, with a port interface for every adapter. At about 1,200 lines and one developer, that roughly doubles the files and adds interfaces with one implementation each. The tests already run against a real SQLite file and an `httptest` Telegram server, so the ports would buy nothing yet.
- **Hexagonal or DDD.** Hexagonal's ports and adapters cost the same as above. DDD's aggregates and bounded contexts pay off across several contexts or teams; Cermin has one of each.
- **Domain types at the module root.** As `package cermin` imported from `github.com/arkoes07/cermin`, call sites read the same, but the package would be importable from outside the module. `internal/cermin` keeps every package private.
- **Leave the types in the SQLite adapter.** No move, but every new feature would depend on the SQLite package to name a Thought.
- **Name adapters after their job** (`store`, `classify`). `classify` is the glossary's Classification, which reads as a use case, and it would be too narrow once Claude also answers Asks and writes Digests. `store` would then be the only adapter named by job.

## Consequences

- Use cases call adapters directly rather than through interfaces, a deliberate departure from strict clean architecture. A use case declares a function type or interface for an adapter only when a second implementation arrives or a test needs a fake that a real instance can't provide, as `capture.ClassifyFunc` does for the Claude call. The first trigger is [ADR-0006](0006-claude-classification-gemini-embeddings.md)'s: the classifier seam is formalised when week 3 evaluates a second classifier.
- When that second classifier arrives, a `classify` use case combines the classifier adapters, such as `claude` and `jev`, behind one `Classify` method, which `capture` receives as its `ClassifyFunc`. `capture` doesn't change. Before then, a `classify` package would only forward the call to `claude`.
- `capture` both reads Telegram updates and runs the Capture. The Update handler is Telegram-shaped by design: commands are Telegram commands, and the Acknowledgement is a Telegram reply. The two split when a second entry point needs the same flow.
- `cmd/cermin` wires the parts by hand, with their constructors. One process with about six parts doesn't need a dependency-injection library.
- Moving to Postgres ([ADR-0003](0003-one-process-embedded-sqlite.md)) means a new `postgres` adapter, with `capture` and `cmd/cermin` switched to it. Only one database runs at a time, so it needs no interface.
- The keep rule and its threshold sit in `internal/cermin` as `KeepTopics`, so tuning them in week 3 is a domain change, tested without Claude or SQLite.
