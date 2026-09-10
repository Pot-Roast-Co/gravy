# Gravy — v0.1 Ticket Backlog

39 tickets across seven waves, one of which is a throwaway spike. **Tickets within a wave are independent and may execute
concurrently.** Each wave depends on the one before it.

Every ticket is written to be executed by an independent coding agent with no context beyond this
file, `PRODUCT.md`, and `ARCHITECTURE.md`.

**Critical path:** GR-000 → GR-002 → GR-004 → GR-006 → GR-017 → GR-018 → GR-022 → GR-028 → GR-038.

**Concurrency summary**

| Wave | Tickets | Parallel width | Milestone |
|---|---|---|---|
| S — Spike | GR-000 | 1 | M0 — **do this first** |
| 0 — Foundation | GR-001…005 | 5 | M0 |
| 1 — Plumbing | GR-006…011 | 6 | M0 |
| 2 — Capability | GR-012…017 | 6 | M0 (013,015,016 → M1) |
| 3 — The loop | GR-018, then 019…023, 035, 036 | 1, then 7 | M0 (018,020,022); rest M1 |
| 4 — TUI | GR-024, then 025…030, 037, 038 | 1, then 8 | M0 (025…029, 037, 038); 030 M1 |
| 5 — Surface | GR-031…034 | 4 | M1 |

**M0 is 26 tickets:** GR-000, 001…012, 014, 017, 018, 020, 022, 024, 025, 026, 027, 028, 029,
037, 038. GR-029 ships the four reasons that exist in M0; GR-038 depends on GR-028 and so lands
last in Wave 4.

**GR-000 gates everything.** It is a half-day investigation whose findings may change GR-012 and
GR-021. Do not start Wave 0 in parallel with it under the assumption that the answers will be
convenient.

**Global conventions for every ticket**

- Go 1.23, standard project layout as defined in `ARCHITECTURE.md` §2.
- Respect the two layering rules (`ARCHITECTURE.md` §1.1). CI enforces them.
- Unit tests alongside the code. `make check` (build + vet + lint + test) must pass.
- No package outside `internal/host` imports `os/exec`. No package outside `internal/tui` and
  `cmd/` imports a terminal UI library.
- Public interfaces come from `ARCHITECTURE.md` verbatim. If an interface must change, say so in
  the run summary — do not change it silently.

---

# Wave S — Spike

*Do this before anything else. It is throwaway code.*

## GR-000 — claude-code driveability spike

**Goal.** Verify the four assumptions the entire architecture rests on, before building twenty
tickets of scaffolding around them.

The `claude-code` adapter sits on the critical path, and the design assumes things about a CLI
whose surface Gravy does not control. If **resume** does not work as assumed, GR-021's entire
blocked/resume mechanism changes shape — and that is much cheaper to discover now.

**Scope.** Timeboxed to half a day. A scratch directory, a throwaway Go program or shell script,
and the real `claude` binary. Answer four questions and write the answers down:

1. **Non-interactive execution** — can it be driven headless to completion in a given working
   directory, with no TTY and no prompt, and exit with a meaningful status?
2. **Structured streaming output** — is progress parseable as it happens (tool use, messages,
   token usage), or only available at exit?
3. **Session capture and resume** — can a session identifier be captured, and can a later
   invocation resume that session with an injected message and prior context intact?
4. **Permission control** — can tool use be gated by a pre-tool hook or an equivalent mechanism
   that can deny a call, as GR-035 requires?

Also record: how quota, rate-limit, and auth-expiry failures actually present (exit codes and
message text), as seed fixtures for `Classify`.

**Non-goals.** Production code. Tests. Any of this surviving into the repository — the deliverable
is `docs/SPIKE-claude-code.md`, not an implementation.

**Acceptance criteria.**
1. All four questions answered yes or no in writing, with the exact commands and flags that worked.
2. Captured output samples for at least one success and one failure, saved as fixtures for GR-012.
3. **Any "no" is written up with its architectural consequence** — specifically, if resume is
   unavailable, say what GR-021 must become instead.
4. No spike code is merged.

**Validation.** The document exists and GR-012 can be written from it without re-deriving anything.
**Deps.** none. **Route.** standard *(this is investigation, not code)*. **Milestone.** M0.

---

# Wave 0 — Foundation

*No dependencies. All five may run concurrently.*

## GR-001 — Repo scaffold and CI

**Goal.** A buildable, lintable Go repository with the layering rules enforced mechanically.

**Scope.** `go.mod` (module `github.com/pot-roast-co/gravy`, Go 1.23). Directory skeleton per
`ARCHITECTURE.md` §2 with a doc.go in each package. `Makefile` with `build`, `test`, `lint`,
`check`, `run`. `.golangci.yml`. GitHub Actions running `make check` on push and PR. A custom
lint check (`make lint-layers`, a small Go program in `tools/`) that fails when a package outside
`internal/host` imports `os/exec`, or a package outside `internal/tui` and `cmd/` imports
`bubbletea`/`lipgloss`/`bubbles`.

**Non-goals.** Any domain logic. Release tooling. Cross-compilation.

**Acceptance criteria.**
1. `make check` passes on a clean checkout.
2. `make lint-layers` fails when a deliberate violating import is added, and passes when removed.
3. CI runs `make check` and is green.
4. `go build ./...` produces `cmd/gravy` as a single binary.

**Validation.** `make check` green; layering check demonstrated failing then passing.
**Deps.** none. **Route.** cheap. **Milestone.** M0.

## GR-002 — Core domain types and state machine

**Goal.** The vocabulary of the whole system, with zero I/O.

**Scope.** `internal/core`: `Project`, `Ticket`, `Run`, `Summary`, `Attention`, `Host`
capabilities, enums for `State`, `AttentionReason`, `LandMode`, `FailureClass`. An explicit
transition table implementing `ARCHITECTURE.md` §6 with
`func Transition(from State, ev Event) (State, error)`. Illegal transitions return a typed error.
Helpers: `IsTerminal`, `IsActive`, `NeedsHuman`.

**Non-goals.** Persistence, serialization to SQL, any import beyond the standard library.

**Acceptance criteria.**
1. `internal/core` imports nothing outside the standard library.
2. Every state and every reason from `ARCHITECTURE.md` §6 and `PRODUCT.md` §8 is represented.
3. Table-driven tests cover every legal transition **and** assert that a representative set of
   illegal ones return errors.
4. The full v0.1 happy path (Draft→Backlog→Ready→Assigned→Running→Validating→Reviewing→Review
   →Landing→Done) is exercised in one test, as are the Blocked and NeedsYou re-entries.

**Validation.** `go test ./internal/core/...` with transition coverage.
**Deps.** GR-001. **Route.** standard. **Milestone.** M0.

## GR-003 — Config schema and loader

**Goal.** `~/.gravy/config.yaml` parsed, defaulted, and validated.

**Scope.** `internal/config`: struct definitions for global config — providers, routes (ordered
provider/model choices per route), worker concurrency, notification settings, timeouts, retry
budget, context token budget. Load with sane defaults, create the file and `~/.gravy/` on first
run, validate on load with actionable error messages naming the offending key. `Save` for
onboarding write-back.

**Non-goals.** Per-project config (lives in SQLite, GR-004). Reading `.gravy.yaml` from repos.

**Acceptance criteria.**
1. Missing config file produces a valid default config and writes it.
2. Malformed YAML and unknown routes produce errors naming the key and line.
3. Round-trip: `Load` → `Save` → `Load` is stable.
4. Defaults documented in the generated file as comments.

