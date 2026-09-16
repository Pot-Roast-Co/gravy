# Gravy — Architecture (v0.1)

This document is the technical contract for building Gravy. It defines the layering rules,
package map, interfaces, data model, and the algorithms that must be deterministic and
explainable. `PRODUCT.md` says what Gravy does and why; this says how it is built.

Implementation language is **Go** — portable single binary, excellent subprocess and concurrency
handling, mature TUI ecosystem (Bubble Tea, Lip Gloss, Bubbles). Minimum Go 1.24 — Bubble Tea
v1.3 requires it, so adopting the TUI raised the floor from 1.23.

---

## 1. System shape

```
                   ┌──────────────────────────────────┐
   gravy (TUI) ────┤                                  │
   gravy ls    ────┤   api  (unix socket, JSON-RPC)   ├────► gravyd
   future GUI  ────┤                                  │        ├── scheduler
                   └──────────────────────────────────┘        ├── worker pool
                                                               │     └── agentrun
                                                               │           ├── host ──► provider CLI
                                                               │           ├── git  ──► worktree
                                                               │           └── validate
                                                               └── store (SQLite, WAL)
```

`gravyd` is the only process that owns state. It runs the scheduler, holds the worker pool,
supervises agent subprocesses, and is the single writer to SQLite. Clients — the TUI, the CLI,
anything later — are thin and stateless, talking JSON-RPC over a unix socket at
`~/.gravy/gravyd.sock`.

**Why a daemon.** Agents must keep working when the terminal closes, and idle workers must pick
up Ready tickets with nothing open. A TUI-owned scheduler cannot do either without becoming a
daemon in disguise. It also gives SQLite a single writer for free and makes every future client a
straightforward API consumer.

### 1.1 The two layering rules

These are enforced by a lint check in CI (GR-001), not merely by convention.

**Rule 1 — all execution goes through `internal/host`.**
No package outside `internal/host` may import `os/exec` or construct worktree filesystem paths
directly. Every command run and every file touched on a working machine goes through the `Host`
interface. This single rule is what makes remote hosts a later addition rather than a rewrite: a
`SSHHost` or `AgentHost` implementation drops in beneath an unchanged scheduler, run
orchestrator, provider layer, and UI.

**Rule 2 — core never imports presentation.**
No package outside `internal/tui` and `cmd/` may import Bubble Tea, Lip Gloss, or any terminal
library. Core is a library; the TUI is one client. The v0.1 CLI subcommands (GR-033) exist
specifically to keep this seam honest — a second client proves the first one is not load-bearing.

---

## 2. Package map

```
gravy/
  cmd/gravy/              # single binary: TUI (default), `serve`, CLI subcommands
  internal/
    core/                 # domain types + state machine. ZERO I/O, zero imports outside stdlib
    store/                # SQLite: schema, migrations, WAL, typed queries
    api/                  # service interface, JSON-RPC codec, server handlers, Go client
    daemon/               # lifecycle, scheduler loop, worker pool, startup reconciliation
    scheduler/            # eligibility filtering, assignment, decision trace
    host/                 # Host interface + LocalHost
    provider/             # Provider interface, registry, detection
      adapters/claudecode/
      adapters/codex/
    router/               # route table, resolution, availability cooldowns
    git/                  # worktrees, branches, rebase, land (merge | pr)
    validate/             # validation step runner
    permission/           # allowlist model, matching, escalation, write-back
    agentrun/             # run orchestrator — the state machine executor
    summary/              # result summary generation
    review/               # automated code review, ticket critique
    contextbuild/         # per-ticket context assembly under a token budget
    notify/               # terminal bell + OS notification
    tui/                  # Bubble Tea app: screens, components, keymap
  docs/
```

Dependency direction is strictly downward: `core` depends on nothing; `store`, `host`,
`provider`, `git`, `validate` depend on `core`; `agentrun` and `scheduler` compose them; `daemon`
composes those; `api` exposes `daemon`; `tui` and `cmd` consume `api`.

## 3. On-disk layout

```
~/.gravy/
  config.yaml           # global config: providers, routes, concurrency, notifications
  gravy.db              # SQLite (WAL) — all live state
  gravyd.sock           # unix socket
  gravyd.pid            # daemon pidfile
  projects/<slug>/
    worktrees/<ticket-id>/     # isolated worktree per active ticket
  runs/<run-id>/
    agent.log                  # raw provider stdout/stderr
    events.jsonl               # parsed structured events
    validation/<step>.log
    ask.json                   # present iff the run escalated
  summaries/<ticket-id>.md     # durable result summaries
```

Repositories are never written to by Gravy for bookkeeping. An optional `.gravy.yaml` in a repo
is read if present and never written.

---

## 4. Core interfaces

### 4.1 Host

