# Gravy — Product Specification (v0.1)

## 1. What Gravy is

Gravy is a keyboard-first CLI/TUI engineering manager for agentic software development.

It sits above coding agents — Claude Code, Codex, and others later — and runs them as a managed
fleet against a ticket queue, in isolated Git worktrees, with automated validation and a hard
human approval gate before anything merges.

The dividing line the entire product rests on:

> **The human manages ideas, tickets, priorities, and approvals.**
> **Gravy manages agents, models, machines, worktrees, retries, validation, and execution.**

### The one-sentence goal

**Reduce the human attention required to run multiple coding agents, without reducing human
control over what gets built and merged.**

### What that means in practice

The primary value of Gravy is **organization, not maximum parallelism.**

The pain being solved is jumping between terminal windows trying to remember which agent is
working on which repository and branch, what is waiting, what finished, and what needs review.
Gravy replaces that with one queue, one dashboard, and one place where things need you.

Throughput is a secondary benefit. Agents finish tickets fast enough that even **one ticket at a
time per repository, carried all the way through merge**, is a substantial improvement over the
status quo. Parallelism is an optimization to be turned on deliberately, never a requirement for
the product to be worth using.

Every design decision in this document and in `ARCHITECTURE.md` is evaluated against that
sentence. Features that save attention at the cost of control are rejected. Features that
preserve control at the cost of enormous attention are also rejected — that is the status quo.

## 2. The problem

Running several coding agents today means running several terminals. The human becomes a manual
scheduler:

- copying repositories or hand-managing worktrees so agents do not collide
- remembering which agent is working on what
- noticing when an agent finished, failed, or silently stalled waiting on a prompt
- re-running builds and tests by hand to find out whether the work is any good
- rebasing and merging by hand
- deciding, every time an agent frees up, what it should do next
- discovering that a provider hit a quota only after wasting twenty minutes

None of that is engineering judgment. All of it is dispatch work, and dispatch work is what
computers are for. Meanwhile the parts that *are* judgment — what to build, in what order, and
whether the result is acceptable — get squeezed.

Gravy automates the dispatch and protects the judgment.

## 3. Non-negotiable principles

1. **Human approval is a hard boundary.** Gravy never merges unapproved agent work. Not for
   trivial diffs, not for green tests, not for any configuration setting. This is not a default;
   it is an invariant.
2. **The human is never surprised.** Every automated decision — host choice, model choice,
   fallback, retry, failure classification — is recorded and can be explained in the interface in
   plain language.
3. **Everything that needs a human goes in one queue.** One place to look, always. If Gravy
   needs you, it is in Needs You. If it is not in Needs You, Gravy does not need you.
4. **An agent never waits silently.** Blocked on ambiguity, blocked on permission, blocked on a
   conflict — it escalates and releases its worker slot. Invisible stalls are the enemy.
5. **Spend expensive intelligence on judgment, not routine typing.** Model routing exists to put
   the strong models where they change outcomes.
6. **Boring, robust components.** Git stays Git. State is SQLite. Execution is subprocesses.
   Gravy is not a distributed system and should not acquire the problems of one.
7. **The repository is not Gravy's scratchpad.** Live state lives in `~/.gravy/`. Repositories
   hold code and durable documentation, and Gravy does not write bookkeeping into them.

## 4. Core concepts

| Concept | Definition |
|---|---|
| **Project** | A Git repository plus its configuration: target branch, merge mode, validation steps, host requirements, permission allowlist, default routes. |
| **Ticket** | An executable work order belonging to exactly one project. The atomic unit of everything. |
| **Run** | One attempt by one agent at one ticket. A ticket may have several runs (retries, resumes, requested changes). |
| **Worker** | A concurrency slot on a host. A worker executes one run at a time. |
| **Host** | A machine that can execute runs. v0.1 has exactly one: the local machine. |
| **Provider** | An adapter for a coding agent CLI (`claude-code`, `codex`). |
| **Route** | A semantic intent — `cheap`, `standard`, `strong`, `review`, `planning`, `implementation`, `local` — resolved to an ordered list of provider/model choices. |
| **Summary** | The compact durable record a finished run leaves behind, consumed by dependent tickets instead of a transcript. |
| **Needs You** | The single queue of everything awaiting human judgment. |