**Validation.** Unit tests over valid, malformed, and partial configs.
**Deps.** GR-001. **Route.** cheap. **Milestone.** M0.

## GR-004 — SQLite store

**Goal.** All live state, transactionally, with one writer.

**Scope.** `internal/store`: embedded numbered forward-only migrations implementing
`ARCHITECTURE.md` §5 verbatim. Open with WAL, `foreign_keys=ON`, `busy_timeout=5000`. Typed
repository methods for projects, tickets (including fractional `position` reorder), deps, runs,
validations, summaries, attention, and provider availability. Transaction helper. Use
`modernc.org/sqlite` (pure Go — keeps the single-binary promise, no cgo).

**Non-goals.** Business logic. Any knowledge of scheduling or providers.

**Acceptance criteria.**
1. Migrations run from empty to current, are idempotent on re-open, and are versioned.
2. Every table in `ARCHITECTURE.md` §5 exists with its indexes.
3. `ReorderTicket(id, before, after)` updates exactly one row using fractional positioning.
4. Concurrent readers with one writer do not error under a 100-goroutine read test.
5. Cascade deletes verified: removing a project removes its tickets, runs, and attention rows.

**Validation.** `go test ./internal/store/...` against a temp-file database.
**Deps.** GR-001, GR-002. **Route.** standard. **Milestone.** M0.

## GR-005 — Notifications

**Goal.** The human finds out that Gravy needs them.

**Scope.** `internal/notify`: `Notify(title, body string, urgency Urgency)`. Terminal bell.
OS notification via `osascript` on darwin, `notify-send` on linux, no-op elsewhere with a logged
warning. Config-gated (off, bell only, bell + OS). Rate-limited so a burst of attention items
does not produce a burst of alerts.

**Non-goals.** Sound files. Remote or push notification. Notification history.

**Acceptance criteria.**
1. Disabled config produces no output and no subprocess.
2. Unsupported platform degrades to bell without erroring.
3. Rate limiting coalesces N notifications inside the window into one.
4. The OS notification call goes through `host.Host.Exec` once GR-008 lands, or is structured to
   accept a runner interface so it can.

**Validation.** Unit tests with a fake exec runner; manual check on darwin.
**Deps.** GR-001, GR-003. **Route.** cheap. **Milestone.** M0.

---

# Wave 1 — Plumbing

*All six may run concurrently once Wave 0 lands.*

## GR-006 — API service interface and JSON-RPC transport

**Goal.** The seam that makes the TUI one client among several.

**Scope.** `internal/api`: the `Service` interface from `ARCHITECTURE.md` §8 with all request and
response types. JSON-RPC 2.0 codec over a unix socket. Server that dispatches to a `Service`
implementation. Go client implementing the same `Service` interface, so callers cannot tell local
from remote. Server-push event stream (`Events`) for ticket, run, and attention changes. Socket
at `~/.gravy/gravyd.sock` with 0600 permissions.

**Non-goals.** The daemon itself (GR-007). HTTP transport. Authentication.

**Acceptance criteria.**
1. Client and server both satisfy `api.Service`; a test asserts this at compile time.
2. Round-trip test over a real unix socket for every method.
3. Errors propagate with type and message preserved across the wire.
4. Event stream delivers pushed events to multiple concurrent clients; a slow client is dropped
   rather than blocking the daemon.
5. Socket file is 0600 and is removed on clean shutdown.

**Validation.** `go test ./internal/api/...` including a stale-socket-file case.
**Deps.** GR-002, GR-004. **Route.** standard. **Milestone.** M0.

## GR-007 — Daemon lifecycle

**Goal.** Work survives the terminal closing.

**Scope.** `internal/daemon` + `cmd/gravy serve`: start, bind socket, serve API, run the
scheduler tick loop, graceful shutdown on SIGINT/SIGTERM (stop accepting, let runs finish or
persist, close DB). Pidfile at `~/.gravy/gravyd.pid` with staleness detection. Auto-start: the
TUI spawns a detached daemon if none is running and waits for the socket. **Startup
reconciliation**: on boot, find runs marked active in the DB, check whether their recorded PIDs
are alive, re-adopt the live ones and mark the dead ones failed with an attention row.

**Non-goals.** Scheduler internals (GR-017). Worker execution (GR-018). Auto-update.

**Acceptance criteria.**
1. `gravy serve` starts and answers `Status` over the socket.
2. A second `gravy serve` detects the running daemon and exits non-zero with a clear message.
3. Stale pidfile and stale socket are detected and cleaned.
4. SIGTERM shuts down cleanly with no database corruption and no orphaned socket.
5. Killing the daemon with an active run and restarting it marks that run failed and raises
   attention rather than losing it.
6. The TUI auto-starts a daemon on first launch.

**Validation.** Integration test spawning a real daemon; kill -9 recovery case.
**Deps.** GR-006. **Route.** standard. **Milestone.** M0.

## GR-008 — Host interface and LocalHost

**Goal.** The abstraction that keeps multi-machine cheap later.

**Scope.** `internal/host`: the `Host`, `Process`, `FS`, `Caps`, `ExecSpec` types from
`ARCHITECTURE.md` §4.1. `LocalHost` implementing them over `os/exec` — **the only package in the
repository permitted to import it**. Streaming stdout/stderr (never buffer to completion),
context cancellation, timeout enforcement, process-group kill so child processes die with the
parent. Capability detection: OS, arch, RAM, GPU where cheap, and tool discovery by probing
`git`, `go`, `node`, `python3`, `xcodebuild`, `swift`, `docker` for presence and version. Worker
slot accounting.

**Non-goals.** Remote hosts. Containers. Resource limits or cgroups.

**Acceptance criteria.**
1. `Exec` streams output incrementally — verified by a test asserting output arrives before the
   process exits.
2. Cancelling the context kills the process **and its children** within 2s.
3. Timeout produces a `Timeout` exit status, not a generic error.
4. `Capabilities` returns correct OS/arch and detects at least `git` and `go` on this machine.
5. `Slots` accounting is race-free under concurrent claim/release (`-race`).

**Validation.** `go test -race ./internal/host/...` including a process-tree kill test.
**Deps.** GR-002. **Route.** standard. **Milestone.** M0.

## GR-009 — Git worktree manager

**Goal.** Isolated, disposable working copies, and safe landing primitives.

**Scope.** `internal/git`: the `Repo` interface from `ARCHITECTURE.md` §4.6. Fetch, create
worktree from target branch at `~/.gravy/projects/<slug>/worktrees/<ticket-id>/` on branch
`gravy/<ticket-id>-<slug>`, remove worktree, `CommitAll`, `Rebase` returning conflict file lists,
`Diff` against a base producing per-file status and line counts and patches. Stale-worktree
cleanup on startup (worktrees whose tickets are terminal). All git invoked via `host.Host.Exec`.

**Non-goals.** Landing (GR-022/023). Conflict resolution. Any embedded git library.

**Acceptance criteria.**
1. Creating a worktree for two tickets in one repo yields two independent directories, both
   branched from target.
2. `Rebase` onto a diverged target returns `Clean=false` with the conflicting paths listed, and
   leaves the worktree recoverable (rebase aborted).
3. `RemoveWorktree` removes the directory and prunes git's worktree metadata.
4. `Diff` line counts match `git diff --numstat` exactly.
5. Every git call goes through `Host.Exec` — asserted by the layering check.
6. **`base` is an honest parameter, never hardcoded to the target branch.** A test creates a
   worktree based on another ticket's branch rather than target. This keeps stacked tickets
   (`ARCHITECTURE.md` §10) a scheduler change later, not a rewrite of this package.

