package config

// defaultFile is written on first run. Every setting appears with its default value and a
// comment explaining it, so the file itself is the documentation.
//
// It must parse to exactly Default(); TestDefaultFileMatchesDefault enforces that, so the two
// cannot drift apart.
const defaultFile = `# ~/.gravy/config.yaml — global Gravy configuration.
#
# Per-project settings (target branch, validation steps, allowlist, merge mode) are not here:
# they live in the database and are edited from the TUI, so this file stays small.
#
# Every value below is the default. Delete any line to keep the default.

concurrency:
  # Worker slots shared across all projects. Project availability, not this number, limits any
  # single repository: with the default serial mode, N projects run N agents concurrently, one
  # per repository.
  workers: 4

  # Per-route limits: how many tickets on each route may be in flight at once, across every
  # project. A route is a bucket of agent capacity — tickets ask for one by name with
  # "gravy ticket add -route planning" — so this is where "two planning agents and four
  # implementation agents" is expressed.
  #
  # An unlisted route is limited only by the workers setting above. Uncomment to cap one:
  # routes:
  #   planning: 1
  #   implementation: 3
  #   review: 2

providers:
  # The coding agent CLIs Gravy may drive. Gravy uses the CLIs you have already authenticated —
  # it does not want your credentials and does not store them, so there is no API key here.
  claude-code:
    enabled: true
    command: claude
  codex:
    enabled: true
    command: codex

routes:
  # Tickets request a route, never a model. Each route is an ordered list of "provider/model"
  # choices.
  #
  # A route can only name a provider this build has an adapter for — the list above selects
  # among what is compiled in, it cannot add a new agent. Naming an unknown provider is
  # ignored rather than silently becoming the default.
  #
  # In v0.1 the FIRST usable choice is used for everything and there is no fallback: quota and
  # rate-limit cooldowns arrive with the router (GR-016). Reorder these to change which agent
  # runs your work.
  implementation:
    - claude-code/sonnet
    - claude-code/haiku
  # Codex picks its own model on a ChatGPT-account login and rejects explicit names, so the
  # "default" here means "do not ask for one". On an API-key login you can name a real model.
  # Move this above claude-code to run your work through Codex instead.
  # - codex/default
  review:
    - claude-code/sonnet
  cheap:
    - claude-code/haiku
  standard:
    - claude-code/sonnet
  strong:
    - claude-code/opus
  planning:
    - claude-code/opus
  # Local models are not supported in v0.1; this route falls through to the next choice.
  local: []

timeouts:
  # Wall-clock cap on a single agent run.
  #
  # Keep this comfortably above any provider's internal retry window, or healthy-but-slow runs
  # get killed as false positives: an unauthenticated claude CLI was measured retrying silently
  # for 188 seconds before reporting a 401.
  run: 30m
  # Cap on a single validation command (build, test, lint).
  validation_step: 10m
  # How long a run may emit no events at all before it is treated as hung. This is a better
  # liveness signal than the wall clock, because a wedged run and a slow one look identical
  # from the outside. Set to 0 to disable.
  stall: 5m

retry:
  # How many times a red validation goes back to the agent before the ticket parks in Needs You.
  self_correction_budget: 1
  # How long a model is considered unavailable after each kind of provider-side failure. When a
  # provider reports its own reset time, that is used instead of these.
  cooldown_quota: 1h
  cooldown_rate_limit: 5m
  cooldown_unavailable: 15m

context:
  # Hard cap on the context assembled for a ticket. Agents get the ticket, capped excerpts of
  # project docs, and the result summaries of dependency tickets — never their transcripts.
  token_budget: 60000

notifications:
  # off | bell | bell_and_os
  mode: bell_and_os
  # Coalesce a burst of attention items into one alert, so a wave of finishing runs does not
  # produce a wave of pings.
  rate_limit_window: 30s

retention:
  # How long a finished run's agent output and event stream are kept on disk.
  # Summaries are not covered by this: they live in the database and are what a dependent
  # ticket reads as fact, so they outlive the logs they were derived from. 0 keeps forever.
  run_logs: 336h
`
