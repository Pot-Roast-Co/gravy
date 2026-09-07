-- Feedback a human wrote when sending work back for another attempt.
--
-- It lives on the ticket rather than in the run that prompted it: the next run has not been
-- created yet when the reviewer types it, and it is the ticket that carries the instruction
-- forward. Cleared when the ticket is approved or rejected.
ALTER TABLE tickets ADD COLUMN feedback TEXT NOT NULL DEFAULT '';
