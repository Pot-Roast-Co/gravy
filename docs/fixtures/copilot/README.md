# Copilot CLI fixture

`offline-edit.jsonl` contains selected, unmodified JSONL lines emitted by Copilot CLI 1.0.83
in `TestRealCLIWithFakeModel` on 2026-09-13. A local fake model requested a real `create` tool
call, then returned a completion. Only message, tool, turn-end and result events are retained.
This is real CLI output from a simulated model, not a captured GitHub-hosted model run.