**Validation.** Integration tests against temporary repositories created in the test.
**Deps.** GR-002, GR-008. **Route.** standard. **Milestone.** M0.

## GR-010 — Provider interface, registry, and detection

**Goal.** Adding a provider means adding one package.

**Scope.** `internal/provider`: the `Provider`, `Availability`, `AgentTask`, `Handle`, `Outcome`,
`SessionRef` types from `ARCHITECTURE.md` §4.2, and `FailureClass` from §4.3. A registry keyed by ID with
`Register`, `Get`, `All`. A shared `Classify` helper taking a matcher table (exit codes plus
ordered regexes → `FailureClass`) so adapters declare patterns rather than write logic — with
**`Unknown` mapping to `TaskFailure`** as the enforced default. A `DetectAll` that probes every
registered provider on a host. A `fake` provider for testing.

**Non-goals.** Real adapters (GR-012/013). Credential storage of any kind.

**Acceptance criteria.**
1. Registry rejects duplicate IDs.
2. `Classify` with an unmatched error string returns `TaskFailure`, never `QuotaExhausted` — this
   has a dedicated test with an explanatory comment.
3. Classification records which pattern matched, for diagnosis.
4. `DetectAll` returns per-provider installed/authenticated/version without throwing when a CLI
   is absent.
5. The `fake` provider supports scripted outcomes, events, and session resume for downstream
   tests.

**Validation.** `go test ./internal/provider/...`.
**Deps.** GR-002, GR-008. **Route.** standard. **Milestone.** M0.

## GR-011 — Run log storage and streaming

**Goal.** Watch a run live, and read it afterwards.

**Scope.** `internal/runlog`: write `runs/<run-id>/agent.log` and `events.jsonl` as a run
proceeds; per-step validation logs. A tailer that streams appended lines to N subscribers with
bounded buffers, dropping slow subscribers rather than stalling the writer. Retention: prune runs
older than a configured age, keeping summaries. Expose through `api.StreamLogs`.

**Non-goals.** Log parsing per provider (GR-012/013). Search. Compression.

**Acceptance criteria.**
1. Logs are readable while the run is in flight.
2. Two concurrent subscribers both receive all appended lines.
3. A subscriber that stops reading is dropped without blocking the writer — verified with `-race`.
4. Historical logs are readable after daemon restart.
5. Pruning removes run directories but never summaries.

**Validation.** `go test -race ./internal/runlog/...`.
**Deps.** GR-004, GR-006. **Route.** standard. **Milestone.** M0.

---

# Wave 2 — Capability

*All six may run concurrently once Wave 1 lands.*

## GR-012 — claude-code adapter

**Goal.** Drive Claude Code as a managed, non-interactive worker.

**Scope.** `internal/provider/adapters/claudecode`: implement `Provider`. Detect the `claude`
binary and version; determine authentication status without prompting. Non-interactive run in the
worktree with streaming structured output, capturing turns and token usage into `Outcome`.
Capture the session identifier into `SessionRef` and implement `Resume` so a blocked run
continues rather than restarting. Install the pre-tool permission hook (GR-035 defines the
contract; ship a stub that always allows until it lands). A `Classify` matcher table for quota,
rate limit, auth expiry, and availability, each with a comment citing the observed message.

**Non-goals.** Managing credentials. Interactive mode. Anthropic API usage — this drives the
user's existing CLI and subscription.

**Acceptance criteria.**
1. A trivial ticket ("create hello.txt containing hello") runs to completion in a temp worktree
   and produces the file.
2. Streamed events surface tool use and messages during the run, not only at exit.
3. `SessionRef` captured, and `Resume` demonstrably continues prior context.
4. Timeout and turn cap terminate the run and its children.
5. `Classify` returns `TaskFailure` for an ordinary build error and `AuthExpired` for an
   unauthenticated CLI. Matcher patterns are documented with observed output.

**Validation.** Integration test against the real CLI, skipped with a clear message when
unavailable; unit tests for `Classify` against captured fixture output.
**Deps.** GR-008, GR-010. **Route.** strong *(adapter details are fiddly and the fixtures matter)*.
**Milestone.** M0.

## GR-013 — codex adapter

**Goal.** A second real provider — the proof that the abstraction holds.

**Scope.** As GR-012, for the `codex` CLI: detection, authentication status, non-interactive
execution in a worktree, streaming, session capture and resume, permission hook, `Classify`
matcher table.

**Non-goals.** Feature parity with claude-code where the CLI does not support it — report
capabilities honestly in `Availability` rather than emulating.

**Acceptance criteria.**
1. Same trivial ticket completes via codex.
2. Anything the CLI cannot do (e.g. resume, token reporting) is reported as unsupported rather
   than faked; the orchestrator degrades gracefully.
3. `Classify` table covers quota, rate limit, and auth, documented with observed output.
4. **No changes were required outside this package and one registry line.** If changes were
   needed elsewhere, say so explicitly in the run summary — that is a finding about the
   abstraction, not a detail.

**Validation.** Integration test skipped when the CLI is absent; `Classify` unit tests.
**Deps.** GR-008, GR-010. **Route.** strong. **Milestone.** M1.

## GR-014 — Validation runner

**Goal.** Decide whether agent work is actually any good.

**Scope.** `internal/validate`: `Step`, `Result`, `Runner` from `ARCHITECTURE.md` §4.7. Run steps
in order inside the worktree via `Host.Exec`, stop at the first failed **required** step, continue
past failed optional ones. Per-step timeout. Full output to `runs/<id>/validation/<step>.log`,
tail-capped output in the `Result`. Project-type detection proposing default steps: Go
(`go build ./... `, `go test ./...`, `go vet ./...`), Node (scripts from `package.json`), Python
(`pytest`), Rust (`cargo build`, `cargo test`), Xcode (`xcodebuild test`).

**Non-goals.** Deciding what to do about failure (GR-018). Coverage or quality gates.

**Acceptance criteria.**
1. Steps run in order; a failed required step stops the sequence and later steps are not run.
2. A failed optional step records a warning and execution continues.
3. Per-step timeout kills the process tree and records `Timeout`.
4. Detection proposes correct steps for a Go repo and a Node repo fixture.
5. Detection **proposes**; it never writes project config on its own.

**Validation.** `go test ./internal/validate/...` with fixture repositories.
**Deps.** GR-008. **Route.** standard. **Milestone.** M0.

## GR-015 — Context builder

**Goal.** Give the agent what its ticket needs and nothing else.

**Scope.** `internal/contextbuild`: assemble the agent prompt under a hard token budget (default
30k, configurable) in the priority order of `ARCHITECTURE.md` §7, truncating from the lowest
priority upward so degradation is graceful. Include dependency **summaries**, never transcripts.
Include project docs by capped excerpt. On retry, append the previous attempt's failure output.
Include the standing instruction that the agent must write its **`AskPath`** — an absolute path
outside the worktree, supplied in the task — and stop when genuinely blocked. Never instruct the
agent to write anything inside the worktree. Approximate token counting (chars/4) is acceptable; document it as approximate.

**Non-goals.** Per-provider prompt formatting (adapters own that). Embeddings or retrieval.
Reading whole repositories.

**Acceptance criteria.**
1. Output never exceeds the budget.
2. When over budget, project doc excerpts are dropped before dependency summaries, and the ticket
   body is never dropped.
3. Dependency summaries appear for every dependency in `ticket_deps`.
4. Retry context contains the prior failure output.
5. The escalation instruction, with its exact schema and the absolute `AskPath`, is present in
   every prompt, and that path is **outside the worktree** — asserted by test.
