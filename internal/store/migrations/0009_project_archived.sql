-- A finished repository leaves the working set without losing its history.
--
-- Deleting the project would take its tickets, runs and summaries with it, which is the wrong
-- trade for work that is simply over: the record of what was built there is the reason to keep
-- it. Archived projects are still read by review, summaries and history; they are only left out
-- of the lists that answer "what am I doing this week".
--
-- Existing rows default to 0, so every project registered before this migration reads as active.
ALTER TABLE projects ADD COLUMN archived INTEGER NOT NULL DEFAULT 0;