```go
package host

type Host interface {
    ID() string
    // Capabilities may block on its first call for a host; every call after it answers from
    // memory and refreshes behind the caller. The scheduler tick and the dashboard rely on
    // that: a machine that is switched off does not refuse a connection, it never answers,
    // so probing one inline stalls whatever asked.
    Capabilities(ctx context.Context) (Caps, error)
    // Reachability is what was last learned about the machine, from memory, never the network.
    Reachability() Reachability
    // Recheck probes now and waits, whatever is cached: the human saying they have just
    // switched the machine back on.
    Recheck(ctx context.Context) (Caps, error)
    Exec(ctx context.Context, spec ExecSpec) (Process, error)
    FS() FS
    Slots() (used, total int)
}

// Reachability is remembered rather than discovered on demand, so that a configured host which
// is off is reportable as off instantly and indefinitely — the cost of finding out must not be
// paid by every caller that merely wants to name it.
type Reachability struct {
    Online    bool      // the last probe succeeded
    Err       string    // why not, verbatim from ssh
    CheckedAt time.Time // zero means never probed
    Checking  bool      // a probe is in flight
}

type Caps struct {
    OS        string            // darwin, linux, windows
    Arch      string
    RAMBytes  uint64
    GPU       string
    Tools     map[string]string // name -> version: git, go, node, xcodebuild, ...
    Providers map[string]bool   // provider id -> installed
}

type ExecSpec struct {
    Cmd     string
    Args    []string
    Dir     string
    Env     map[string]string
    UnsetEnv []string // remove inherited and explicitly supplied variables; removal wins
    Timeout time.Duration
    Stdin   io.Reader
}

type Process interface {
    Stdout() io.Reader        // streaming, not buffered-to-completion
    Stderr() io.Reader
    Wait() (ExitStatus, error)
    Kill() error
    PID() int
}

type ExitStatus struct {
    Code     int
    Signaled bool
    Duration time.Duration
}

type FS interface {
    ReadFile(path string) ([]byte, error)
    WriteFile(path string, b []byte, perm os.FileMode) error
    Stat(path string) (os.FileInfo, error)
    MkdirAll(path string, perm os.FileMode) error
    RemoveAll(path string) error
    Exists(path string) bool
}
```

v0.1 implements `LocalHost` only. `Slots()` reflects configured worker concurrency.

### 4.2 Provider

```go
package provider

type Provider interface {
    ID() string
    Detect(ctx context.Context, h host.Host) (Availability, error)
    Models(ctx context.Context) ([]Model, error)
    Run(ctx context.Context, h host.Host, t AgentTask) (Handle, error)
    Resume(ctx context.Context, h host.Host, s SessionRef, msg string) (Handle, error)
    Classify(exit int, stdout, stderr string) FailureClass
}

type Availability struct {
    Installed     bool
    Version       string
    Authenticated bool
    Detail        string   // human-readable, shown in onboarding and Needs You
}

type AgentTask struct {
    RunID        string
    WorktreePath string
    Prompt       string          // built by contextbuild; agents never get transcripts
    Model        string
    Timeout      time.Duration
    MaxTurns     int
    LogPath      string
    AskPath      string          // ~/.gravy/runs/<run-id>/ask.json — OUTSIDE the worktree
    Allowlist    permission.Allowlist
}

type Handle interface {
    Events() <-chan Event         // parsed progress: tool use, message, token usage
    Wait() (Outcome, error)
    Kill() error
}

type Outcome struct {
    Class     FailureClass
    Session   SessionRef          // opaque, provider-specific; enables Resume
    ExitCode  int
    Turns     int
    TokensIn  int
    TokensOut int
    CostUSD   *float64            // nil when the provider does not report it
}

type SessionRef struct {
    ProviderID string
    ID         string
}
```

Providers are registered in a registry keyed by ID. Adding a provider means adding one package
and one registry entry — no changes anywhere else in the application.

### 4.3 Failure classification

```go
type FailureClass int

const (
    Success FailureClass = iota
    TaskFailure          // the agent failed at the work — retryable by self-correction
    QuotaExhausted       // subscription/quota genuinely exhausted — cooldown the model
    RateLimited          // transient rate limit — short cooldown
    ProviderUnavailable  // CLI broken, API down
    AuthExpired          // needs human re-auth -> Needs You
    Timeout              // wall-clock or turn cap exceeded
    Unknown
)
```

| Class | Retry same model? | Cooldown model? | Escalate to human? |
|---|---|---|---|
| `TaskFailure` | yes, up to budget | no | after budget |
| `QuotaExhausted` | no | yes, long (default 1h, or provider-reported reset) | only if no fallback remains |
| `RateLimited` | no | yes, short (default 5m) | only if no fallback remains |
| `ProviderUnavailable` | no | yes, medium | only if no fallback remains |
| `AuthExpired` | no | yes, until resolved | immediately (`provider_auth`) |
| `Timeout` | yes, once | no | after budget |
| `Unknown` | **treated as `TaskFailure`** | no | after budget |