6. Golden-file test pins the assembled prompt for a fixture ticket.

**Validation.** `go test ./internal/contextbuild/...` with golden files.
**Deps.** GR-004. **Route.** standard. **Milestone.** M1.

## GR-016 — Router

**Goal.** Semantic routes, ordered fallbacks, and honest unavailability.

**Scope.** `internal/router`: types from `ARCHITECTURE.md` §4.4. Resolve a route to the first
choice that is installed, authenticated, not cooling down, and not already excluded in this run.
`MarkUnavailable` writes to `provider_availability` with a class-appropriate cooldown
(`QuotaExhausted` 1h or provider-reported reset; `RateLimited` 5m; `ProviderUnavailable` 15m;
`AuthExpired` until resolved). Every resolution returns a human-readable `Why` trace. `RouteLocal`
resolves to nothing in v0.1 and falls through.

**Non-goals.** Budgets. Cost optimisation. Predicting quota before hitting it.

**Acceptance criteria.**
1. Resolution walks the configured order and skips cooling-down choices.
2. `Why` explains each skip in plain language, including cooldown expiry times.
3. Cooldowns persist across daemon restart.
4. Exhausting every choice returns a typed error carrying the full trace — the caller raises
   attention rather than failing silently.
5. Expired cooldowns are ignored without needing a sweeper.
6. `RouteLocal` falls through cleanly with no configured local provider.

**Validation.** `go test ./internal/router/...` with a fake clock.
**Deps.** GR-004, GR-010. **Route.** standard. **Milestone.** M1.

## GR-017 — Scheduler

**Goal.** Deterministic assignment that can always explain itself.

**Scope.** `internal/scheduler`: implement `ARCHITECTURE.md` §4.5 exactly — ordering, **project
availability**, project requirement filter, ticket requirement filter, tooling and provider filter,
idle-then-least-busy preference, human override, route resolution. `Tick` returns assignments
without executing them. `Explain(ticketID)` answers "why is this not running?" from recorded state.
Ready gating: a ticket is eligible only when every dependency is **Done**.

**Project availability is the point of this ticket.** In serial mode (the default) a project is
available only when it has **zero tickets in flight** — nothing between Ready and Done. The next
ticket starts only after the previous one has merged. In opt-in parallel mode, availability is
"fewer than `max_concurrency` runs currently running" and tickets awaiting review do not hold the
project. The global worker pool is shared across projects, so N serial projects run N agents.

**Non-goals.** Executing runs (GR-018). Any AI in scheduling. Preemption or priority inversion
handling.

**Acceptance criteria.**
1. Ordering is `priority DESC, position ASC, created_at ASC`, verified by test.
2. **Serial mode: a project with a ticket in Review yields no new assignments**, even though no
   run is executing. A ticket in any in-flight state holds its project until it reaches Done. This
   is the central test of this ticket.
3. Serial mode: when that ticket reaches Done, the next Ready ticket in that project is assigned
   on the following tick.
4. With three serial projects and enough workers, three tickets are assigned — one per project,
   never two from the same project.
5. Parallel mode: a project with `max_concurrency: 3` assigns up to three concurrently, and
   tickets awaiting review do **not** count against the cap.
6. `Explain` reports a held ticket as "project serialized; GR-xxx awaiting your review", naming the
   blocking ticket and its state — never an unexplained idle.
7. A ticket whose dependency is merely Approved is **not** eligible; when that dependency reaches
   Done, it becomes eligible.
8. A macOS+Xcode project requirement excludes a Linux host with a `Why` entry saying so.
9. Idle host preferred over busy; least-busy chosen among busy.
10. Explicit `host_override` bypasses preference but **not** hard requirement filters.
11. `Explain` produces a readable reason for every non-running Ready ticket.
12. `Tick` is pure with respect to execution — it starts nothing.

**Validation.** `go test ./internal/scheduler/...` with fake hosts and fixture tickets.
**Deps.** GR-004, GR-008, GR-016. **Route.** standard. **Milestone.** M0
*(with a single hardcoded route until GR-016 lands).*

---

# Wave 3 — The loop

*GR-018 first. Then GR-019 through GR-023 and GR-035 may run concurrently.*

## GR-018 — Run orchestrator

**Goal.** The engine. One ticket, start to review, unattended.

**Scope.** `internal/agentrun`: implement the run lifecycle of `ARCHITECTURE.md` §6.1. Claim
slot, **fetch the remote and then create the worktree from the freshly-fetched target branch**
(per ticket, at claim time — never a batch fetch), build context, resolve route, launch agent,
stream events to runlog, classify outcome, run validation, apply the bounded self-correction budget (default 2
retries, failure output appended to context, optional bump to the `strong` route on the final
attempt), then transition to Reviewing and release the slot. Persist run rows including PID for
reconciliation. Enforce wall-clock timeout and turn cap. Support `Kill`.

**Non-goals.** Summary generation (GR-019). Automated review (GR-020). Escalation (GR-021).
Landing (GR-022). Wire clean extension points for each.

**Acceptance criteria.**
1. With the fake provider, a ticket goes Ready → Reviewing with a worktree, commits, and
   validation results recorded.
2. **The worktree is created from freshly-fetched target state.** A test in which a commit lands
   on the remote target between ticket creation and claim must produce a worktree containing that
   commit. This is what makes queued tickets pick up previously merged work.
3. A validation failure retries up to the budget, each retry receiving the prior failure output.
4. Budget exhaustion transitions to NeedsYou with reason `validation_failed`, never a hang.
5. `QuotaExhausted` cools the model down and re-resolves the route **without** consuming a
   self-correction retry — the two budgets are independent, and a test asserts this.
6. The worker slot is released on every exit path, including panic — verified by a test that
   panics inside the provider.
7. `Kill` terminates the agent and its children and frees the slot.
8. The run row carries provider, model, retries, duration, and PID.

**Validation.** `go test -race ./internal/agentrun/...` with the fake provider and a temp repo.
**Deps.** GR-009, GR-012, GR-014, GR-015, GR-017. **Route.** strong *(this is the critical path)*.
**Milestone.** M0.

## GR-019 — Result summary generator

**Goal.** The durable record dependent tickets consume instead of a transcript.

**Scope.** `internal/summary`: mechanical half from git and validation state — branch, commits,
per-file line counts, validation results and exit codes, provider/model, retries, duration. The
narrative half generated on the `cheap` route **from `git diff`** — what changed, decisions,
assumptions, interfaces added, notes for dependents. Extract `assumptions` as a structured list.
Persist to the `summaries` table and `~/.gravy/summaries/<ticket-id>.md`.

**Non-goals.** Summarising the agent transcript. Asking the implementing agent to summarise its
own work — this is deliberate; see `ARCHITECTURE.md` §7.

**Acceptance criteria.**
1. The mechanical half is derived entirely from git and validation records, never from model
   output.
2. The narrative prompt receives the diff and the ticket — **never the agent transcript**; a test
   asserts the transcript is absent from the prompt.
3. Assumptions are extracted as a structured list and stored separately.
4. Summary generation failure degrades to the mechanical half plus a warning; it never fails the
   run.
5. `contextbuild` consumes the stored summary for dependent tickets.

**Validation.** `go test ./internal/summary/...` with a fixture diff and a stubbed model.
**Deps.** GR-018. **Route.** standard. **Milestone.** M1.

## GR-020 — Automated review agent

**Goal.** Triage that earns enough trust to let the human skim.