## 5. The loop

The whole workflow, as the human experiences it:

```
Write / plan tickets  →  Ready  →  Gravy executes  →  Needs You  →  decide  →  continue
```

That is the entire mental model. Everything below is Gravy's half of it, and the human should
never need to hold it in their head.

```
Idea
  → Ticket (Quick, or Assisted/Planned in v0.2)
  → Backlog
  → Ready              (dependencies landed; nothing blocking)
  → Running            (worker assigned, worktree created, agent implementing)
  → Validation         (build, tests, lint — bounded self-correction on failure)
  → Automated review   (structured verdict from a review-route model)
  → Human Review       ◄── the hard gate
  → Approve
  → Land               (rebase onto target, re-validate, merge or PR, push)
  → Done               (worktree cleaned, dependents unblocked, worker returns to pool)
```

Idle workers automatically consume eligible Ready tickets. That is the engine. Everything else
in this document exists to make that engine trustworthy enough to leave running.

## 6. Ticket creation

### 6.1 Quick (v0.1)

The human writes the ticket. It enters Backlog **immediately** — creation is never blocked.

An AI reviewer then critiques it in the background on the `review` route and returns *proposed
changes*, never a silent rewrite. The critique looks for:

- ambiguous requirements
- missing acceptance criteria
- missing or unspecified validation
- likely dependencies on other tickets
- scope problems (too large, or two tickets wearing one hat)
- missing project context the agent will need

Suggestions arrive as a Needs You item. Each is individually **accepted, edited, or dismissed**.
The human's original text is never replaced without an explicit accept.

> **Design note.** The critique is asynchronous on purpose. Blocking ticket creation for thirty
> seconds at the moment of peak impatience is how a good feature gets switched off in week one.

### 6.2 Assisted (v0.2)

A rough idea in, a drafted ticket out, edited and approved by the human, then optionally
critiqued as above.

### 6.3 Planned (v0.2)

For larger work: an idea becomes a planning and grilling conversation, which produces a proposed
implementation plan, which produces several linked tickets with dependencies, which the human
approves into the backlog. Planning must be possible *while workers continue implementing other
tickets* — planning is a foreground human activity, not a stop-the-world event.

## 7. Ticket lifecycle and states

```
Draft → Backlog → Ready → Assigned → Running → Validating
                                        │           ├─ pass ──────────► Reviewing
                                        │           ├─ fail, retries left ──► Running
                                        │           └─ fail, exhausted ────► Needs You
                                        └─ asks / needs permission ───────► Blocked
Reviewing (automated) → Review (human)
    ├─ approve ─────► Landing ─ rebase + re-validate ─┬─ clean ─────► Done
    │                                                 └─ conflict/red ──► Needs You
    ├─ request changes ──► Ready   (feedback attached, worktree reused)
    └─ reject ───────────► Rejected (worktree cleaned)
Blocked ── human answers ──► Ready (prior agent session resumed)
```

**Ready gating.** A ticket becomes Ready only when every dependency is **Done — landed, not
merely approved**. Dependent work must branch off a target that actually contains what it depends
on. This does mean dependency chains serialize through human review; that is the honest cost of
a real approval gate, and it is why ticket dependencies should be used sparingly.

## 8. Needs You

The primary queue and, in practice, the product. A single typed list of everything awaiting human
judgment, each entry carrying enough context to act without hunting:

