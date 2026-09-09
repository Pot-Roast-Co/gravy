-- What a project is for: goals, constraints, decisions taken.
--
-- Read by planning, which is the difference between proposing work for a repository and
-- proposing work for a project somebody actually has intentions about. A project may have notes
-- and no repository at all while the shape of the thing is still being decided.
ALTER TABLE projects ADD COLUMN notes TEXT NOT NULL DEFAULT '';