**Scope.** `internal/review`: run a review-route model over the diff, ticket, and validation
results, producing a **structured** verdict — overall `pass | concerns | fail`, plus findings with
severity, file, line, and rationale. Persist with the run. Deliberately conservative: report what
is defensible, not everything imaginable. Prompt must state that this is advisory input to a human
reviewer, not a gate.

**Non-goals.** Blocking or auto-approving on the verdict — it is advisory only, always. Style
nitpicking. Re-running validation.

**Acceptance criteria.**
1. Verdict is structured and machine-readable, rendering directly on the Review screen.
2. Findings carry file and line where determinable.
3. A failed or unparseable review degrades to "review unavailable" and never blocks reaching
   Review.
4. The verdict **never** changes ticket state — asserted by test.
5. Prompt and diff stay within the token budget for a large diff (truncate by file, noting what
   was omitted).

**Validation.** `go test ./internal/review/...` with fixture diffs and a stubbed model.
**Deps.** GR-018. **Route.** review. **Milestone.** **M0.**

> **Why M0.** Serial mode makes human review latency the gate on repository throughput
> (`PRODUCT.md` §10). Anything that lets the human approve routine work confidently in seconds
> directly unblocks the queue, so the advisory review pass is load-bearing for M0 rather than
> polish. It remains advisory: it never changes ticket state.

## GR-021 — Escalation and resume

**Goal.** An agent never waits silently, and never holds a worker while blocked.

**Scope.** `internal/agentrun` extension implementing `ARCHITECTURE.md` §6.2: detect the run's
`AskPath` (`~/.gravy/runs/<run-id>/ask.json`, **outside the worktree**) after a run exits, parse both
`question` and `permission` variants, commit WIP (`gravy: wip before <reason>`), **release the
worker slot**, retain the worktree, record `session_ref`, transition the ticket to `Blocked`,
create the attention row, and notify. On answer, return the ticket to Ready and, when next
scheduled, resume via `Provider.Resume` with the answer injected rather than starting fresh.

**Non-goals.** The permission allowlist itself (GR-035). The Needs You UI (GR-029).

**Acceptance criteria.**
1. A fake provider writing `AskPath` and exiting produces a Blocked ticket, an attention row, and
   a notification.
2. **The WIP commit contains no Gravy-owned files.** A test asserts the escalation file is absent
   from the worktree, from `git status`, and from the commit. Gravy writes nothing into a repo.
3. **The worker slot is released while blocked** — a second ticket starts immediately. This is the
   central test of this ticket.
4. WIP is committed; the worktree survives; nothing is lost.
5. Answering resumes via `Resume` with the answer present in the resumed context, verified by the
   fake provider recording what it received.
6. A malformed escalation file is treated as a generic failure rather than crashing the run.
7. A provider that cannot resume degrades to a fresh run whose context includes prior work and
   the answer.

**Validation.** `go test -race ./internal/agentrun/...` covering block, slot release, and resume.
**Deps.** GR-018. **Route.** strong. **Milestone.** M1.

## GR-035 — Permission broker

**Goal.** Unattended agents that neither auto-grant nor silently hang.

**Scope.** `internal/permission`: the `Allowlist`, `Pattern`, `Broker` types from
`ARCHITECTURE.md` §4.8. Glob and regex matching for read paths, write paths, and commands.
`Check` returns Allow or Escalate. `Grant` with `ScopeOnce` or `ScopeProject`, the latter
appending to the project allowlist in SQLite. The provider-side pre-tool hook contract: consult
the broker, and on Escalate deny the tool call and write the run's `AskPath` (outside the
worktree) with `type: "permission"`, which stops the agent and triggers GR-021. Seeded defaults per project type:
read and write anywhere in the worktree, the project's declared validation commands, common
read-only shell (`ls`, `cat`, `grep`, `find`, `git status|diff|log`), network off.

**Non-goals.** Sandboxing or OS-level enforcement — this is cooperative, mediated by the provider
hook, and `PRODUCT.md` §12 says so plainly. Cross-project global allowlists.

**Acceptance criteria.**
1. An allowlisted command returns Allow with the matching rule identified for display.
2. A non-allowlisted command returns Escalate; the hook denies the call and writes `AskPath`,
   which is outside the worktree.
3. `ScopeProject` persists to the project allowlist, and the identical action is subsequently
   allowed — including after a daemon restart.
4. `ScopeOnce` does **not** persist.
5. Writes outside the worktree escalate even when the command itself is allowlisted.
6. Seeded defaults for a Go project allow `go build` and `go test` without escalation.
7. Pattern matching is tested against traversal attempts (`../`) and shell chaining
   (`;`, `&&`, `|`) — an allowlisted prefix must not smuggle an arbitrary suffix.

**Validation.** `go test ./internal/permission/...` including the evasion cases in AC 7.
**Deps.** GR-010, GR-021. **Route.** strong *(security-adjacent matching logic)*. **Milestone.** M1.

## GR-022 — Land in merge mode

**Goal.** Approved work reaches the target branch, safely.

**Scope.** `internal/git` extension plus orchestration implementing `ARCHITECTURE.md` §6.3: on
approval, fetch, rebase the worktree onto the current target, then squash-merge into target and
push. Re-run validation **only when the rebase actually replayed commits** — if the branch was
already on top of target, which is the common case under the conservative per-repository default,
there is nothing to re-validate. On success: ticket Done, worktree removed, dependents
re-evaluated for Ready, worker released. On rebase conflict: abort the rebase, **preserve the
worktree**, raise NeedsYou `merge_conflict` with the conflicting file list. On red re-validation:
NeedsYou `validation_failed`.

**Non-goals.** PR mode (GR-023). **Any automatic conflict handling whatsoever** — no resolution,
no retry loop, no conflict-resolving agent, no three-way merge cleverness. A conflict stops and
asks the human. Keeping this deliberately dumb is the ticket's main constraint; an integration
agent is a later addition only if real usage shows conflicts are frequent.

**Acceptance criteria.**
1. Clean case: squash commit on target referencing the ticket, pushed, ticket Done, worktree gone.
2. **No-op rebase skips re-validation.** A branch already on top of target lands without re-running
   tests.
3. **A rebase that replayed commits re-validates first** — a test where the code is green
   pre-rebase and red post-rebase must reach NeedsYou, not merge. This is the point of the gate.
4. Conflict case: rebase aborted, **worktree preserved and usable**, attention row lists the
   conflicting files, ticket parked in Needs You. Gravy attempts nothing further.
5. After the human resolves the conflict in the worktree, an explicit "continue" retries the land
   and succeeds.
6. Dependent tickets become Ready only after the dependency reaches Done.
7. Push failure (rejected, no remote) escalates cleanly and leaves local state consistent.
8. Nothing merges without a recorded human approval — asserted by test.

**Validation.** Integration tests against temp repos, covering no-op rebase, replayed-commit
rebase, and the conflict/resolve/continue path.
**Deps.** GR-009, GR-018. **Route.** strong *(this is the merge gate)*. **Milestone.** M0.

## GR-023 — Land in PR mode

**Goal.** Projects where Gravy must not write to the target branch.

**Scope.** Same rebase gate as GR-022, then push the branch and create a PR via the `gh` CLI with
a body assembled from the ticket, summary, and validation results. Ticket → Done with the PR URL
recorded and displayed. `merge_mode` is per-project config.

**Non-goals.** Tracking PR state after creation. Reacting to CI. Non-GitHub forges.

**Acceptance criteria.**
1. `merge_mode: pr` pushes the branch and opens a PR; the target branch is untouched.
2. PR body contains ticket, summary, and validation results.
3. Missing or unauthenticated `gh` escalates to NeedsYou with actionable instructions — it does
   not fall back to merging.
