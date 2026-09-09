-- A project belongs to a machine, because its clone does.
--
-- Each host has its own checkout and its own worktrees, with no shared filesystem, so a
-- project's repo_path is meaningless on any host but the one it was registered on. Empty means
-- the local machine and any other host meeting the project's requirements.
ALTER TABLE projects ADD COLUMN host_id TEXT NOT NULL DEFAULT '';
