# Gravy

```text
                           )    )    )
                          (    (    (
                           )    )    )
                          (    (    (

            ______________________________________
       .--'"                                      "`--.
    o (    .___________________________________.      \     .---.
   o   `.  |  ~   ~   ~   ~   ~   ~   ~   ~   ~ |       |___/     \
    o    `-|___________________________________|       |    .-.   |
   o        `-.                                      .-'   \  `-'  /
               `--.___                         ___.-'       `-.__.'
                      `--------.     .--------'
                               |     |
                         .-----'     `-----.
                         `-----------------'

       ######    #######      ####     ##    ##   ##    ##
      ##    ##   ##    ##    ##  ##    ##    ##   ##    ##
      ##         ##    ##   ##    ##   ##    ##    ##  ##
      ##  ####   #######    ########   ##    ##     ####
      ##    ##   ##   ##    ##    ##    ##  ##       ##
      ##    ##   ##    ##   ##    ##     ####        ##
       ######    ##    ##   ##    ##      ##         ##

           ----------   P O T   R O A S T   C O   ----------
```

**A keyboard-first CLI/TUI engineering manager for agentic software development.**

> The human manages ideas, tickets, priorities, and approvals.
> Gravy manages agents, models, machines, worktrees, retries, validation, and execution.

Gravy runs your existing coding agents — Claude Code, Codex, and GitHub Copilot CLI — as a managed fleet
against a ticket queue. Each ticket gets an isolated Git worktree. Work is validated
automatically. Nothing merges without you.

> **Status: alpha.** v0.1.0 is the first tagged release and is used daily on Linux and macOS.
> Expect rough edges and breaking changes before 1.0.

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

After installing an updated build, quit any open Gravy UI, run `gravy stop`, then start
`gravy` again so both the UI and daemon use it. Database migrations run automatically at
startup; existing single-command preview settings are preserved.

Gravy tells you when a newer release exists: the daemon asks GitHub once at startup and daily
after, and the status bar gains a `v0.1.4 available` note when there is something newer. It
never installs anything — upgrading is `brew upgrade`, a new tarball, or `make install`, and
stays your decision. Builds from source are left alone, since they are already ahead of the
newest release.

This is the only request Gravy makes on its own behalf. Turn it off and it makes none:

```yaml
# ~/.gravy/config.yaml
updates:
  check: false
```

Requires `git` and at least one coding agent CLI (`claude`, `codex`, or `copilot`) that you have already
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

**6 Running** follows one ticket from fetch to hand-off. The header shows the current phase,
elapsed time, live turns and tokens, the attempt against the self-correction budget, and
"no output for Xm" in amber when the agent goes quiet. Below it, the progress timeline lists
each step: fetch, worktree, prompt, agent start and exit, each validation step with its exit
code and duration, and retries with their reason. Press `t` to collapse or expand it; it opens
by default before the agent starts and during validation. The log pane shows agent events as
readable lines, with tool calls, messages, errors and thinking each styled differently. When a
retry starts, the log switches to the new attempt. When the ticket leaves Running, the screen
keeps its final timeline and says where the ticket went: "moved to Review · press 7", or
"parked in Needs You: <reason> · press 8". On the dashboard, each Running row shows the same
activity line.

On Ready, `space` sends tickets back to Backlog and `r` rejects them after confirmation.
Use `x` to select multiple tickets for either action. Backlog supports `D` to permanently delete a ticket.

To write a ticket manually, open **4 Backlog** and press **n**. Enter its title and body,
choose a project, and save with Enter. Tab moves between fields; pasted text works too.
It stays in Backlog until you press Space to queue it. Plan is optional, and no `CLAUDE.md`
is required. Gravy reads the instruction files that exist, including `AGENTS.md` and
`.github/copilot-instructions.md`, and skips missing ones.

Long text-entry fields wrap to the terminal width and keep the insertion cursor visible.
When input exceeds the available space, the view follows its end; the complete text is still
submitted. Every screen keeps its available keyboard shortcuts visible alongside status messages.
Long shortcut lists wrap instead of dropping actions, including after sending a ticket back
for changes.

On **7 Review**, **Shift+T** opens **Try feature**:

- **a** runs the project's preview services together in the ticket's worktree.
  Open the URL/window they provide. **Ctrl+C** stops all services and their children.
  If any service exits, Gravy stops the others and names the service that exited.
- **t** reruns validation commands in order. These manual checks do not replace
  recorded validation evidence or approve the ticket.
- **e** edits a single app command, or shows where to edit multiple services.
  **Esc** returns to the review card.

Configure multiple services under **Projects → c settings → preview services**. Enter names
separated by commas, such as `Backend, Frontend`. Each name adds three editable fields below it:

| Field | Backend example | Frontend example |
|---|---|---|
| directory | `backend` | `frontend` |
| command | `mix phx.server` | `flutter run -d chrome` |
| environment file | `.env` | leave empty |

Directories are relative to the ticket worktree. An environment file is optional and is
sourced as a shell file with its variables exported; its path is relative to the service's
directory, or it can be an absolute local path to a shared development environment file.
Gravy does not copy secrets into worktrees or infer which environment file to use.
Finish each field with Enter, then Esc saves the project. Remove a name from **preview services**
to remove that service. Names must be unique, and every service needs a command.

Services start concurrently, with output labeled by service name and no interactive input.
Configure commands to install dependencies first if needed (e.g. `mix deps.get && mix phx.server`).
Services must tolerate each other's startup time; Gravy does not wait for readiness or allocate
ports. Stop other servers using the same ports before launching a preview.
Missing directories or environment files are reported before any service starts.

For a single app, leave **preview services** empty and set **preview command** (e.g. `npm run dev`).
Existing single commands continue to work. Configured services take precedence over that command.
Output stays visible until Enter returns to Gravy. Previews run locally on Linux/macOS;
remote worktrees must be tested on their host.

For a Phoenix backend and Flutter frontend, a complete setup might be:

| Service | Directory | Command | Environment file |
|---|---|---|---|
| Backend | `backend` | `mix deps.get && mix phx.server` | `/absolute/path/to/project/backend/.env` |
| Frontend | `frontend` | `flutter pub get && flutter run -d chrome` | empty |

Use your own environment-file path. A shared absolute path works even when the ignored `.env`
file is absent from a new ticket worktree. The backend's database and other external services
must already be set up; previews do not create databases or apply migrations automatically.
Configure the frontend's API URL to match the backend, for example with
`flutter run -d chrome --dart-define=API_URL=http://127.0.0.1:4000/api`.

On Linux with Chromium, use `CHROME_EXECUTABLE=/usr/bin/chromium flutter run -d chrome`
as the launch portion of the frontend command, substituting your installed browser path.
On macOS with Google Chrome, `flutter run -d chrome` is usually sufficient.
The machine running the preview needs the project's toolchains on `PATH`.

No aliases or custom launcher are required. Commands run in a non-interactive shell, which
does not normally load aliases/functions from `.bashrc`. If a command reports "command not
found", use the actual executable or a script on `PATH` instead of an interactive-shell alias.
Project settings are saved in this Gravy installation; teammates configure them in their own
installation, using their own local environment-file and browser paths.

To try it: open a ticket awaiting Review, press **Shift+T**, check the listed services, then
press **a**. Backend and frontend logs share the terminal with service-name prefixes.
When finished, press **Ctrl+C**, then **Enter** to return to Review. Approval remains a separate
decision. If the frontend opens before the API is ready, wait for the backend's startup output
and reload the app.

### Model fallbacks

In **Settings**, edit the **agents** field under each **Bucket** section. Enter comma-separated
model choices in the order Gravy should try them:

| Bucket | agents |
|---|---|
| implementation | `claude-code/opus, codex/gpt-6-astra, claude-code/sonnet` |
| strong | `claude-code/opus, codex/gpt-6-astra` |

These are examples, not installed defaults. Enable and authenticate both provider CLIs on the
host that will run the ticket, and use model IDs available to your accounts. Global route edits
take effect without restarting. **Projects → c settings → buckets** can override a project's
routes; each overridden bucket replaces that bucket's global list, including its fallbacks.
The project override field uses `bucket=provider/model provider/model` entries separated by
commas, for example `strong=claude-code/opus codex/gpt-6-astra`.

When a provider reports a recognized quota or rate-limit failure, Gravy records a cooldown
and automatically returns the ticket to Ready. The next scheduler pass skips that choice
and selects the next usable fallback, preserving the worktree and feedback. No manual retry
is needed and the self-correction budget is unchanged. If every choice is cooling down, the
ticket waits in Ready without holding a worker; routing resumes when a choice becomes usable.
Authentication failures and interrupted runs still require attention. An ordinary task
failure does not trigger quota fallback. Check the run's recorded provider/model and routing
explanation to see which choice was used. Adding a fallback does not switch an already-running
agent mid-process.

The review card's **e editor** uses `VISUAL`, then `EDITOR`, from the environment Gravy was
started in. For VS Code, use `export VISUAL="code --wait"` with `code` on PATH; for Neovim,
use `export VISUAL=nvim`. Put that in your shell startup file to persist it. The editor opens
a separate review checkout showing the ticket's work as uncommitted changes; app/test commands
run in the ticket's original worktree.

For Copilot setup and testing without a subscription, see [Copilot setup](docs/COPILOT.md).

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
workers, `claude-code`, `codex`, and `copilot`, routing with fallback, validation, automated review, planning
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

Planning also follows the configured `planning` fallback order. When a provider reports a quota or rate limit, Gravy cools down that model and retries the same turn with the next available choice. It rebuilds the conversation for the fallback instead of passing another model's session ID. If every choice is unavailable, Plan shows an error; retry once a model becomes available. Restart Gravy after installing an updated binary to use this behavior.

If Gravy stops during automated review, startup recovery moves the ticket to **Review** and **Needs You**, preserving its work and any completed verdict. An interrupted verdict is marked unavailable. Select the ticket in Review and press **v** to rerun the advisory review. Human approval is still required before landing.

On **Projects (3)** or **Plan (2)**, press **/** while not editing to search registered projects by name, slug, or repository path. Type to filter, use **↑/↓** to select, and press **Enter** to choose; **Esc** cancels and **Ctrl+U** clears the search. Projects selects the matching row; Plan selects the project for a new conversation. To switch an existing conversation to another project, cancel search and use **x** to start over first.