4. The PR URL is recorded on the ticket and shown in Done.
5. The rebase gate behaves identically to merge mode.

**Validation.** Integration test with a stubbed `gh`; manual check against a real repo.
**Deps.** GR-022. **Route.** standard. **Milestone.** M1.

## GR-036 — Merge helper

**Goal.** Turn a merge conflict into one keystroke instead of one context switch — without
creating a privileged path around the merge gate.

**Scope.** An agent run scoped to exactly one job: resolve the conflicts in a preserved worktree.
Invoked **only** by explicit human action from a `merge_conflict` item in Needs You. Prompt
contains the conflicted files, both sides of each hunk, the ticket, and the summary of the work it
is landing on top of. It runs `git rebase --continue` to completion, then the result **re-enters
the normal path**: validation runs, then human review, then landing. Reuses `agentrun`,
`validate`, and the existing review flow — this is a narrow prompt plus an entry point, not a new
subsystem.

**Non-goals.** Automatic invocation — never, under any configuration. Resolving anything other
than conflicts. Refactoring, redesign, or "while I'm here" changes. Bypassing validation or human
review. Any special-casing of its output.

**Acceptance criteria.**
1. Invoked only from an explicit Needs You action; no code path calls it automatically — asserted
   by test.
2. A textual conflict in a fixture repo is resolved, the rebase completes, and the branch is
   mergeable.
3. **The result goes through validation and human review before landing**, exactly like any other
   change. This is the central test of this ticket.
4. Failure to resolve returns the ticket to `merge_conflict` with the worktree still preserved and
   the attempt logged — it never leaves a half-finished rebase.
5. The prompt constrains the agent to conflict resolution; a test asserts the ticket's original
   implementation instructions are **not** included, so it cannot wander into new work.
6. Semantic conflicts it cannot safely resolve are reported as such rather than guessed at.

**Validation.** Integration test with a fixture conflict; a test asserting no automatic invocation.
**Deps.** GR-022, GR-029. **Route.** strong *(conflict resolution is judgement work)*.
**Milestone.** M1.

---

# Wave 4 — TUI

*GR-024 first. Then GR-025 through GR-030 may run concurrently — one screen each, no shared
files beyond the shell's registry.*

## GR-024 — TUI shell

**Goal.** The frame every screen plugs into.

**Scope.** `internal/tui` + `cmd/gravy` default command: Bubble Tea app with a screen registry,
persistent status bar (project, worker usage, daemon health, open attention count), global keymap
(`1-7` sections, `p` project switcher, `?` help overlay, `q` quit, `/` filter), Lip Gloss theme
tokens, and an event-stream subscription that re-renders on push rather than polling. Daemon
auto-start with a clear "starting gravy daemon…" state. Graceful degradation when the daemon is
unreachable.

**Non-goals.** Any individual screen's content. Mouse support. Themes beyond one good default.

**Acceptance criteria.**
1. `gravy` launches, auto-starts the daemon if needed, and renders the frame.
2. Global keys work from every screen; `?` lists them, generated from the keymap so it cannot
   drift.
3. Layout is correct at 80x24 and adapts to resize without panicking.
4. Daemon unreachable renders an actionable error state, not a crash or a hang.
5. Server-push events update the UI with no polling loop in the code.
6. **The TUI contains no domain logic** — it only calls `api.Service`. Enforced by review.

**Validation.** `go test ./internal/tui/...` with `teatest`; manual resize and daemon-down checks.
**Deps.** GR-006. **Route.** standard. **Milestone.** M0.

## GR-025 — Dashboard

**Goal.** One screen that replaces the terminal juggling — "what needs me, and what is happening,
across every project?"

This screen is the primary deliverable of M0. The product's core value is organization
(`PRODUCT.md` §1), and this is where organization is either delivered or not.

**Scope.** Default screen. Three sections in fixed priority order: **Needs You** (with reason and
age), **Running** (ticket, **project**, **branch**, provider/model/host, elapsed, current
activity), **Ready** (next up, in queue order). Every row names its project — the screen spans all
projects at once, because remembering which agent is on which repository is the exact pain being
removed. Counts in section headers. `enter` jumps to the relevant detail screen. Empty states that
say what to do next.

**Non-goals.** Editing. Charts or analytics. Configurable layout.

**Acceptance criteria.**
1. Section order is always Needs You → Running → Ready.
2. **All projects appear on one screen**, each row labelled with its project and branch. A user
   with three active repositories sees all three without switching context.
3. Each Running row shows provider, model, and host.
4. `enter` on any row opens the correct detail screen.
5. Live updates via the event stream, no polling.
6. Sections degrade gracefully when long (scroll or truncate with a count, never overflow).
7. Empty state reads as guidance, not as an error.
8. A ticket held back by its project's concurrency cap shows that as its status, not as an
   unexplained absence from Running.

**Validation.** `teatest` snapshots for populated and empty states.
**Deps.** GR-024. **Route.** standard. **Milestone.** M0.

## GR-026 — Backlog and Ready screens

**Goal.** Create and order work at speed.

**Scope.** List view with filter. Quick ticket creation (`n`) in a modal — title, body, route,
optional dependencies — writing to Backlog **immediately**. Reorder with `J`/`K` (fractional
positioning). Priority with `+`/`-`. State moves: Backlog↔Ready (`space`). Edit (`e`), delete
(`D` with confirmation). Multi-select for bulk state moves. Dependency display with a warning when
a ticket is Ready-blocked by an unlanded dependency.

**Non-goals.** Assisted or Planned creation (v0.2). Dependency graph visualisation. Drag and drop.

**Acceptance criteria.**
1. `n` → type → save lands a ticket in Backlog with no blocking network call. Creation never
   waits on the critique.
2. Reorder updates exactly one row; order survives restart.
3. **Revised.** A ticket with an unlanded dependency *can* be queued; the reason it is waiting is
   displayed, and the scheduler holds it until the dependency is Done. Originally this move was
   refused, on the grounds that a queued-but-blocked ticket would look eligible while the
   scheduler passed over it. It does not: the scheduler records the dependency as the reason and
   every queue screen draws it. The refusal only meant a plan arriving as a chain had to be
   queued one ticket at a time, as each predecessor landed.
4. Bulk move works over a multi-selection.
5. Delete confirms first.
6. Filter matches title and body.

**Validation.** `teatest` for create, reorder, and blocked-move flows.
**Deps.** GR-024. **Route.** standard. **Milestone.** M0.

## GR-027 — Running detail

**Goal.** See what an agent is doing, and stop it.

**Scope.** Per-run detail: ticket, project, branch, host, provider, model, route trace, elapsed,
retry count, turns, token usage where reported. Live log tail with follow mode (`f`) and scrollback.
Kill (`K`, confirmed). Open the worktree in `$EDITOR` (`e`) or a shell (`!`).

**Non-goals.** Interacting with the agent mid-run. Log search. Editing the ticket while running.

**Acceptance criteria.**
1. Log tail follows live output and can be paused and scrolled back.
2. Kill terminates the agent and its children, frees the slot, and updates within one second.
3. Route trace explains why this provider and model were chosen.
4. Retry count and the reason for each retry are visible.
5. Shell and editor escapes return cleanly to the TUI.

**Validation.** `teatest` with a fake streaming run; manual kill test against a real provider.
**Deps.** GR-011, GR-024. **Route.** standard. **Milestone.** M0.

## GR-028 — Review screen

**Goal.** Approve or reject in seconds for routine work, with depth one keystroke away.