**`Unknown` maps to `TaskFailure` by design.** Classification is string and exit-code matching
against CLI output that changes on the vendor's schedule. A false `QuotaExhausted` silently
escalates work up the fallback chain toward the most expensive model — the exact outcome routing
exists to prevent. Failing toward "the agent had a bad run" is cheap and visible; failing toward
"the provider is down" is expensive and silent. Every classification is recorded with the matched
evidence so misclassification is diagnosable.

### 4.4 Router

```go
package router

type Route string

const (
    RouteLocal          Route = "local"
    RouteCheap          Route = "cheap"
    RouteStandard       Route = "standard"
    RouteStrong         Route = "strong"
    RoutePlanning       Route = "planning"
    RouteImplementation Route = "implementation"
    RouteReview         Route = "review"
)

type Router interface {
    Resolve(ctx context.Context, r Route, c Constraints) (Choice, error)
    MarkUnavailable(providerID, model string, class provider.FailureClass, until time.Time)
    Availability(ctx context.Context) ([]AvailabilityRow, error)
}

type Constraints struct {
    HostID  string
    Routes  map[Route][]Choice  // the project's own bucket table, if it has one
    Exclude []Choice            // choices already tried and failed in this run
}

type Choice struct {
    ProviderID string
    Model      string
    Why        []string   // "route implementation choice 2 of 4",
                          // "choice 1 claude-code/sonnet cooling down until 14:32 (QuotaExhausted)"
}
```

`RouteLocal` resolves to nothing in v0.1 and falls through to the next configured choice. When a
route exhausts every choice, the ticket goes to Needs You with the full `Why` trace rather than
failing silently.

A project's `Routes` **replace** the global table for any bucket they name — the project's list is
the whole preference order, not a prefix of the global one, because a project that pins `review` to
a local model does not want the fleet's cloud model waiting behind it. Buckets the project does not
name fall through to the global table. Every caller passes the project's table: the scheduler when
it places a ticket, planning on each turn, and the advisory review pass, which
resolves the `review` bucket per review rather than once at startup for that reason.

### 4.5 Scheduler

```go
package scheduler

type Scheduler interface {
    Tick(ctx context.Context) ([]Assignment, error)
    Explain(ctx context.Context, ticketID string) (Explanation, error)
}

type Assignment struct {
    TicketID   string
    HostID     string
    ProviderID string
    Model      string
    Why        []string   // surfaced verbatim in the TUI
}
```

The algorithm is deterministic and, deliberately, dull:

1. Load Ready tickets ordered by `(priority DESC, position ASC, created_at ASC)`.
2. **Skip tickets whose project is not available.** Availability depends on project mode:
   - **serial (default)** — available only when the project has **zero tickets in flight**, where
     in-flight means any state after `Ready` and before `Done`/`Rejected`. The next ticket starts
     only once the previous one has **merged**.
   - **parallel (opt-in)** — available while fewer than `max_concurrency` runs are *running*.
     Tickets awaiting review do not hold the project. Conflicts become possible and are the
     human's to handle; see `PRODUCT.md` §10.
3. For each remaining ticket, filter hosts by project requirements (OS, tools, e.g. macOS+Xcode).
4. Filter by ticket-specific requirements (adds to, or overrides, project requirements).
5. Filter by required tooling and provider availability on that host.
6. Prefer an eligible **idle** host; otherwise the **least busy** eligible host.
7. Honour an explicit human host override unconditionally.
8. Resolve the ticket's route through the Router for the chosen host.
9. If no host or no route resolves, leave the ticket Ready and record why.

**Concurrency shape.** The global worker pool is shared across projects; project availability is
what limits any single repository. N projects in serial mode run N agents concurrently, one per
repository — the intended default shape. Cross-project parallelism is free because separate
repositories cannot collide; within-repository parallelism is opt-in because that is where
collisions come from.

**Serial mode makes review latency the throughput gate**, by design (`PRODUCT.md` §10). The
scheduler must therefore make that visible rather than merely idle: `Explain` reports a held
ticket as "project serialized; GR-014 awaiting your review" — naming the blocking ticket and its
state — and the dashboard surfaces the same string. An idle queue is always explained.

Every step appends to `Why`. `Explain` answers "why is this ticket not running?" and "why this
host and model?" from recorded state. There is no AI in scheduling, in v0.1 or later.

### 4.6 Git

```go
package git

type Repo interface {
    Fetch(ctx context.Context) error
    CreateWorktree(ctx context.Context, branch, base string) (Worktree, error)
    RemoveWorktree(ctx context.Context, w Worktree) error
    Rebase(ctx context.Context, w Worktree, onto string) (RebaseResult, error)
    CommitAll(ctx context.Context, w Worktree, msg string) (string, error)
    Diff(ctx context.Context, w Worktree, base string) (Diff, error)
    Land(ctx context.Context, w Worktree, mode LandMode) (LandResult, error)
}

type LandMode string
const (
    LandMerge LandMode = "merge" // squash into target, push
    LandPR    LandMode = "pr"    // push branch, `gh pr create`
)

type RebaseResult struct {
    Clean         bool
    ConflictFiles []string
}

type Diff struct {
    Files []FileDiff  // path, status, additions, deletions, patch
}
```

