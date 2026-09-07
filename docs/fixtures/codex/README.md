# codex fixtures

Captured from `codex-cli 0.152.1` on 2026-09-07, with machine-local paths scrubbed.

- `exec-success.jsonl` — a complete `codex exec --json` run that edited one file. Shows
  `thread.started` (the thread id used for resume), `item.started`/`item.completed` for
  `agent_message`, `command_execution` and `file_change`, and `turn.completed` carrying token
  usage. Note there is no cost field: codex reports tokens but not a price.
- `exec-model-rejected.jsonl` — `codex exec -m no-such-model-9000`, which exits 1 and emits an
  `error` plus a `turn.failed`, both carrying the upstream envelope as an escaped JSON string
  with `"status":400`. This is the only failure mode the classifier's table has observed
  evidence for; the 401, 429, 5xx and quota rules are marked UNVERIFIED in `classify.go`.