**Scope.** Progressive disclosure per `PRODUCT.md` §9. Compact card: project, ticket, branch,
worker/host/provider/model, retries, duration, per-step validation with exit codes, automated
review verdict, agent summary with **assumptions flagged amber**, changed files with line counts.
`enter` expands a file's diff inline with syntax highlighting. `e` `$EDITOR`, `d` `git difftool`,
`!` shell, `l` logs. `a` approve, `r` request changes (feedback prompt), `x` reject (confirmed).

**Non-goals.** Side-by-side diffs. Inline hunk comments. Editing the diff. These are explicitly
out of v0.1.

**Acceptance criteria.**
1. The compact card fits an 80x24 terminal without scrolling for a typical ticket.
2. Assumptions from the summary render in amber and are impossible to miss.
3. `enter` expands and collapses per-file diffs; large files truncate with a count and an offer to
   open externally.
4. `a` triggers landing and moves the ticket out of the review queue immediately.
5. `r` prompts for feedback, returns the ticket to Ready, and preserves the worktree; the feedback
   reaches the agent's next context.
6. `x` confirms, then closes the ticket and removes the worktree.
7. External escapes return cleanly to the TUI.

**Validation.** `teatest` for approve, request-changes, and reject; manual pass on a real diff.
**Deps.** GR-024, GR-020. **Route.** standard. **Milestone.** M0
*(minus the automated-review panel until GR-020 lands).*

## GR-029 — Needs You screen

**Goal.** One queue, every reason, always actionable.

Together with GR-025 this is the heart of M0. "One place where things need you" is the product
claim; this screen is that place.

**M0 cut / M1 completion.** M0 ships the reasons that exist in M0 — `review_pending`,
`validation_failed`, `merge_conflict`, `host_unavailable`. `agent_question` and
`permission_request` arrive with GR-021 and GR-035; `ticket_critique` with GR-031. Build the
per-reason renderer as a registry so the later reasons are additions, not edits.

**Scope.** Unified attention list ordered by age, filterable by reason and project. Per-reason
detail panes and actions: `review_pending` → jump to Review; `agent_question` → show question,
options, and context, answer inline, resume; `permission_request` → show requested action and
rationale, **allow once / always allow for this project / deny**; `validation_failed` → failing
step output, send back with guidance, or reject; `merge_conflict` → list conflicting files, open a
shell or `$EDITOR` in the **preserved** worktree, a "continue" action that retries the land once
resolved, and (M1, GR-036) "ask the merge helper to resolve it"; `provider_auth` → re-auth
instructions, disable provider;
`ticket_critique` → per-suggestion accept/edit/dismiss; `host_unavailable` → acknowledge.

**Non-goals.** Resolving conflicts *for* the human in-TUI — Gravy opens the preserved worktree and
gets out of the way, or delegates to the merge helper on request. Editing provider credentials.

**Acceptance criteria.**
1. Every reason from `PRODUCT.md` §8 renders with its own actions; an unknown reason degrades to a
   readable generic view rather than crashing.
2. Answering an `agent_question` resumes the run with the answer.
3. "Always allow for this project" persists to the allowlist and the same action never asks again.
4. Resolving an item removes it from the queue and updates the dashboard count immediately.
5. Items carry enough context to act **without leaving the screen** for the common cases.
6. Ordering is oldest-first so nothing starves.
7. `merge_conflict` opens a shell in the preserved worktree, and "continue" retries the land
   successfully after the human resolves it.
8. A repository whose queue is held by serial mode shows the blocking ticket by name, so an idle
   queue is never unexplained.

**Validation.** `teatest` covering every reason type available at the milestone.
**Deps.** GR-024, GR-022 *(M0 cut)*; GR-021, GR-035, GR-031, GR-036 *(remaining reasons, M1)*.
**Route.** standard. **Milestone.** **M0** *(review_pending, validation_failed, merge_conflict,
host_unavailable)*; remaining reasons M1.

## GR-030 — Done and history

**Goal.** What shipped, and what it cost.

**Scope.** Completed tickets with merge commit or PR URL, duration, provider/model, retries, and
token or cost figures where reported. Filter by project and date. Open the summary (`enter`), the
run logs (`l`), or the commit (`o`).

**Non-goals.** Analytics or charts. Cross-project rollups. Export.

**Acceptance criteria.**
1. Done tickets list newest first with merge or PR reference.
2. Summaries and archived logs are readable after restart.
3. Cost and token columns render "—" when a provider does not report them, never zero.
4. Filters work and persist within a session.

**Validation.** `teatest` snapshot with fixture history.
**Deps.** GR-024. **Route.** cheap. **Milestone.** M1.

## GR-037 — Project registration

**Goal.** Make M0 usable at all: a way to add a repository without the onboarding wizard.

M0 is 26 tickets and, without this, contains no path to create a project — `AddProject` exists on
the API but nothing calls it until GR-032 in M1. Since M0's entire purpose is a two-week
dogfooding trial, that is a blocking gap, not a nicety.

**Scope.** `gravy project add <path>` and `gravy project list` in `cmd/gravy`, plus an "add
project" action in the TUI project switcher. Registers the repository, resolves the target branch
from the remote's HEAD, and accepts `--target-branch`, `--merge-mode`, and `--validate` (repeatable)
flags. Validates that the path is a git repository with a remote before accepting it.

**Non-goals.** Detection, suggestion, or any wizard flow — that is GR-032. This ticket takes what
it is given and writes it down.

**Acceptance criteria.**
1. `gravy project add ~/projects/foo` registers the project and it appears in the TUI switcher.
2. A path that is not a git repository, or has no remote, is rejected with a clear message.
3. Target branch is resolved from the remote's HEAD when not given explicitly.
4. Validation steps supplied on the command line are stored and used by runs.
5. The TUI action and the CLI command share one code path through `api.Service`.
6. **A fresh install can reach a first landed ticket using only this ticket's surface** — no YAML
   editing, no wizard.

**Validation.** Integration test against a temp repo with a remote; manual first-project walkthrough.
**Deps.** GR-006, GR-024. **Route.** standard. **Milestone.** M0.

## GR-038 — Review sweep mode

**Goal.** Make clearing the review queue fast, because review latency now gates throughput.

Serial mode (`PRODUCT.md` §10) means a ticket sitting in Review blocks its repository. The highest-
leverage optimisation is therefore reviewing *faster*, not executing faster. This is `git add -p`
for tickets.

**Scope.** One keystroke from the Dashboard or Needs You enters a sequential walk of every pending
review, newest-blocking-first. Each step renders the GR-028 review card. `a` approve and advance,
`r` request changes and advance, `x` reject and advance, `s` skip and advance, `q` exit the sweep.
A progress indicator ("3 of 7"). The sweep never returns to a list between items. Full-depth
escapes (`e`, `d`, `!`, `enter`) remain available and return into the sweep at the same position.

**Non-goals.** Bulk approve — every ticket is still approved individually and deliberately. Any
change to approval semantics. A separate review screen: this is a mode over GR-028, not a fork of
it.

**Acceptance criteria.**
1. The sweep visits every pending review exactly once and exits cleanly at the end.
2. Approving inside the sweep triggers landing identically to approving from the Review screen —
   shared code path, asserted by test.
3. Ordering puts repository-blocking reviews first, so the sweep unblocks queues soonest.
4. Escaping to `$EDITOR` or a shell and returning resumes at the same position.
5. Exiting mid-sweep leaves all unvisited tickets untouched.
6. **No bulk-approve affordance exists** — one keystroke approves one ticket, never several.

