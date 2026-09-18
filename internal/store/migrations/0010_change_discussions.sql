-- The discussion that agrees a correction before implementation resumes, and the instructions
-- it produced.
--
-- Two tables rather than one because they have different lifetimes. A discussion is about one
-- review of one ticket and stops mattering once it is sent or abandoned; the instructions it
-- agreed outlive it, because the preservation constraints from the first correction are still
-- binding during the third. Folding them together would mean re-reading old transcripts to find
-- out what a ticket has already promised.
CREATE TABLE change_discussions (
  id         TEXT PRIMARY KEY,
  ticket_id  TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
  -- The run whose diff is under discussion. Empty when the ticket has never run.
  run_id     TEXT NOT NULL DEFAULT '',
  state      TEXT NOT NULL,                  -- open | sent | canceled
  session    TEXT NOT NULL DEFAULT '',       -- opaque provider session, for resuming
  agent      TEXT NOT NULL DEFAULT '',       -- provider/model holding the conversation
  messages   TEXT NOT NULL DEFAULT '[]',     -- JSON: []core.DiscussionMessage
  proposal   TEXT NOT NULL DEFAULT '{}',     -- JSON: core.ChangeInstruction, the editable draft
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX idx_change_discussions_ticket ON change_discussions(ticket_id, created_at);

-- One agreed correction. Written only when a human explicitly confirms it, which is what makes
-- this table the record of what was authorised rather than of what was discussed.
CREATE TABLE change_instructions (
  id            TEXT PRIMARY KEY,
  ticket_id     TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
  discussion_id TEXT NOT NULL DEFAULT '',
  correction    TEXT NOT NULL,
  preserve      TEXT NOT NULL DEFAULT '[]',  -- JSON: []string, still binding in later rounds
  verify        TEXT NOT NULL DEFAULT '[]',  -- JSON: []string
  created_at    INTEGER NOT NULL
);
CREATE INDEX idx_change_instructions_ticket ON change_instructions(ticket_id, created_at);
