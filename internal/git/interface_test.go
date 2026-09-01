package git

// LocalRepo must satisfy Repo; interface drift should fail to compile, not fail at wiring-up.
var _ Repo = (*LocalRepo)(nil)