**Validation.** `teatest` over a fixture queue of five pending reviews, including mid-sweep exit.
**Deps.** GR-028. **Route.** standard. **Milestone.** M0.

---

# Wave 5 — Product surface

*All four may run concurrently.*

## GR-031 — Async ticket critique

**Goal.** Better tickets without slowing down ticket writing.

**Scope.** On ticket creation, enqueue a background critique on the `review` route. The reviewer
identifies ambiguous requirements, missing acceptance criteria, missing validation, likely
dependencies, scope problems, and missing project context, returning **structured proposed
changes** — never a rewritten ticket. Results arrive as a `ticket_critique` attention item where
each suggestion is individually accepted, edited, or dismissed.

**Non-goals.** Blocking ticket creation — this is the point of the ticket. Auto-applying
suggestions. Assisted or Planned creation modes.

**Acceptance criteria.**
1. Ticket creation returns immediately; the critique runs in the background.
2. The critique **never mutates the ticket**; it only proposes. Asserted by test.
3. Each suggestion is individually accept / edit / dismiss.
4. Accepting applies only the accepted suggestions, preserving the human's other text verbatim.
5. Critique failure is silent apart from a log line — it never blocks or dirties the queue.
6. The feature can be disabled in config, and then no critique run is started at all.

**Validation.** `go test ./internal/review/...` for critique; `teatest` for the accept flow.
**Deps.** GR-020, GR-029. **Route.** review. **Milestone.** M1.

## GR-032 — Onboarding wizard

**Goal.** A fresh machine reaches a first landed ticket without hand-editing YAML.

**Scope.** First-run flow, also reachable from Settings: detect installed provider CLIs and
authentication status; list discoverable models; show local host capabilities; set worker
concurrency; add the first repository; detect project type and tooling; **suggest** host
requirements (an Xcode project suggests macOS + Xcode); **suggest** validation commands; seed the
permission allowlist for the detected project type; configure routes with fallbacks. Every
detected value is presented for approval, editable, and never silently applied.

**Non-goals.** Provider credential management — Gravy uses existing CLI authentication and says
so. Cloud sync. Importing tickets from other systems.

**Acceptance criteria.**
1. First run with no `~/.gravy/` completes end to end and writes valid config plus one project.
2. Every detected value is shown and requires explicit approval before being saved.
3. An iOS/Xcode repo suggests macOS + Xcode requirements and an `xcodebuild test` validation step.
4. A Go repo suggests `go build`/`go test`/`go vet`.
5. An unauthenticated provider is reported clearly with instructions, and does not block setup.
6. Re-running from Settings edits existing config without clobbering unrelated values.
7. Quitting midway leaves no partial or invalid config.

**Validation.** `teatest` over the full flow with fake detection; manual first-run on a clean
`~/.gravy/`.
**Deps.** GR-010, GR-014, GR-024. **Route.** standard. **Milestone.** M1.

## GR-033 — CLI subcommands

**Goal.** Prove the core/presentation seam with a second client.

**Scope.** `cmd/gravy` subcommands against `api.Service`: `ticket add`, `ticket list`,
`ticket show`, `status`, `approve <id>`, `logs <run-id>`, `serve`, and **`context <ticket-id>`** —
print the exact prompt the agent would receive, without running anything. Human-readable by
default, `--json` for scripting. Sensible exit codes. `gravy` with no subcommand launches the TUI.
Builds on the `project` commands from GR-037.

**Non-goals.** Full TUI parity. Shell completions. A second interactive mode.

**Acceptance criteria.**
1. Every subcommand works **with the TUI closed** — this is the point of the ticket.
2. `--json` emits valid parseable JSON for every command that supports it.
3. Exit codes are meaningful (0 success, non-zero with a message on failure).
4. No subcommand imports anything from `internal/tui`, and none reaches past `api.Service` into
   core — verified by the layering check.
5. `gravy approve` performs exactly the same landing path as the TUI, sharing the code.
6. `gravy context <id>` prints the assembled prompt — including dependency summaries and the
   token budget accounting — and **starts no run**. When an agent misbehaves, this is the first
   diagnostic: what did it actually see?

**Validation.** Integration tests against a live daemon.
**Deps.** GR-006. **Route.** cheap. **Milestone.** M1.

## GR-034 — Documentation and release

**Goal.** Someone other than the author can install and use it.

**Scope.** `README.md` — what it is, the loop, install, quickstart, a screenshot or asciicast.
`CLAUDE.md` — conventions for the agents building Gravy. `CONTRIBUTING.md`. A short guide on
adding a provider (which is the real test of the abstraction). Reconcile `PRODUCT.md` and
`ARCHITECTURE.md` against what was actually built, correcting the docs where reality diverged.
`goreleaser` config for darwin and linux, amd64 and arm64.

**Non-goals.** A website. Package manager submissions. Video.

**Acceptance criteria.**
1. A new user can install and reach a first landed ticket using only the README.
2. The provider guide is accurate enough to add a third adapter without reading the source.
3. `PRODUCT.md` and `ARCHITECTURE.md` describe the shipped system; every divergence is either
   fixed in code or corrected in the docs.
4. `goreleaser --snapshot` produces working binaries for all four targets.
5. LICENSE present and referenced.

**Validation.** A clean-machine install walkthrough; `goreleaser` snapshot build.
**Deps.** all. **Route.** standard. **Milestone.** M1.

---

## Dependency graph

```
GR-001 ─┬─► GR-002 ─┬─► GR-004 ─┬─► GR-006 ─┬─► GR-007
        ├─► GR-003  │           │           ├─► GR-011
        └─► GR-005  │           │           ├─► GR-024 ─┬─► GR-025
                    │           │           │           ├─► GR-026
                    │           │           │           ├─► GR-027
                    │           │           │           ├─► GR-028
                    │           │           │           ├─► GR-029
                    │           │           │           └─► GR-030
                    │           │           └─► GR-033
                    │           ├─► GR-015
                    │           └─► GR-016 ─► GR-017 ─┐
                    ├─► GR-008 ─┬─► GR-009 ──────────┤
                    │           ├─► GR-010 ─┬─► GR-012┤
                    │           │           └─► GR-013│
                    │           └─► GR-014 ───────────┤
                    │                                 ▼
                    │                              GR-018 ─┬─► GR-019
                    │                                      ├─► GR-020 ─► GR-031
                    │                                      ├─► GR-021 ─► GR-035
                    │                                      └─► GR-022 ─┬─► GR-023
                    │                                                  └─► GR-036 ◄─ GR-029
                    └─► GR-032, GR-034 (late, broad deps)

GR-000 ──► (informs GR-012, GR-021 — no code dependency, but do it first)
GR-024 ──┬─► GR-037   (project registration — M0 needs this to be usable at all)
         └─► GR-028 ─► GR-038   (review sweep)
```

Acyclic. Every ticket's dependencies appear earlier in the graph, except GR-036's dependency on
GR-029 (its invocation point) and GR-038's on GR-028 — both later-numbered by placement, not by
cycle.

## Notes for executing agents

- **Interfaces are contracts.** Take them verbatim from `ARCHITECTURE.md`. If one genuinely must
  change, make the change and **state it prominently in the run summary** — downstream tickets are
  built against it.
- **The two layering rules are not style preferences.** They are what makes remote hosts and
  future GUIs cheap. CI enforces them; do not work around the check.
- **Prefer boring.** No new dependency without a clear reason. Standard library first.
- **Write the test that would have caught the bug**, not the test that confirms the happy path.
  Several acceptance criteria above name a specific adversarial case — those exist because that
  case is the ticket's real risk.
