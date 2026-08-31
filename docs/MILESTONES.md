# Gravy — Milestones

## M0 — Stop juggling terminals

**Goal.** Replace the multi-terminal workflow with one dashboard and one queue, for real work, on
one machine.

M0 is not a technical skeleton that happens to have a UI bolted on. The product's primary value is
**organization** (`PRODUCT.md` §1), so the dashboard and the Needs You queue are part of the
minimum, not polish deferred to later.

**The loop M0 must complete, unattended:**

```
create ticket → mark Ready → worker claims it → fetch → worktree from latest target →
claude-code implements → validation runs → automated review → Review →
human approves → merge → push → Done → next ticket in that repo starts
```

Across **multiple repositories at once**, one agent per repository, visible on one screen.

**In M0:** provider spike · SQLite store · daemon + unix socket API · LocalHost · git worktrees ·
`claude-code` adapter · validation with bounded retry · scheduler with serial-per-project
availability · automated review · merge + push · project registration · TUI shell, Dashboard,
Backlog/Ready, Running, Review, Needs You (four reasons), and review sweep mode.

**Not in M0:** codex · routing and fallback · result summaries · blocked/resume · permission
broker · merge helper · PR mode · onboarding wizard · ticket critique · Done/history. M0 runs one
hardcoded route; failures park the ticket in Needs You.

**Tickets (26):** GR-000, 001…012, 014, 017, 018, 020, 022, 024, 025, 026, 027, 028, 029, 037, 038.

**Start with GR-000.** It is a half-day spike against the real `claude` CLI, verifying the four
assumptions the architecture rests on — headless execution, streaming output, session resume, and
tool-use gating. If resume does not work as assumed, GR-021 changes shape, and that is far cheaper
to learn before Wave 0 than after Wave 3.

### M0 exit criteria

**The criterion that matters:** two weeks of real development, and the author is not opening
agent terminal windows.

Supporting checks:

1. `gravy serve` starts, survives the TUI closing, and reconciles orphaned runs on restart.
2. A ticket goes Ready → Done with no terminal juggling and no manual git.
3. **Three repositories run concurrently**, one agent each, all visible on one dashboard with
   project and branch on every row.
4. A project can be added with `gravy project add` — no YAML editing, no wizard (GR-037).
5. The review sweep clears a queue of pending reviews without returning to a list between them.
6. **Serial mode holds:** a repository with a ticket in Review starts nothing new, and the
   dashboard names the blocking ticket rather than showing an unexplained idle queue.
7. Each ticket's worktree is created from freshly-fetched target state, so queued work builds on
   what already merged.
8. A failed validation retries once, then parks the ticket visibly rather than hanging.
9. Killing a run from the TUI kills the subprocess and frees the slot.
10. Nothing merges without an explicit human approve.
11. **Gravy has written nothing into any repository** — no stray files, no bookkeeping commits.

---

## M1 — v0.1

**Goal.** The loop is trustworthy enough to leave running, honest about every automated decision,
and no longer single-provider.

**Adds:** `codex` adapter · router with fallback and cooldowns · result summaries · async ticket
critique · blocked/resume for agent questions · permission broker with allowlist and write-back ·
merge helper · PR land mode · parallel mode for projects that want it · onboarding wizard ·
Done/history · CLI subcommands · notifications across all reasons.

**Tickets (13):** GR-013, 015, 016, 019, 021, 023, 030, 031, 032, 033, 034, 035, 036.

### M1 exit criteria

1. Killing a provider's auth mid-run produces `provider_auth` in Needs You and a clean failover to
   the next route choice — not a mystery failure.
2. An agent needing an un-allowlisted action escalates, releases its slot, resumes correctly after
   "always allow for this project", and never asks for that action again.
3. A project switched to parallel mode runs several tickets at once; a resulting conflict reaches
   Needs You with a preserved worktree, and the merge helper resolves it — after which the result
   still passes through validation and human review.
4. Every automated decision is explainable in the TUI: why this host, why this model, why this
   retry, why this classification, why this queue is idle.
5. Onboarding takes a fresh machine to a first landed ticket without editing YAML by hand.
6. `gravy ticket add` and `gravy status` work with the TUI closed — proving the client seam.

---

## The real test

After M0, run actual work through Gravy for two weeks.

- **It worked** if the author reaches for `gravy` instead of opening three terminals, and the
  dashboard answers "what is happening, what needs me" without him reconstructing it.
- **It did not** if he opens the terminals anyway.

Note what is *not* in the test: throughput, agents run in parallel, tickets closed per day. If
Gravy runs one agent per repository and still ends the terminal juggling, it has succeeded. M1
should not begin in earnest until M0 has survived that fortnight.

---

## Deliberately deferred

| Deferred | Until there is evidence that |
|---|---|
| Remote hosts, `gravy-worker` | one machine is genuinely the constraint |
| Local model provider | a local agent loop produces mergeable work |
| Assisted / Planned ticket creation | Quick tickets are the bottleneck |
| Affected-component hints, auto-safe parallelism | manual parallel mode proves too blunt |
| Stacked tickets (branch from the previous ticket, not target) | serial mode's throughput cost bites in practice |
| Budgets, cost optimisation | routing alone does not control spend |
| Dependency graph visualisation | dependency chains get deep enough to need one |
| Web / desktop GUI | the TUI is genuinely limiting |
| RBAC, SSO, audit logs, org policy | more than one person uses an installation |