| Reason | Meaning | Actions |
|---|---|---|
| `review_pending` | Run finished, validated, awaiting approval | inspect, diff, logs, approve, request changes, reject |
| `agent_question` | Agent hit a genuine ambiguity and stopped | answer and resume |
| `permission_request` | Agent needs an action outside the project allowlist | allow once, always allow for project, deny |
| `validation_failed` | Self-correction budget exhausted, still red | inspect logs, send back with guidance, reject |
| `merge_conflict` | Rebase onto target failed at land time | send back to agent to resolve, resolve in shell |
| `checkout_dirty` | The project's main checkout has uncommitted changes, which a squash would carry into the merge | commit or stash them, then continue the land |
| `provider_auth` | A provider CLI is unauthenticated or expired | re-auth instructions, disable provider |
| `ticket_critique` | Ticket review produced suggestions | accept / edit / dismiss per suggestion |
| `host_unavailable` | A required host or tool is missing | acknowledge, adjust requirements |

Every entry raises a notification (terminal bell plus OS notification, configurable). The
dashboard orders **Needs You**, then **Running**, then **Ready**.

## 9. Human review

Review is where the product either works or becomes a chore. Gravy makes agent output cheap to
produce; if review is expensive, the human simply becomes the new bottleneck. So the review
screen is designed for **fast triage with an escape hatch to depth** — progressive disclosure,
not a diff engine.

Default view is a compact card:

- project, ticket, branch
- worker, host, provider, model, retry count, duration
- validation results per step, with exit codes
- automated review verdict
- agent summary, with **assumptions flagged in amber** — an agent that guessed says so here
- changed files with per-file line counts

From there: expand any file to read its diff inline, or leave for real tools —

| Key | Action |
|---|---|
| `enter` | expand/collapse file diff inline |
| `e` | open the worktree in `$EDITOR` |
| `d` | open in `git difftool` |
| `!` | drop to a shell in the worktree |
| `l` | full run logs |
| `a` | approve |
| `r` | request changes (prompt for feedback, ticket returns to Ready) |
| `x` | reject (ticket closed, worktree cleaned) |

**Approve** triggers landing: fetch, rebase onto the current target branch, merge or open a PR per
project config, push, mark Done, clean the worktree, unblock dependents, and return the worker to
the pool. If the rebase actually replayed commits — meaning the target moved underneath this work
— validation re-runs on the rebased result first, because green against yesterday's target is not
evidence about today's. If the branch was already on top of target, which is the common case under
the conservative default, there is nothing to re-validate.

A conflict or a red re-validation goes to Needs You. Approval is not a promise that landing will
succeed, and Gravy never resolves a conflict on its own initiative.

## 10. Concurrency and conflicts

**Default: one ticket at a time per repository, carried all the way through merge.**
**Repositories run concurrently with each other.**

A repository is available for a new ticket when it has **nothing in flight** — no ticket between
Ready and Done. The next ticket does not start while the previous one is running, awaiting review,
or landing. It starts once that work is **merged**, and therefore starts from a target branch that
already contains it.

This is the setting that makes conflicts structurally impossible rather than merely unlikely.
Combined with fetching the remote immediately before each worktree is created, a queued ticket
always begins on top of everything that came before it. Nothing to rebase, nothing to conflict.

Cross-project parallelism is free and always on — separate repositories cannot collide, so the
global worker pool spreads across projects. Three active repositories means three agents working
at once, one per repository. That is the intended shape.

### The cost, stated plainly

Serializing on *merge* means **human review latency gates repository throughput**. A ticket
sitting in Review blocks its repository's queue. This is a real cost and it is accepted
deliberately: it trades throughput for the elimination of an entire class of problem, and
throughput was never the primary value.

Gravy makes the cost visible rather than hiding it. When tickets are waiting on a repository's
review, the dashboard says so — "3 Ready, blocked on your review of GR-014" — so an idle queue is
always explained, and the human can see exactly where their attention converts into progress.

It also raises the value of the automated review pass (§9): anything that helps the human approve
routine work confidently in seconds directly unblocks the queue. The review sweep — one keystroke
walking the whole pending queue in sequence — exists for the same reason.

