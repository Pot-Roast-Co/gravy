-- Every project names the machine its clone is on.
--
-- An empty host_id used to mean "any host meeting the requirements", which is wrong: a project
-- registered without one had its path validated on the local machine, so that is where its clone
-- is. Left ambiguous, the scheduler sent a local project's agent to a remote host while its
-- worktree was created here — the run and the code on different machines.
UPDATE projects SET host_id = 'local' WHERE host_id = '';
