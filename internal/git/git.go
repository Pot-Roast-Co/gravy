package git

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
)

// Worktree is an isolated working copy for one ticket.
type Worktree struct {
	// Path is the absolute directory the agent works in. It is the blast radius.
	Path string
	// Branch is the branch checked out there, gravy/<ticket-id>-<slug>.
	Branch string
	// Base is the ref the branch was created from. It is recorded so a rebase knows what
	// "moved underneath this work" means.
	Base string
}

// RebaseResult reports whether a rebase replayed cleanly.
type RebaseResult struct {
	Clean         bool
	ConflictFiles []string
	// Refused reports that git would not start the rebase at all, as opposed to starting one
	// and hitting a conflict. The two look identical from an exit code and are nothing alike:
	// a conflict needs the human to resolve overlapping edits, a refusal needs them to deal
	// with something in the worktree.
	Refused bool
	// Detail is git's own explanation, kept because it is invariably more useful than any
	// summary of it. Empty when git said nothing.
	Detail string
	// Replayed reports whether any commits were actually moved.
	Replayed bool
}

// FileDiff is one file's contribution to a diff.
type FileDiff struct {
	Path      string
	Status    string // added, modified, deleted, renamed
	Additions int
	Deletions int
	Patch     string
}

// Diff is a set of file changes.
type Diff struct {
	Files []FileDiff
}

// Totals returns the summed additions and deletions.
func (d Diff) Totals() (additions, deletions int) {
	for _, f := range d.Files {
		additions += f.Additions
		deletions += f.Deletions
	}
	return additions, deletions
}

// Repo is a git repository Gravy manages worktrees in.
type Repo interface {
	Fetch(ctx context.Context) error
	CreateWorktree(ctx context.Context, branch, base string) (Worktree, error)
	// OpenWorktree returns an existing worktree for a branch, if there is one.
	OpenWorktree(ctx context.Context, branch string) (Worktree, bool, error)
	RemoveWorktree(ctx context.Context, w Worktree) error
	Rebase(ctx context.Context, w Worktree, onto string) (RebaseResult, error)
	CommitAll(ctx context.Context, w Worktree, msg string) (string, error)
	Diff(ctx context.Context, w Worktree, base string) (Diff, error)
}

// runner executes git and returns its output. Every git invocation in this package goes through
// host.Host.Exec — this package never imports os/exec, which is what keeps a remote host a drop-in
// replacement (ARCHITECTURE.md §1.1).
type runner struct {
	host    host.Host
	timeout time.Duration
}

// result is a finished git invocation.
type result struct {
	stdout string
	stderr string
	code   int
}

// run executes git in dir and returns its output. A non-zero exit is not an error here; callers
// that care check result.code, because several git commands use exit status as information
// (rebase conflicts, diff --quiet) rather than as failure.
func (r runner) run(ctx context.Context, dir string, args ...string) (result, error) {
	p, err := r.host.Exec(ctx, host.ExecSpec{
		Cmd:     "git",
		Args:    args,
		Dir:     dir,
		Timeout: r.timeout,
		// Keep git non-interactive: a credential or editor prompt in an unattended run would
		// hang until the timeout instead of failing with something diagnosable.
		Env: map[string]string{
			"GIT_TERMINAL_PROMPT": "0",
			"GIT_EDITOR":          "true",
			"GIT_PAGER":           "cat",
		},
	})
	if err != nil {
		return result{}, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}

	// Drain both streams concurrently: a command that fills the stderr pipe while we read
	// stdout would otherwise deadlock.
	outCh := drain(p.Stdout())
	errCh := drain(p.Stderr())
	status, waitErr := p.Wait()
	out, errOut := <-outCh, <-errCh

	if waitErr != nil {
		return result{}, fmt.Errorf("git %s: %w", strings.Join(args, " "), waitErr)
	}
	if status.TimedOut {
		return result{}, fmt.Errorf("git %s: timed out after %s", strings.Join(args, " "), r.timeout)
	}
	return result{stdout: out, stderr: errOut, code: status.Code}, nil
}

// mustRun is run, treating a non-zero exit as an error with git's own stderr attached.
func (r runner) mustRun(ctx context.Context, dir string, args ...string) (string, error) {
	res, err := r.run(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	if res.code != 0 {
		return "", fmt.Errorf("git %s: exit %d: %s",
			strings.Join(args, " "), res.code, firstLine(res.stderr))
	}
	return res.stdout, nil
}

func drain(r io.Reader) <-chan string {
	ch := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		ch <- string(b)
	}()
	return ch
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
