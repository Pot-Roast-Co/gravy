-- The progress journal becomes the ticket's history.
--
-- One append-only table rather than a second one for events: the journal already hangs off the
-- ticket, already orders by time and rowid, and already tolerates run_id being absent. What it
-- lacked was who did a thing, and the facts behind the sentence in a form a program can read.
--
-- The table keeps its name, and phase keeps its column name, so every row written before this
-- still reads — the defaults describe those rows truthfully, since only the orchestrator wrote
-- them.
ALTER TABLE ticket_progress ADD COLUMN actor   TEXT NOT NULL DEFAULT 'gravy'; -- human | gravy | agent:<p>/<m> | worker:<p>/<m>
ALTER TABLE ticket_progress ADD COLUMN payload TEXT NOT NULL DEFAULT '{}';    -- JSON object