Branch naming: `gravy/<ticket-id>-<slug>`. Worktrees live at
`~/.gravy/projects/<slug>/worktrees/<ticket-id>/`. Gravy shells out to `git` through `Host.Exec`
— it does not embed a Git implementation.

### 4.7 Validation

```go
package validate

type Step struct {
    Name     string        // build, test, lint, typecheck
    Cmd      string
    Timeout  time.Duration
    Required bool          // a failed optional step warns; it does not fail the run
}

type Result struct {
    Step     string
    ExitCode int
    Output   string        // tail-capped; full output in runs/<id>/validation/<step>.log
    Duration time.Duration
}

type Runner interface {
    Run(ctx context.Context, h host.Host, wt string, steps []Step) ([]Result, error)
}
```

Steps run in configured order inside the worktree and stop at the first failed **required** step.

### 4.8 Permission

```go
package permission

type Allowlist struct {
    ReadPaths   []string  // globs, relative to worktree
    WritePaths  []string
    Commands    []Pattern // allowed shell commands
    Network     bool
}

type Pattern struct {
    Match string  // glob or ^regex
    Note  string  // shown in Needs You when this rule is what allowed an action
}

type Broker interface {
    Check(a Action) Decision                      // Allow | Escalate
    Grant(projectID string, a Action, scope Scope) error  // ScopeOnce | ScopeProject
}
```

Enforcement is provider-side: each adapter installs a pre-tool hook that consults the broker.
On `Escalate` the hook denies the tool call and writes the run's `AskPath` (outside the
worktree, §6.2) with `type: "permission"`,
which stops the agent and triggers the standard escalation path (§6). `ScopeProject` grants are
appended to the project allowlist so the same prompt never recurs.

Defaults seeded at onboarding: read anywhere in the worktree; write anywhere in the worktree;
the project's declared validation commands; common read-only shell (`ls`, `cat`, `grep`, `find`,
`git status/diff/log`); network off. Everything else escalates.

---

## 5. Data model

SQLite with WAL, `foreign_keys=ON`, `busy_timeout=5000`. Single writer (`gravyd`). Migrations are
numbered, forward-only, and embedded in the binary.

```sql
CREATE TABLE projects (
  id              TEXT PRIMARY KEY,
  slug            TEXT NOT NULL UNIQUE,
  name            TEXT NOT NULL,
  repo_path       TEXT NOT NULL,
  target_branch   TEXT NOT NULL DEFAULT 'main',
  merge_mode      TEXT NOT NULL DEFAULT 'merge',   -- merge | pr
  requirements    TEXT NOT NULL DEFAULT '{}',      -- JSON: os, tools
  validation      TEXT NOT NULL DEFAULT '[]',      -- JSON: []Step
  allowlist       TEXT NOT NULL DEFAULT '{}',      -- JSON: Allowlist
  routes          TEXT NOT NULL DEFAULT '{}',      -- JSON: per-project route overrides
  parallel_mode   INTEGER NOT NULL DEFAULT 0,   -- 0 = serial: one ticket in flight, through merge
  max_concurrency INTEGER NOT NULL DEFAULT 1,   -- only consulted when parallel_mode = 1
  created_at      INTEGER NOT NULL
);

CREATE TABLE tickets (
  id            TEXT PRIMARY KEY,
  project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  title         TEXT NOT NULL,
  body          TEXT NOT NULL,
  state         TEXT NOT NULL,                     -- see §6
  priority      INTEGER NOT NULL DEFAULT 0,
  position      REAL NOT NULL,                     -- fractional ordering; cheap reorder
  route         TEXT NOT NULL DEFAULT 'implementation',
  requirements  TEXT NOT NULL DEFAULT '{}',
  host_override TEXT,
  worktree_path TEXT,
  branch        TEXT,
  retry_count   INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE INDEX idx_tickets_state ON tickets(state, priority DESC, position);

CREATE TABLE ticket_deps (
  ticket_id  TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
  depends_on TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
  PRIMARY KEY (ticket_id, depends_on)
);

CREATE TABLE runs (
  id            TEXT PRIMARY KEY,
  ticket_id     TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
  host_id       TEXT NOT NULL,
  provider_id   TEXT NOT NULL,
  model         TEXT NOT NULL,
  session_ref   TEXT,
  state         TEXT NOT NULL,
  failure_class TEXT,
  failure_note  TEXT,                              -- matched evidence for the classification
  pid           INTEGER,                           -- for startup reconciliation
  turns         INTEGER,
  tokens_in     INTEGER,
  tokens_out    INTEGER,
  cost_usd      REAL,
  started_at    INTEGER NOT NULL,
  ended_at      INTEGER
);

CREATE TABLE validations (
  id        TEXT PRIMARY KEY,
  run_id    TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  step      TEXT NOT NULL,
  exit_code INTEGER NOT NULL,
  duration_ms INTEGER NOT NULL,
  log_path  TEXT NOT NULL
);

CREATE TABLE summaries (
  ticket_id  TEXT PRIMARY KEY REFERENCES tickets(id) ON DELETE CASCADE,
  run_id     TEXT NOT NULL REFERENCES runs(id),
  branch     TEXT NOT NULL,
  commits    TEXT NOT NULL,   -- JSON
  files      TEXT NOT NULL,   -- JSON: path, +, -
  narrative  TEXT NOT NULL,   -- markdown, generated from the diff
  assumptions TEXT NOT NULL DEFAULT '[]',
  created_at INTEGER NOT NULL
);

CREATE TABLE attention (                            -- the Needs You queue
  id         TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  ticket_id  TEXT REFERENCES tickets(id) ON DELETE CASCADE,
  run_id     TEXT REFERENCES runs(id) ON DELETE CASCADE,
  reason     TEXT NOT NULL,                         -- see PRODUCT.md §8
  payload    TEXT NOT NULL DEFAULT '{}',            -- JSON: question, options, diff stats...
  resolved   INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_attention_open ON attention(resolved, created_at);

CREATE TABLE provider_availability (
  provider_id TEXT NOT NULL,
  model       TEXT NOT NULL,
  class       TEXT NOT NULL,
  until       INTEGER NOT NULL,
  note        TEXT,
  PRIMARY KEY (provider_id, model)
);
```