The structural fix, later, is **stacked tickets**: letting the next ticket branch from the previous
ticket's branch instead of target, so the queue keeps moving while approval is pending. It
preserves every invariant and costs only discarded agent work when a ticket is rejected. Deferred
until the serial default proves limiting in practice.

### Parallel mode — opt in

A project can be switched to parallel mode, allowing several tickets in flight at once with a
configurable cap. **The tradeoff is explicit: in parallel mode you may have to handle merge
conflicts yourself.** Gravy will still detect them and hand them over cleanly; it will not
pretend they cannot happen. Turn this on for repositories where tickets reliably touch separate
areas, or when you would rather manage conflicts than wait on your own review.

Later, explicit dependencies and affected-component hints on tickets could let Gravy decide
automatically which tickets are safe to run together. Until that exists it is a human decision,
made deliberately, per project.

### Conflicts

Conflicts should be rare under the default and expected in parallel mode. When one occurs, Gravy
rebases, finds the conflict, aborts cleanly, **preserves the worktree**, and raises it in Needs
You with the conflicting files listed. From there the human has two options:

- **Resolve it directly** — open the preserved worktree in a shell or editor, fix it, tell Gravy
  to continue.
- **Ask the merge helper** — an agent run scoped to exactly one job: resolve these conflicts in
  this worktree. Its output is not special-cased; it goes back through validation and human review
  like any other work, because a conflict resolution is a code change and the merge gate does not
  have exceptions.

The merge helper is deliberately small. It resolves conflicts on request; it does not plan, refactor,
or make design decisions, and it is never invoked automatically.

**Known limitation, stated honestly:** rebasing catches *textual* conflicts. It does not catch
*semantic* drift — two agents independently inventing two slightly different helpers for the same
job. No test suite catches that either. Under the default this cannot arise, which is a further
argument for it. In parallel mode the human review gate is the only real mitigation.

## 11. Providers, routes, and limits

Gravy drives the user's **existing authenticated CLIs**. It does not want provider credentials
and does not store them. It detects which CLIs are installed and whether they appear
authenticated.

Tickets request a semantic route, not a model. Configuration maps routes to ordered choices:

```yaml
routes:
  implementation: [claude-code/sonnet, codex/gpt-5-codex, claude-code/haiku]
  review:         [claude-code/sonnet]
  cheap:          [claude-code/haiku]
  strong:         [claude-code/opus]
```

When a provider reports a **genuine** quota, rate-limit, auth, or availability failure, Gravy
marks that provider/model unavailable for a cooldown and moves to the next choice. Ordinary
coding failures are never treated as quota failures, and **an unrecognized error is classified as
a task failure, not a quota failure** — a mistaken quota classification silently escalates work up
the fallback chain to the most expensive model, which is exactly the outcome routing exists to
prevent.

## 12. Permissions and safety

Agents run unattended, so the permission question has to be answered explicitly rather than
waved through.

Each project carries an **allowlist**. Reading files, editing inside the worktree, running the
project's declared validation commands, and common read-only shell operations are pre-approved.
Anything outside the allowlist **escalates to Needs You as `permission_request`** — the agent's
tool call is denied, WIP is committed, the worker slot is released, and the human decides:

- **allow once** — resume this run with the action permitted
- **always allow for this project** — append to the project allowlist so it never asks again
- **deny** — resume with the action refused, or reject the ticket

The allowlist therefore trains itself against real work rather than being guessed during
onboarding. The tuning risk runs the other way: too tight and the human is interrupted
constantly, which defeats the purpose. Onboarding seeds sensible defaults per project type, and
"always allow" converges quickly.

**Stated plainly:** Gravy does not sandbox agents. They run as the user's own process with the
user's own privileges. The real boundaries are the worktree, the allowlist, and the merge gate.
Gravy will not claim more than that.

Every run additionally has a wall-clock timeout, a turn cap, and a kill switch in the TUI.

## 13. Hosts

