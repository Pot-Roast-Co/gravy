-- The per-ticket progress journal the orchestrator narrates into.
--
-- Hung off the ticket rather than the run because the interesting phases start before a run row
-- exists: the fetch, the worktree and the first prompt build all happen while the ticket is
-- Assigned and nothing has been launched yet, and landing happens long after the run has ended.
-- run_id is therefore nullable and is filled in whenever the entry belongs to one.
--
-- Append-only, and deliberately without retention: a journal that prunes itself is a journal you
-- cannot trust to explain a run you came back to in the morning.
CREATE TABLE ticket_progress (
  id        TEXT PRIMARY KEY,
  ticket_id TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
  run_id    TEXT,                             -- NULL before a run exists, and during landing
  at        INTEGER NOT NULL,                 -- unix seconds; rowid breaks ties within one
  phase     TEXT NOT NULL,                    -- fetch | worktree | prompt | agent_start | ...
  detail    TEXT NOT NULL                     -- one human sentence
);
CREATE INDEX idx_ticket_progress_ticket ON ticket_progress(ticket_id, at);
