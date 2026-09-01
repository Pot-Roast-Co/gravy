-- Initial schema, implementing ARCHITECTURE.md §5.

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
  parallel_mode   INTEGER NOT NULL DEFAULT 0,      -- 0 = serial: one ticket in flight, through merge
  max_concurrency INTEGER NOT NULL DEFAULT 1,      -- only consulted when parallel_mode = 1
  created_at      INTEGER NOT NULL
);

CREATE TABLE tickets (
  id            TEXT PRIMARY KEY,
  project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  title         TEXT NOT NULL,
  body          TEXT NOT NULL,
  state         TEXT NOT NULL,                     -- see ARCHITECTURE.md §6
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
CREATE INDEX idx_runs_ticket ON runs(ticket_id, started_at DESC);

CREATE TABLE validations (
  id          TEXT PRIMARY KEY,
  run_id      TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  step        TEXT NOT NULL,
  exit_code   INTEGER NOT NULL,
  duration_ms INTEGER NOT NULL,
  log_path    TEXT NOT NULL
);
CREATE INDEX idx_validations_run ON validations(run_id);

CREATE TABLE summaries (
  ticket_id   TEXT PRIMARY KEY REFERENCES tickets(id) ON DELETE CASCADE,
  run_id      TEXT NOT NULL REFERENCES runs(id),
  branch      TEXT NOT NULL,
  commits     TEXT NOT NULL,   -- JSON
  files       TEXT NOT NULL,   -- JSON: path, +, -
  narrative   TEXT NOT NULL,   -- markdown, generated from the diff
  assumptions TEXT NOT NULL DEFAULT '[]',
  created_at  INTEGER NOT NULL
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
