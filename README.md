# Gravy

**A keyboard-first CLI/TUI engineering manager for agentic software development.**

> The human manages ideas, tickets, priorities, and approvals.
> Gravy manages agents, models, machines, worktrees, retries, validation, and execution.

Gravy runs your existing coding agents — Claude Code, Codex, more later — as a managed fleet
against a ticket queue. Each ticket gets an isolated Git worktree. Work is validated
automatically. Nothing merges without you.

> **Status: pre-alpha.** Under active development toward M0. Not yet usable.

## Why

Running several coding agents today means running several terminals, and becoming a manual
scheduler: hand-managing worktrees, remembering which agent has what, noticing when one finished
or silently stalled, re-running tests by hand, rebasing, merging, and deciding what each freed-up
agent should do next.

None of that is engineering judgment. Gravy automates the dispatch and protects the judgment.

## The loop

```
Ticket → Backlog → Ready → Running → Validation → Review → Approve → Merge → Done
                     ▲                                        │
                     └──────── idle worker takes the next ────┘
```

Idle workers automatically claim eligible Ready tickets. You write tickets and approve diffs.

## What it does

- **One dashboard for every repository.** Which agent is on which repo and branch, what is
  waiting, what finished — one screen instead of a wall of terminals. This is the point of Gravy.
- **Isolated worktrees.** Every active ticket gets its own, created from freshly-fetched target
  state so queued work builds on whatever merged before it.
- **Conservative by default.** One ticket at a time per repository, carried through merge, so
  conflicts mostly cannot happen. Repositories run concurrently with each other. Parallel mode
  within a repository is opt-in, and you handle the conflicts.
- **A single "Needs You" queue.** Everything awaiting your judgment in one place — reviews,
  questions, permission requests, failures, conflicts. If it is not there, Gravy does not need you.
- **Automated validation.** Build, test, lint per project. Failures go back to the agent for a
  bounded number of self-corrections before reaching you.
- **A hard approval gate.** Gravy never merges unapproved work. Not for green tests, not for
  trivial diffs, not by configuration.
- **A merge gate.** On approval, work is rebased onto the current target and re-validated if the
  target moved. A conflict stops and asks you — resolve it yourself in the preserved worktree, or
  hand it to the merge helper, whose output still goes through review like anything else.
- **An automated review pass** over every diff before you see it, so routine work can be approved
  in seconds. Advisory only — it never decides anything.
- **Review sweep.** One keystroke walks the entire pending review queue in sequence — approve,
  next, approve, next — without returning to a list.
- **Planning as a conversation.** One screen reads the project's own documents, proposes the next
  piece of work, is grilled until it is right, and produces tickets you approve into the backlog.
  It never creates one on its own.
- **More than one machine.** A project belongs to the machine its clone is on, reached over ssh.
  An iOS project lives on the Mac and runs there; the scheduler refuses to send work anywhere its
  code is not, and says so.
- **Model routing.** Tickets request a route (`cheap`, `standard`, `strong`, `review`), not a
  model. Genuine quota and rate-limit conditions trigger cooldown and fallback; ordinary coding
  failures do not.
- **Explainability.** Why this host, why this model, why this retry, why this classification —
  always answerable in the UI.
- **Your CLIs, your subscriptions.** Gravy drives the agent CLIs you already have authenticated.
  It does not want your credentials and does not store them.

## Install

On macOS, Homebrew:

```sh
brew install pot-roast-co/tap/gravy
gravy            # launches the TUI; press P to add your first repository
```

