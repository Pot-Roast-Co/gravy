-- The automated reviewer's advisory verdict, stored as JSON on the run that produced the diff.
--
-- On the run rather than the ticket: a ticket sent back and retried gets a new diff and deserves
-- a new opinion, and keeping the old one would attach an assessment to code that no longer
-- exists. It is advisory in the schema too — nothing reads it to make a decision.
ALTER TABLE runs ADD COLUMN verdict TEXT NOT NULL DEFAULT '';
