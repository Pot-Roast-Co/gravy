# GitHub Copilot CLI

Gravy can drive the standalone `copilot` coding agent. The old `gh copilot` extension is not
supported. Your friend needs a GitHub account with access to Copilot CLI and an authenticated
CLI on the machine where the project runs.

1. Install [GitHub Copilot CLI](https://docs.github.com/en/copilot/how-tos/copilot-cli/set-up-copilot-cli).
2. Run `copilot login`, then try a small prompt directly in Copilot to verify account access.
3. Start this build of Gravy. Setup/Settings lists `copilot` alongside the other compiled-in agents.
4. Set the desired buckets to `copilot/default`. For a Copilot-only setup, set implementation,
   review, and planning (if used) to Copilot; default buckets otherwise still name Claude Code.
   In Settings, use the bucket editor. Project-specific bucket overrides take precedence.
5. Add the project, write a small ticket with **Backlog → n**, and queue it with Space.
   No Plan session or `CLAUDE.md` is required. Review and approve the result yourself.

Equivalent config entries (merge these with your existing config):

```yaml
providers:
  copilot:
    enabled: true
    command: copilot
routes:
  implementation: [copilot/default]
  review: [copilot/default]
  planning: [copilot/default]
  cheap: [copilot/default]
  standard: [copilot/default]
  strong: [copilot/default]
```

`default` means the CLI chooses its configured model. Explicit account-supported model IDs
can be used instead, as `copilot/<model-id>`. Gravy neither stores your GitHub token nor handles
login. Authentication detection does not make a model request; account entitlement and access
to a particular model are verified only when a real run starts.

## What was tested without a subscription

Verified against Copilot CLI **1.0.83**:

- CLI flags and the SDK's framed `auth.getStatus` response.
- A real programmatic run against a local fake model endpoint, in offline mode with isolated
  Copilot state: streamed output, an actual file edit, final outcome, and session resume.
- Unit tests for permission arguments, denials, missing/malformed results, failures, timeout,
  model passthrough, usage, and preserving the worktree/permissions on resume.

Run all offline adapter tests:

```sh
go test -race ./internal/provider/adapters/copilot
```

To drive an installed CLI against the local fake model (no account or paid requests):

```sh
GRAVY_COPILOT_CLI=/absolute/path/to/copilot go test -run TestRealCLIWithFakeModel -v ./internal/provider/adapters/copilot
```

Your friend can run the opt-in account test, which consumes Copilot requests and creates a file
in a temporary directory:

```sh
GRAVY_COPILOT_LIVE=1 go test -run TestLiveCopilotAccount -v ./internal/provider/adapters/copilot
```

The local fake-model test does **not** prove GitHub account entitlement, organizational policy,
quota behavior, or a particular hosted model. Hosted success has not been verified here.

## Adapter behavior

The adapter requests native read/write permissions and translates project command permissions
into Copilot `shell(...)` patterns. It does not request `--allow-all` or `--allow-all-tools`.
Patterns that cannot be represented safely (such as regular expressions) fail with an explanation.
Copilot's native saved permissions and path verification also apply. The Ask file's directory is
explicitly allowed so Gravy's escalation channel can be written outside the worktree.

There is no normal-turn cap equivalent to Gravy's MaxTurns; the wall-clock timeout applies.
Token counts are recorded when emitted; absent USD cost remains unknown, and Copilot's credit
or request units are not treated as dollars. Unknown failures remain task failures, never quota
failures. CLI version changes may change the stream; raw output is kept in the run log.

Sources: [CLI command reference](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-command-reference),
[programmatic permissions](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-programmatic-reference),
[SDK client protocol](https://github.com/github/copilot-sdk/blob/main/nodejs/src/client.ts),
[event schema](https://github.com/github/copilot-sdk/blob/main/nodejs/src/generated/session-events.ts).