`position` is a REAL so reordering a ticket is a single row update (insert between neighbours by
averaging) rather than renumbering the queue.

---

## 6. Ticket state machine

Defined in `internal/core` with an explicit transition table and exhaustive table-driven tests.
Illegal transitions return an error; they are never silently ignored.

```
Draft ──► Backlog ──► Ready ──► Assigned ──► Running ──► Validating
                                                │            ├─ pass ──────────────► Reviewing
                                                │            ├─ fail (budget left) ─► Running
                                                │            └─ fail (exhausted) ───► NeedsYou
                                                └─ ask.json ─────────────────────────► Blocked

Reviewing ──► Review
   ├─ approve ────────► Landing ─ rebase + re-validate ─┬─ clean ──────────► Done
   │                                                    └─ conflict / red ─► NeedsYou
   ├─ request changes ─► Ready     (feedback appended to ticket, worktree reused)
   └─ reject ──────────► Rejected  (worktree removed)

Ready ── return_to_backlog ──► Backlog (withdraw from scheduling; preserve work)

Blocked ── human answers ──► Ready  (resumes prior session via Provider.Resume)
NeedsYou ── human acts ────► Ready | Review | Rejected
```

### 6.1 Run lifecycle (`internal/agentrun`)

```
1.  claim worker slot (respecting the project concurrency cap)
2.  git fetch the remote, THEN create the worktree from the freshly-fetched target branch.
    Per ticket, at claim time — never a batch fetch. This is what makes a queued ticket start
    on top of whatever landed before it.
3.  build context (contextbuild) -> prompt
4.  resolve route (router) -> provider + model
5.  launch agent (provider.Run) with allowlist + timeout + turn cap
6.  stream events -> runs/<id>/events.jsonl, live to clients
7.  on exit:
      ask.json present?          -> escalate (§6.2)
      Classify() != Success?     -> quota/auth: cooldown, re-resolve route, retry
                                    task failure: retry within budget, else NeedsYou
8.  run validation steps
9.  red and budget remains?      -> retry with failure output appended to context.
                                    On the final attempt, optionally bump to the `strong` route.
    red and budget exhausted?    -> Needs You (validation_failed)
10. green                        -> generate summary (§7), run automated review
11. -> Review. Release worker slot.
```

### 6.2 Escalation — one path, three causes

Ambiguity, permission, and hard failure all travel the same road, which is why permission
handling costs one component rather than a subsystem:

- **Ambiguity** — the prompt instructs the agent that on a genuine ambiguity it must write its
  **`AskPath`** as `{"type":"question","question":...,"options":[...],"context":...}` and stop.
- **Permission** — the provider pre-tool hook consults the broker, denies the call, and writes
  `AskPath` as `{"type":"permission","action":...,"command":...,"rationale":...}`.

**`AskPath` is an absolute path outside the worktree** — `~/.gravy/runs/<run-id>/ask.json` — passed
to the agent in its task. It is deliberately *not* `<worktree>/.gravy/ask.json`: the orchestrator
commits WIP before escalating, so an in-worktree file would be committed into the branch and land
in the merge, and it would dirty `git status` in exactly the worktree a human may be resolving a
conflict in. **Gravy writes nothing into a repository or a worktree, ever** (§3), and the
escalation channel is not an exception.

In both cases the orchestrator:

1. commits WIP to the branch (`gravy: wip before <reason>`)
2. **releases the worker slot** — a blocked ticket never holds a worker
3. retains the worktree and records `session_ref`
4. sets the ticket `Blocked`, opens an `attention` row, fires a notification

On answer the ticket returns to Ready and, when scheduled, resumes via `Provider.Resume` with the
answer injected — not a fresh run, so context is preserved. Permission answers additionally carry
`ScopeOnce` or `ScopeProject`.

### 6.3 Landing

Approval starts landing; it does not guarantee it:

```
fetch → rebase worktree onto target
  ├─ conflict ──────────────────► NeedsYou (merge_conflict), worktree PRESERVED
  └─ clean
       └─ rebase succeeds (including a no-op)            → re-run validation
             └─ red ────────────► NeedsYou (validation_failed)
       → merge_mode
            ├─ merge: squash-merge into target, push
            │    └─ main checkout dirty ──► NeedsYou (checkout_dirty), nothing touched
            └─ pr:    push branch, `gh pr create`
          → ticket Done, worktree removed, dependents re-evaluated, worker released
```

Three deliberate rules:

**Validation runs on every landing attempt.** A no-op rebase can follow failed validation or
a human conflict resolution; it does not establish that the current tree is green.

**A refusal is not a conflict.** The squash happens in the main checkout, because a worktree
already has the ticket's branch checked out. If that checkout has uncommitted changes, git would
sweep them into the squash commit, so landing refuses before moving anything and raises
`checkout_dirty` — carrying the checkout path and the files, because the work to do is in a
directory the ticket never mentions. Calling it `merge_conflict` would send the human to a
preserved worktree that has no conflicting files and nothing to resolve. The same rule governs
the rebase step above: git declining to start is not the same event as commits that would not
replay.

**Conflicts are handed to the human, not solved.** On conflict Gravy aborts the rebase, preserves
the worktree, records the conflicting paths, and raises `merge_conflict`. The human resolves it in
their own tools and tells Gravy to continue. There is no automatic resolution, no retry loop, and
no conflict-resolving agent. An integration agent is a plausible later addition **if real usage
shows conflicts are frequent enough to justify it** — the conservative default exists precisely so
that evidence probably never arrives.

---

## 7. Context and summaries

**`contextbuild`** assembles the agent prompt under a hard token budget (default 30k), in
descending priority so truncation degrades gracefully:

1. the ticket (title, body, acceptance criteria)
2. result summaries of dependency tickets — **never transcripts**
3. project conventions (`CLAUDE.md`, `AGENTS.md`, `.github/copilot-instructions.md`, contributing docs)
4. capped excerpts of project docs (`ARCHITECTURE.md`, `PRODUCT.md`, `ROADMAP.md`, design docs)
5. validation commands the agent is expected to satisfy
6. on a retry: the previous attempt's failure output

**`summary`** produces the durable record in two halves:

- **Mechanical** (facts, not opinion): branch, commits, files with line counts, validation
  results and exit codes, provider/model, retries, duration.
- **Narrative** (cheap route, generated **from `git diff`**): what changed, decisions taken,
  assumptions made, interfaces added, notes for dependent tickets.

The narrative is generated from the diff rather than authored by the implementing agent. A
self-report from the party with a motive to declare success is not evidence, and dependent
tickets consume these summaries as fact. Extracted `assumptions` surface as an amber flag on the
Review screen.

---

## 8. API and clients

`internal/api` defines one transport-agnostic service interface implemented by the daemon and
consumed by the Go client. JSON-RPC 2.0 over a unix socket, plus a server-push event stream so
the TUI does not poll.

```go
type Service interface {
    // projects
    ListProjects(ctx, ProjectFilter) ([]core.Project, error) // working set unless asked
    AddProject(ctx, AddProjectReq) (core.Project, error)
    UpdateProject(ctx, core.Project) error
    ArchiveProject(ctx, id string, archived bool) error      // its own method, never a field

    // tickets
    ListTickets(ctx, TicketFilter) ([]core.Ticket, error)
    CreateTicket(ctx, CreateTicketReq) (core.Ticket, error)
    UpdateTicket(ctx, core.Ticket) error
    MoveTicket(ctx, id string, state core.State) error
    ReorderTicket(ctx, id string, before, after string) error

    // runs
    GetRun(ctx, id string) (core.Run, error)
    StreamLogs(ctx, runID string) (<-chan LogLine, error)
    KillRun(ctx, runID string) error

    // review + attention
    GetReview(ctx, ticketID string) (ReviewBundle, error)   // diff, validation, summary, verdict
    Approve(ctx, ticketID string) error
    RequestChanges(ctx, ticketID, feedback string) error
    Reject(ctx, ticketID string) error
    ListAttention(ctx) ([]core.Attention, error)
    ResolveAttention(ctx, id string, answer Answer) error

    // system
    Status(ctx, ProjectFilter) (SystemStatus, error)        // hosts, workers, providers, routes
    ReconnectHost(ctx, id string) (HostStatus, error)       // probe a machine that was off
    ExplainTicket(ctx, ticketID string) (Explanation, error)
    Events(ctx) (<-chan Event, error)                        // server push
}
```