The tap ships a cask, and Homebrew only installs casks on macOS — on Linux, download the
`.tar.gz` for your architecture from the
[releases page](https://github.com/pot-roast-co/gravy/releases) and put `gravy` on your `PATH`:

```sh
tar -xzf gravy_<version>_linux_amd64.tar.gz
install -m 0755 gravy ~/.local/bin/gravy
gravy
```

The same tarballs are published for macOS. The binaries are static; there is nothing else to
install, and Go is not required for either route.

If you already have Go:

```sh
go install github.com/pot-roast-co/gravy/cmd/gravy@latest
```

Or from a clone, which stamps the version and avoids `GOBIN`:

```sh
make install                    # -> ~/.local/bin/gravy
make install PREFIX=/usr/local/bin
```

`make install` is worth preferring over `go install` if you manage Go with mise or asdf: their
`GOBIN` lives inside the toolchain directory, so a `go install`ed binary disappears on the next
Go upgrade.

Requires `git` and at least one coding agent CLI (`claude` or `codex`) that you have already
logged in to. Gravy drives the CLIs; it never handles your credentials. Go 1.24+ is needed only
for the `go install` and `make install` routes.

**Linux and macOS.** Both are used daily. Windows is not supported: process groups, signals and
the daemon's socket are unix-specific, and pretending otherwise would fail at the first run
rather than at the install.

On first run, Gravy opens a five-step setup wizard. It checks agent logins and models, shows
local host capabilities, and lets you edit worker counts and ordered model choices. Enter a
repository path to inspect its toolchain, then review and edit suggested validation, host
requirements and permissions. **Nothing is saved until you approve the final step.**

Use **Ctrl+N** / **Ctrl+B** to move between steps, **Enter** to edit, and **Esc** to cancel.
On the final review, **a** approves and saves. An agent that is not logged in is explained but
does not prevent setup. Xcode suggestions may need your scheme and simulator destination.

Reopen the wizard with **W in Settings**. Leave the repository path blank to update global
settings without changing existing projects. Settings that need a daemon restart are listed
after saving. An installer is still on the roadmap.

## Usage

```sh
gravy                      # launch the TUI (starts the daemon if needed)
gravy serve                # run the daemon in the foreground
gravy stop                 # stop it; safe to run twice
gravy project add <path>   # register a repository (-host to put it on another machine)
gravy ticket add           # create a ticket without opening the TUI
gravy status               # what is running, what needs you
gravy review [<id>]        # what is awaiting judgement, and its diff
gravy approve <ticket-id>  # approve and land
```

Agents keep working when you close the TUI.

The TUI's number row is the ticket lifecycle, and `,` opens settings:

```
1 Dashboard  2 Plan  3 Projects  4 Backlog  5 Ready  6 Running  7 Review  8 Needs You
```

On the Review screen, `t` shows the ticket you are checking against, `e` opens the changes in
your editor as uncommitted changes so its git panel works, `!` opens a shell in the worktree, and
`v` asks for a fresh automated opinion.

## Documentation

| Document | Contents |
|---|---|
| [docs/PRODUCT.md](docs/PRODUCT.md) | What Gravy is, the principles, the lifecycle, review and permission semantics, v0.1 scope |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Layering rules, interfaces, data model, state machine, extension paths |
| [docs/MILESTONES.md](docs/MILESTONES.md) | M0 walking skeleton and M1 v0.1, with exit criteria |
| [docs/BACKLOG.md](docs/BACKLOG.md) | 35 implementation tickets in six concurrency waves |

## Safety

Stated plainly: **Gravy does not sandbox agents.** They run as your user, with your privileges.
The real boundaries are the **worktree** (blast radius), the **permission allowlist** (what an
agent may do without asking), and the **merge gate** (nothing reaches your target branch
unapproved). Every run has a timeout, a turn cap, and a kill switch.

An agent needing an action outside the allowlist stops and asks you — it is never auto-granted,
and never left hanging on a prompt you cannot see.

## Design notes

A few decisions that are deliberate rather than accidental:

- **Result summaries are generated from the diff, not written by the implementing agent.** A
  self-report from the party with a motive to declare success is not evidence.
- **Unrecognized provider errors are treated as task failures, not quota failures.** A wrong quota
  call silently escalates work to your most expensive model — the exact outcome routing exists to
  prevent.
- **Ticket critique never blocks ticket creation.** It arrives asynchronously as a suggestion you
  accept, edit, or dismiss. Your words are never rewritten without an explicit accept.
- **Serializing per repository costs throughput, and that is accepted.** With one ticket in flight
  per repo, your review latency gates that repo's queue. Gravy makes the cost visible — an idle
  queue always names the ticket it is waiting on — rather than hiding it. Organization was always
  the point; throughput is the secondary benefit.

## Roadmap

**Working today** — the full loop across several machines: projects, tickets, local and ssh
workers, `claude-code` and `codex`, routing with fallback, validation, automated review, planning
conversations, merge mode, notifications, and guided onboarding.

**Next** — an installer. Permission escalation when an agent is refused something, rather than it working
around the refusal. Done and history. Per-ticket spend, and budgets.

**Later** — PR land mode, merge helper, local model providers, dependency graphs. The
architecture keeps these cheap; none of them delay the loop.

## Contributing

Early and moving fast; the interfaces in `ARCHITECTURE.md` are the contract. Adding a provider
should require one package and one registry line — if it does not, that is a bug in the
abstraction and worth reporting.

## License

MIT — see [LICENSE](LICENSE).

### Notifications

`notifications.mode: bell_and_os` plays a quiet, bundled two-note chime and sends a desktop
notification. `bell` plays the chime alone; `off` disables both. Alerts share the configured
rate limit (30 seconds by default). Linux playback uses `pw-play`, falling back to `paplay`;
macOS uses `afplay`. If audio playback fails, Gravy falls back to a terminal bell.

On Omarchy, clicking the notification (including in notification history) focuses an existing Gravy terminal
and navigates to the ticket's Review or Needs You screen. A new terminal opens only if no
Gravy TUI is listening for this data directory. The destination is resolved when opened, so a
requeued ticket goes to its current queue. This uses Omarchy's persistent notification action;
other desktops currently receive the notification without this click integration.

Run `gravy notify-test` to preview the sound and desktop notification, or
`gravy notify-test <ticket-id>` to test opening a particular ticket. `gravy open <ticket-id>`
opens that ticket directly. These commands do not approve or run tickets.
