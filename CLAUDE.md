# Working on Gravy

Conventions for coding agents implementing this repository.

## Read first

- `docs/ARCHITECTURE.md` — interfaces and layering. **This is the contract.**
- `docs/BACKLOG.md` — your ticket, its acceptance criteria, and what it must not do.
- `docs/PRODUCT.md` — why the thing you are building exists.

## The two rules

1. **All execution goes through `internal/host`.** No package outside it may import `os/exec` or
   build worktree paths directly.
2. **Core never imports presentation.** No package outside `internal/tui` and `cmd/` may import
   Bubble Tea, Lip Gloss, or Bubbles.

CI enforces both via `make lint-layers`. They are what make remote hosts and future GUIs cheap.
Do not work around the check — if a rule genuinely blocks you, say so in your summary.

## Ground rules

- **Interfaces come from `ARCHITECTURE.md` verbatim.** If one must change, change it and state it
  prominently in your run summary — other tickets are built against it.
- **Stay in scope.** Tickets list non-goals for a reason. Do not build the next ticket's work.
- **Prefer boring.** Standard library first. No new dependency without a clear reason.
- **Write the test that would have caught the bug.** Several acceptance criteria name a specific
  adversarial case; that case is the ticket's real risk, not decoration.
- **`make check` must pass** — build, vet, lint, test — before you are done.

## Style

- Standard Go. `gofmt`, no custom formatting.
- Errors wrapped with context: `fmt.Errorf("create worktree: %w", err)`.
- No panics outside `main`. Return errors.
- Comments explain *why*, not *what*. Match the density of surrounding code.
- Table-driven tests. `-race` for anything concurrent.
- Exported identifiers get doc comments. Unexported ones get them when the reason is non-obvious.

## Invariants — never violate

- **Nothing merges without recorded human approval.** No configuration flag changes this.
- **Unknown provider errors classify as `TaskFailure`, never as a quota condition.**
- **Result summary narratives are generated from the diff, never from the agent transcript.**
- **A blocked run releases its worker slot.** A blocked ticket never holds a worker.
- **Automated review is advisory.** It never changes ticket state.
- **Detection proposes; the human approves.** Never silently write detected configuration.

## Commits

Conventional commits, referencing the ticket:

```
feat(scheduler): add eligibility filtering (GR-017)
fix(host): kill process group on context cancel (GR-008)
```

## Layout

```
cmd/gravy/        binary: TUI, serve, CLI subcommands
internal/core/    domain types + state machine (zero I/O)
internal/store/   SQLite
internal/api/     service interface + JSON-RPC + client
internal/daemon/  lifecycle, scheduler loop, worker pool
internal/host/    Host interface + LocalHost (the only os/exec)
internal/provider/ Provider interface + adapters
internal/tui/     Bubble Tea client
```

Dependencies point downward only. `core` imports nothing outside the standard library.