The TUI holds no domain logic — it renders `Service` results and sends `Service` calls. Anything
it needs that the API cannot answer is a missing API method, not a reason to reach into core.

---

## 9. TUI

Bubble Tea, with a screen per state and a persistent frame (status bar, project indicator, help
overlay on `?`). Global keys: `1-7` jump to sections, `,` Settings, `p` project switcher,
`P` add a project, `q` quit, `/` filter.

The number row is the ticket lifecycle in order — **Dashboard, Plan, Backlog, Ready, Running,
Review, Needs You** — so the numbers mean something. Settings is configuration rather than a
stage of that lifecycle and sits off the row on `,`, the conventional preferences key. Not `s`:
that is already save on the Settings screen and a binding on Review, and the frame checks global
keys before a screen's own, so a global `s` would break saving in the screen it opens.

`P` is global rather than a Settings key because the first-run dashboard is empty: there is no
project to navigate to yet, and a keyboard-first tool should never have to send someone out to a
shell to get started. It is shift-`P` so that it shadows neither Review's `a` nor Backlog's `n` —
the frame checks global bindings before a screen's own keys.

Screens: **Dashboard** (Needs You, Running, Ready — in that order), **Plan**, **Backlog**,
**Ready**, **Running**, **Review**, **Needs You**, **Done**, **Settings**.

The **Plan** screen is the one view behind `PRODUCT.md` §6.2 and §6.3: a conversation that reads
the project's own documents, proposes work, is grilled until it is right, and produces tickets
the human approves into the backlog. It never creates a ticket on its own.

The **Review** screen is progressive disclosure, not a diff engine: a compact card by default
(summary, validation, verdict, changed-file stats, amber assumption flags), `enter` to expand a
file's diff inline, and `e` / `d` / `!` to leave for `$EDITOR`, `git difftool`, or a shell in the
worktree. Review is the bottleneck the product creates, so it optimises for fast triage with a
one-key escape to real tools rather than trying to replace them.

---

## 10. Extension paths

**Remote hosts.** Implement `Host` over SSH or a small `gravy-worker` HTTP service. The scheduler
already filters on `Caps` and already handles multiple hosts; Rule 1 means nothing else changes.
Each machine gets its own clone and worktrees — no shared network filesystem.

**New providers.** Implement `Provider`, register it, add a `Classify` matcher table. Nothing
else in the application changes. This is the test for whether the abstraction is real.

**Local models.** v0.1 has no local adapter because Ollama serves models but does not implement
an agent loop — file editing, tool use, iteration. v0.2 either wraps an OSS terminal agent or
ships a minimal built-in loop. Either arrives as a `Provider`, and `RouteLocal` starts resolving.

**Additional clients.** Implement against `api.Service`. Rule 2 keeps this available.

**Within-repository parallelism.** Raising `max_concurrency` already works. Making it *safe* to
raise automatically needs affected-component hints on tickets — a declared or inferred set of
paths/components a ticket will touch — so the scheduler can run only non-overlapping tickets
together. The scheduler's filter chain is the natural place for that predicate. Deferred until the
conservative default proves limiting.

**Stacked tickets.** The cost of serial mode is that a queue produces one finished ticket while
the human sleeps. The fix is not weakening the merge gate: it is letting ticket 2 branch from
ticket 1's *branch* rather than from target, forming a stack. The queue keeps moving; approving
ticket 1 makes ticket 2 a trivial rebase; rejecting it discards ticket 2's work. That trade is
sound — agent time is cheap, human time is not — and it preserves every invariant.

`Repo.CreateWorktree(ctx, branch, base)` already takes `base` as a parameter for exactly this
reason. **It must never be hardcoded to the target branch**; the branch base is a scheduler
decision, and today it simply always chooses target.

**Merge helper.** Shipped in v0.1 (GR-036), deliberately minimal: an agent run scoped to resolving
conflicts in a preserved worktree, invoked only on explicit human request from Needs You, whose
output re-enters validation and human review like any other change. It is a `Provider` run with a
narrow prompt — not a new subsystem, and not a privileged path around the merge gate.

---

## 11. Safety posture

Stated plainly rather than dressed up: **Gravy does not sandbox agents.** They run as the user's
own process with the user's own privileges. The real boundaries are the **worktree** (blast
radius), the **allowlist** (what the agent may do without asking), and the **merge gate** (nothing
reaches the target branch unapproved).

Every run carries a wall-clock timeout, a turn cap, and a kill switch. Every action outside the
allowlist escalates rather than being auto-granted or silently blocking on an invisible prompt.
Gravy will not claim more isolation than it provides.


### GR-032 setup API

