# SPIKE — claude-code driveability (GR-000)

**Status:** complete. **Date:** 2026-08-31. **Timebox:** half a day.
**Binary under test:** `claude` (Claude Code CLI), macOS arm64, Go 1.23.2 host.

Throwaway investigation verifying the four assumptions the architecture rests on. No spike code
merged; the only artefacts kept are this document and `docs/fixtures/claude-code/`, which exist so
**GR-012 can be written without re-deriving anything**.

## Verdict

| # | Assumption | Answer |
|---|---|---|
| 1 | Non-interactive execution, meaningful exit status | **YES** |
| 2 | Structured streaming output | **YES** |
| 3 | Session capture and resume | **YES** |
| 4 | Permission control via pre-tool hook | **YES** |

**All four are yes.** No architectural rework follows. **GR-021 keeps its designed shape** — the
blocked/resume mechanism is viable exactly as specified. GR-012 and GR-035 are written against the
flags recorded below rather than against assumptions.

Four findings do change how GR-012, GR-014, and GR-008 must be *written*. They are in
[Findings that change tickets](#findings-that-change-tickets), and they are the real value of this
spike.

---

## 1. Non-interactive execution — YES

```sh
claude -p '<prompt>' \
  --output-format json \
  --model <model> \
  --session-id <uuid> \
  --permission-mode acceptEdits \
  < /dev/null
```

Runs headless in the process's working directory with no TTY and stdin closed. Exit `0` on
success. The working directory is the unit of scope, which is what makes the **worktree the blast
radius** (`PRODUCT.md`): the run was confined to the scratch repo and edited only `main.go` there.

`--print` also skips the workspace-trust dialog, so a fresh worktree does not stall on first use.

Result object (`docs/fixtures/claude-code/result-success.json`) carries everything Gravy records
for a run: `session_id`, `is_error`, `subtype`, `terminal_reason`, `num_turns`, `stop_reason`,
`total_cost_usd`, a full `usage` breakdown, per-model `modelUsage`, and `permission_denials`.

Flags worth having in the adapter: `--max-budget-usd` (hard spend cap per run),
`--fallback-model` (only works with `--print`), `--no-session-persistence`, `--strict-mcp-config`,
and `--bare`/`--safe-mode` for reproducibility against a user's local customisations.

## 2. Structured streaming output — YES

```sh
claude -p '<prompt>' --output-format stream-json --verbose
```

NDJSON, one event per line, emitted **as it happens** — not buffered to exit. `--verbose` is
required. Observed sequence for a two-tool edit task:

```
system/init → rate_limit_event → system/thinking_tokens × N
  → assistant (tool_use: Read) → user (tool_result)
  → assistant (tool_use: Edit) → user (tool_result)
  → assistant (text) → result/success
```

- `system/init` arrives **first** and carries `session_id`, `cwd`, `model`, `permissionMode`, and
  the full tool list — enough to populate a Running row before any model output.
- `assistant` events carry `tool_use` blocks (name + input); `user` events carry `tool_result`.
  This is the live activity feed the Running view needs.
- `system/thinking_tokens` gives incremental progress during long thinking stretches — a useful
  liveness signal for distinguishing "working" from "stalled".

Sample: `docs/fixtures/claude-code/stream-tool-use.jsonl`.

## 3. Session capture and resume — YES

Gravy **pre-assigns** the session id rather than scraping it:

```sh
claude -p '<first prompt>'  --session-id 111...555   # Gravy generates the UUID
claude -p '<injected msg>'  --resume 111...555       # prior context intact
```

Verified: after a run that edited `main.go` via the Edit tool, a *separate later process* resuming
that id correctly answered which word it wrote and which tool it used. Prior context — including
tool history — survives.

The resumed run **returns the same `session_id`**; resume does not mint a new one.
`--fork-session` opts into a new id when a branch is wanted.

**Consequence for GR-021:** the blocked/resume design holds. Gravy generates a UUID at run start,
stores it on the `run` row, and resumes with an injected message when a human answers a question
or grants a permission. No scraping, no id-reconciliation logic, no fallback design needed.

## 4. Permission control — YES

A `PreToolUse` hook, supplied per-run via `--settings` (a file path or a JSON string, so Gravy can
pass a per-ticket allowlist without writing into the repo):

```json
{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[
  {"type":"command","command":"/path/to/broker"}]}]}}
```

The hook receives the call on **stdin**:

```json
{"session_id":"...","cwd":"...","permission_mode":"acceptEdits",
 "hook_event_name":"PreToolUse","tool_name":"Bash",
 "tool_input":{"command":"echo hello","description":"..."},
 "tool_use_id":"toolu_01JU4w..."}
```

and decides on **stdout**:

```json
{"hookSpecificOutput":{"hookEventName":"PreToolUse",
  "permissionDecision":"deny",
  "permissionDecisionReason":"Gravy allowlist: Bash is not permitted for this ticket."}}
```

`deny` is honoured — the call never executes. The denial is also reported in the run result:

```json
"permission_denials":[{"tool_name":"Bash","tool_use_id":"toolu_01JU4w...",
  "tool_input":{"command":"echo hello","description":"..."}}]
```

That object is precisely GR-035's escalation payload: what was asked for, with arguments, ready to
show a human and to match against an allowlist on "always allow for this project".

`--allowedTools` / `--disallowedTools` accept patterns like `"Bash(git *)"` and are the cheap
static path; the hook is the dynamic path that a live broker needs. Use both.

---

## Findings that change tickets

### F1 — `subtype:"success"` is not a success signal *(GR-012)*

An unrecognized model returned:

```json
{"subtype":"success","is_error":true,"api_error_status":404,"terminal_reason":"api_error"}
```

`subtype` stayed `"success"` on a hard 404. **An adapter that keys off `subtype == "success"`
reports a failed run as a successful one** — and since Gravy's result summaries come from the diff
rather than the transcript, an empty diff would be summarised as "no changes" instead of surfacing
the failure.

Trust, in order: process **exit code**, then `is_error`, then `terminal_reason`, then
`api_error_status`. Never `subtype` alone. **This is the test GR-012 must have.**

Fixtures: `result-unrecognized-model.json`, `stderr-unrecognized-model.txt`.

### F2 — auth failure retries silently for minutes before reporting *(GR-008, GR-012)*

With a bogus `ANTHROPIC_API_KEY`, `claude --bare -p` produced **no output and no exit for 188
seconds** (measured, single run), then terminated cleanly:

```json
{"subtype":"success","is_error":true,"api_error_status":401,
 "terminal_reason":"api_error","result":"Failed to authenticate. API Error: 401 API key is invalid."}
```

Exit code `1`. So the failure *is* reported correctly and is trivially classifiable — the problem
is purely **latency**: the CLI retries with backoff and stays completely silent while doing so.
Contrast the unrecognized model, which failed in about a second.

A wedged run is therefore indistinguishable from a slow one by process state alone. Consequences:

- **The per-run timeout in GR-008 is not a safety net, it is the primary detector.** It must exist
  from the first line of `LocalHost`, not be deferred.
- Liveness should be judged from the **event stream** (`system/thinking_tokens`, `assistant`,
  `tool_use`), not from the process being alive. A run emitting nothing after `system/init` for a
  configurable interval is stalled — a stronger signal than the wall clock, and the basis for the
  "why is this idle" explainability the TUI owes the user.
- M1 exit criterion 1 — "killing a provider's auth mid-run produces `provider_auth` in Needs You
  and a clean failover" — is achievable (the 401 is unambiguous) but **will not be timely** if
  Gravy simply waits. The stall detector is what makes the failover feel clean rather than hung.
- Default per-run timeouts must comfortably exceed this ~3-minute retry window, or a
  healthy-but-slow run gets killed as a false positive. Note the tension: the timeout must be
  long enough not to kill real work, which is exactly why stream-liveness is the better primary
  signal — a silent 188s is unambiguous, while a 188s wall-clock threshold is not.

Fixture: `result-auth-401.json`. This is also a **second independent confirmation of F1** —
`subtype` again reads `"success"` on a hard 401.

### F3 — machine-readable error tags on stderr *(GR-014)*

stderr carries tagged, parseable errors alongside the human text:

```
[claude-code:unrecognized_model] {"model":"no-such-model-9000","query_source":"sdk"}
```

`Classify` should parse `[claude-code:<code>] <json>` as its primary signal and fall back to
message-text matching only when no tag is present. This is far more stable than regexing prose.

Per the standing invariant, `unrecognized_model` classifies as **`TaskFailure`, never as a quota
condition** — it is a configuration error, and escalating it to a stronger model would burn the
expensive route on a request that will 404 again.

### F4 — proactive quota telemetry exists; do not classify quota only from errors *(GR-014, router)*

The stream carries a `rate_limit_event` **on a perfectly healthy run**:

```json
{"type":"rate_limit_event","rate_limit_info":{
  "status":"allowed","rateLimitType":"five_hour","resetsAt":1788235200,
  "overageStatus":"allowed","isUsingOverage":false,
  "unifiedWindows":{"five_hour":{"utilization":0.68,"resetsAt":1788235200},
                    "seven_day":{"utilization":0.14,"resetsAt":1788714000}}}}
```

Utilization per window, and an exact `resetsAt`. This is strictly better than the designed
approach of inferring quota state from failures after the fact:

- **Cooldowns get a real expiry.** `resetsAt` is authoritative — no guessed backoff.
- The router can **de-prioritise a route approaching exhaustion** before a run fails, rather than
  spending a ticket's run to discover it.
- `status`/`overageStatus` distinguish a genuine quota stop from an ordinary failure directly,
  which is exactly the discrimination the "unknown errors are `TaskFailure`" invariant protects.

Recommendation: the adapter records the latest `rate_limit_info` per provider on every run, and
GR-014 treats it as a first-class input beside error classification. Still M1 — but GR-012 should
**capture and store these events in M0** so the data is already there when the router lands.

### F5 — three different ways a run reports success having done nothing *(GR-012, GR-015, GR-018, GR-021)*

Added after building the adapter and running it for real. Beyond F1's `subtype`, the CLI reports
`exit 0`, `is_error: false` and no error of any kind in **three distinct situations where the
requested work did not happen**:

1. **Every tool call denied.** With no permission mode set, a task requiring `Write` had that
   call refused; the file was never created. The only evidence is a `permission_denials` array.
   *Handled:* the adapter classifies a run with denials as `TaskFailure`, never Success.
2. **A gated command denied mid-run.** Under `--permission-mode acceptEdits`, `curl` was denied
   as expected. Same shape, same handling.
3. **The agent stopped to ask a question.** Given a destructive command, the agent explained
   what it would do and asked for confirmation instead of proceeding. Zero denials, no error,
   exit 0 — and nothing done. **Nothing in the result object distinguishes this from a run that
   genuinely had no work to do.**

The third is not yet handled and cannot be handled in the adapter alone:

- **GR-015** must instruct the agent that on a genuine ambiguity it writes `AskPath` and stops
  (§6.2). That converts case 3 into a detectable file rather than prose in the result.
- **GR-018** should treat *success with an empty diff* as suspicious in its own right. Since
  summaries are generated from the diff, an unhandled case 3 becomes a review item that says
  "no changes" about an agent that was actually waiting for an answer.
- **GR-021** turns the `AskPath` file into the Blocked state and a Needs You entry.

The general lesson, now observed three times: **this CLI's success fields report that the process
completed, not that the work happened.** Every consumer should corroborate against something
external — the diff, the denials array, `AskPath` — rather than trusting the outcome alone.

### F6 — what `acceptEdits` actually gates *(GR-012, GR-035)*

Measured, since the M0 default rests on it:

| Action | Under `acceptEdits` |
|---|---|
| Read files in the worktree | allowed |
| Edit / Write in the worktree | allowed |
| Ordinary shell (`ls`, `find`, `git diff`) | allowed |
| Network (`curl`) | **denied** |
| Destructive shell (`rm` of a tracked file) | agent stops and asks |

This lines up closely with the product's intended default allowlist — read and write anywhere in
the worktree, common read-only shell, network off — which is why the adapter uses it rather than
the always-allow hook stub GR-012's scope suggested. An always-allow stub would have removed the
only brake that exists before GR-035 lands. GR-035 still replaces this with real per-project
matching; `acceptEdits` is a coarse approximation, not the allowlist.

---

## Notes for GR-012

- Always pass `--session-id <uuid>` (Gravy-generated). Never scrape an id.
- `--output-format stream-json --verbose`, parse NDJSON line-by-line; a `result` line ends the run.
- Treat unparseable lines as non-fatal — log and continue. Do not fail a run on one bad line.
- Per-run `--settings` JSON is how the permission allowlist is injected without writing into the
  repository (**M0 exit criterion 11: Gravy writes nothing into any repo**).
- Consider `--max-budget-usd` as a per-run backstop and `--strict-mcp-config` for reproducibility.
- Fixtures in `docs/fixtures/claude-code/` are real captured output with machine-local absolute
  paths replaced by `/WORKTREE`.

## Not answered

- Behaviour when the 5-hour window is genuinely exhausted (could not induce it). `rate_limit_event`
  shape is captured for `allowed`; the exhausted-state text is unverified — seed the fixture when
  first observed in the wild.
- `codex` driveability. Separate concern, M1, GR-013.