v0.1 executes entirely on the local machine. The architecture treats execution as a `Host`
interface from day one so remote hosts can be added without rewriting the scheduler — but no
worker daemon, RPC protocol, or heartbeat machinery is built in v0.1.

Scheduling is deterministic and explainable, never AI-driven:

1. filter hosts by project requirements (e.g. an iOS project requires macOS with Xcode)
2. filter by ticket-specific requirements
3. filter by required tooling and provider availability
4. prefer an eligible idle host
5. otherwise prefer the least busy eligible host
6. always allow explicit human override

Gravy can always answer "why this host, why this model" with the recorded decision trace.

## 14. Context discipline

Agents receive only what their ticket needs, under a hard token budget:

- the ticket itself
- capped excerpts of project documentation (`ROADMAP.md`, `ARCHITECTURE.md`, `PRODUCT.md`,
  `CLAUDE.md`, design docs)
- **result summaries of dependency tickets** — never their transcripts
- the project's validation commands and conventions

Each finished run leaves a durable summary: mechanical facts (branch, commits, files changed,
validation results, provider/model, retries, duration) plus a narrative generated **from the
diff** — what changed, decisions taken, assumptions made, interfaces added, notes for dependent
tickets.

> **Design note.** The narrative is generated from the diff rather than written by the
> implementing agent. A self-report from the party with a motive to declare success is not
> evidence, and dependent tickets consume these summaries as fact.

## 15. Onboarding

First run is a guided setup, not an afterthought, and the same flow is reachable later from
Settings. It walks through: detected coding CLIs and their authentication status; discoverable
models; local host capabilities; worker concurrency; adding the first repository; detected
project type and tooling; suggested host constraints; detected validation commands; a seeded
permission allowlist; and basic route configuration with fallbacks.

Gravy **proposes**; the human **approves**. Adding an iOS project should suggest macOS + Xcode as
requirements and `xcodebuild test` as validation — and then wait to be told it is right.

## 16. v0.1 scope

**In:** the full loop on one machine. Multiple projects running concurrently, conservative within
each. Quick tickets with async critique. Ready queue and priority ordering. Local workers with
worktree isolation. `claude-code` and `codex` adapters.
Route resolution with fallback and cooldowns. Validation with bounded self-correction. Automated
review and result summaries. Blocked/resume for questions and permissions. Needs You with
notifications. Review screen with progressive disclosure. Rebase gate. Merge and PR land modes.
Onboarding. A small CLI surface alongside the TUI.

**Out:** remote hosts and `gravy-worker`. Local/Ollama providers. Assisted and Planned ticket
creation. Dependency graph visualisation. Budgets and cost optimisation. Web, desktop, or mobile
GUIs. RBAC, SSO, audit logs, organizational policy. Analytics. Side-by-side diffs and inline hunk
comments. Any AI-driven scheduling.

## 17. Open source and what comes later

Gravy is MIT-licensed and CLI/TUI-first. The open-source product is the real product: the full
loop, all providers, all routing, the complete TUI. It is not crippled to manufacture an
upgrade path.

The architecture separates Gravy Core from presentation, so future clients — desktop, web,
mobile — are additional API consumers rather than rewrites. A future commercial offering would
address team and organizational concerns that a single-user tool has no business carrying:
centralized coordination, RBAC and SSO, audit logs, org policy, shared agent fleets, analytics.

## 18. How we will know it worked

The honest test is not a feature checklist. After M0 ships, the author runs real work through
Gravy for two weeks, doing genuine development alongside it.

**The criterion is whether Gravy lets him stop juggling agent terminal windows.**

- **It worked** if he reaches for `gravy` instead of opening three terminals, and the dashboard
  answers "what is happening and what needs me" without him having to reconstruct it.
- **It did not** if he opens the terminals anyway — and no amount of v0.2 will fix that.

Note what is *not* in the criterion: throughput, agents run in parallel, tickets closed per day.
If Gravy runs one agent at a time and still ends the terminal juggling, it has succeeded.

Better to learn that in two weeks than in six months.