`api.Service` now exposes `SetupInfo(ctx)`, `PreviewSetup(ctx, AddProjectReq)`, and
`ApplySetup(ctx, SetupRequest)`. SetupInfo returns settings, provider status/model suggestions,
local host capabilities, and existing projects. PreviewSetup shares AddProject's repository
validation and the toolchain table in `internal/api/seed.go`; it proposes validation, host
requirements and permissions without persistence. The TUI reuses settings/project field editors.

The wizard keeps independent drafts of maps and slices. First-run startup uses `config.Read`,
which leaves missing config in memory; only explicit final approval writes config or a project.
ApplySetup rejects a stale configuration snapshot, validates before writing, and removes the
new project if saving configuration fails. Canceling any earlier step writes neither. Existing
projects are untouched when rerunning setup; a blank repository field saves global settings only.
The SQLite runtime database and daemon socket can exist before approval, but no setup project
or configuration file is created. Configuration writes remain atomic via `config.Save`.

`AddProjectReq` adds optional explicit Allowlist, Requirements and Routes fields so the approved
suggestions survive registration exactly, including an explicitly empty allowlist. Existing
callers retain their current default behavior. No host/provider interfaces or dependencies change.

### Review testing and Copilot integration

`core.Project.PreviewCommand` is the user-supplied command to launch the app for hands-on
review. Store migration 0007 persists it; the existing project API carries it. Review's
`Shift+T` panel saves that command explicitly and hands the local terminal to it in the
original ticket worktree. The panel can also rerun configured validation steps, preserving
output until Enter. These manual results do not update the ticket's state or replace stored
validation evidence. Local terminal execution remains in `internal/host`; remote worktrees
are refused with their host named. No Service or Host method was added.

The `copilot` provider drives standalone GitHub Copilot CLI with JSONL output. Detection uses
its SDK `auth.getStatus` RPC without a model call. `copilot/default` omits an explicit model;
model enumeration is advisory. Opaque session references include the CLI session ID, worktree
and permission settings so resume does not silently drop the original command restrictions.
See `COPILOT.md` for contract verification and limitations.

`host.ExecSpec.UnsetEnv` removes named variables from the command environment, including
inherited variables and values in `Env`; removal takes precedence. Local execution,
detached execution, and SSH execution honor it. Copilot uses it to remove
`COPILOT_ALLOW_ALL`, whose presence enables unrestricted tool approval even when its value
is `false`.

Non-interactive review steps use piped output, no terminal input, `CI=true`, and `TERM=dumb`
to prevent pagers and prompts from suspending background process groups. Interrupting a
review command returns `host.ErrInterrupted`, which the review screen treats as a normal stop.

### Multiple preview services

`core.Project.PreviewServices []core.PreviewService` stores named services with `Name`, `Dir`,
`Command`, and `EnvFile`. Migration 0008 adds a JSON column, defaulting to an empty list for
existing projects. The existing project API validates and persists the list. `PreviewCommand`
remains supported when the list is empty; services take precedence when present.

Projects settings edit service names and individual directory/command/environment-file fields.
`host.TerminalRun.Services` launches services concurrently in the ticket worktree with piped,
service-labeled output and no shared terminal input. Directories must stay in the worktree,
including after symlink resolution. Environment files may be relative to a service directory
or absolute; explicit shell sourcing exports variables without logging their contents.
Every directory and environment file is checked before launching any service.

An interrupt or any service exit terminates every started process group, allowing two seconds
for shutdown before forced cleanup. Manual preview never changes approval or validation state.
Service readiness, port allocation, remote preview, and interactive per-service terminals are
not implemented. No Host or Service method was added.

### Automatic quota fallback

After a confirmed `QuotaExhausted` or `RateLimited` outcome, the orchestrator persists the
provider/model cooldown, then applies `EventProviderRetry` (`Running → Ready`). The scheduler
resolves the existing route again, including project overrides. It skips cooled choices,
or leaves the ticket Ready if every choice is unavailable. The worktree, feedback, and
self-correction budget are retained; no attention item is raised for this transition.
Authentication, unknown interruption, and ordinary validation failures retain their existing
attention/retry behavior. An unknown failure is never inferred to be a quota condition.

Planning requests carry optional `History` and `Agent` fields through API `PlanReq` and core `PlanTurn`. These preserve conversation context when routing changes models. The planner checks terminal outcomes before parsing proposals, records quota/rate cooldowns, and resolves the next choice within the same request. Provider session IDs are retained only for the same selected agent. Each choice is attempted at most once per turn.

Startup reconciliation also scans `reviewing` tickets: implementation runs already have an end time during advisory review and are absent from the unfinished-run query. `daemon.ReconcileStore` therefore also requires `ListTicketsByState`, `ListRunsForTicket`, and `ListOpenAttention`. Recovery preserves an existing verdict, marks a missing verdict unavailable, ensures review-pending attention exists, then applies `EventReviewed`. It does not approve or land work. Writing attention before the transition makes interrupted recovery retryable without duplicate attention.
