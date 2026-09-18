package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
)

// DefaultTimeout bounds any single git invocation. Fetches against a slow remote are the
// realistic worst case.
const DefaultTimeout = 5 * time.Minute

// LocalRepo manages worktrees for one repository through a Host.
type LocalRepo struct {
	// repoPath is the main working copy, the one the user cloned.
	repoPath string
	// worktreeRoot is where Gravy puts its worktrees: ~/.gravy/projects/<slug>/worktrees.
	//
	// Deliberately outside the repository. Gravy writes nothing into a repository for
	// bookkeeping, ever (ARCHITECTURE.md §3).
	worktreeRoot string
	runner       runner
}

// Host returns the machine this repository and its worktrees live on.
//
// Anything that runs a command inside one of those worktrees has to run it here: the path is
// interpreted on this host and means nothing to any other.
func (r *LocalRepo) Host() host.Host { return r.runner.host }

// Option configures a LocalRepo.
type Option func(*LocalRepo)

// WithTimeout overrides the per-command timeout.
func WithTimeout(d time.Duration) Option {
	return func(r *LocalRepo) { r.runner.timeout = d }
}

// NewLocalRepo returns a repository manager. worktreeRoot is created on demand.
func NewLocalRepo(h host.Host, repoPath, worktreeRoot string, opts ...Option) *LocalRepo {
	r := &LocalRepo{
		repoPath:     repoPath,
		worktreeRoot: worktreeRoot,
		runner:       runner{host: h, timeout: DefaultTimeout},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// DefaultIdentity is the author Gravy commits as when the machine has none configured.
//
// Gravy's own commits are the agent's work-in-progress, squashed away on landing, so the
// identity is bookkeeping rather than authorship. It matters only that committing works.
const (
	DefaultAuthorName  = "Gravy"
	DefaultAuthorEmail = "gravy@localhost"
)

// identityArgs returns the -c flags needed for git to accept a commit.
//
// Git refuses to commit with "Author identity unknown" when neither user.email nor user.name is
// configured, which is the normal state of a fresh CI runner, container, or newly provisioned
// machine — exactly where an unattended agent runs. The user's own identity is used whenever it
// is configured; the fallback exists so that a missing global git config cannot turn every run
// into a failure the human has to diagnose.
func (r *LocalRepo) identityArgs(ctx context.Context) []string {
	if r.hasIdentity(ctx) {
		return nil
	}
	return []string{
		"-c", "user.name=" + DefaultAuthorName,
		"-c", "user.email=" + DefaultAuthorEmail,
	}
}

func (r *LocalRepo) hasIdentity(ctx context.Context) bool {
	res, err := r.runner.run(ctx, r.repoPath, "config", "--get", "user.email")
	if err != nil {
		return false
	}
	return res.code == 0 && strings.TrimSpace(res.stdout) != ""
}

// Fetch updates remote-tracking refs.
//
// This runs per ticket at claim time, never as a batch: a queued ticket must start from whatever
// landed before it, which is only true if the fetch is immediately before the worktree is cut.
func (r *LocalRepo) Fetch(ctx context.Context) error {
	res, err := r.runner.run(ctx, r.repoPath, "fetch", "--prune", "--all")
	if err != nil {
		return err
	}
	if res.code != 0 {
		// A repository with no remote is a legitimate local-only project, not a failure.
		if strings.Contains(res.stderr, "does not appear to be a git repository") ||
			strings.Contains(res.stderr, "No remote repository specified") {
			return nil
		}
		return fmt.Errorf("git fetch: exit %d: %s", res.code, firstLine(res.stderr))
	}
	return nil
}

// TargetRef returns the ref that represents freshly-fetched target state for a branch.
//
// After a fetch it is the remote-tracking ref, origin/<branch>, NOT the local branch: a local
// branch does not move when you fetch, so cutting a worktree from it silently reuses whatever
// state the clone was last pulled to. That is precisely the bug that makes a queued ticket miss
// the work merged just before it — the ticket branches from yesterday and either reimplements
// the previous ticket or conflicts with it.
//
// A repository with no remote falls back to the local branch, which is correct there: with
// nothing to fetch from, the local branch is the target.
func (r *LocalRepo) TargetRef(ctx context.Context, branch string) (string, error) {
	for _, candidate := range []string{"origin/" + branch, branch} {
		res, err := r.runner.run(ctx, r.repoPath, "rev-parse", "--verify", "--quiet", candidate+"^{commit}")
		if err != nil {
			return "", err
		}
		if res.code == 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("target branch %q does not resolve locally or on origin", branch)
}

// CreateWorktree creates a branch at base and checks it out in its own directory.
//
// base is honoured as given and is never silently replaced with the target branch. That is what
// keeps stacked tickets — branching from the previous ticket rather than from target — a
// scheduler change later rather than a rewrite of this package (ARCHITECTURE.md §10).
func (r *LocalRepo) CreateWorktree(ctx context.Context, branch, base string) (Worktree, error) {
	if branch == "" {
		return Worktree{}, fmt.Errorf("create worktree: no branch given")
	}
	if base == "" {
		return Worktree{}, fmt.Errorf("create worktree %q: no base given", branch)
	}

	// Resolve the base to a commit now, so the recorded Base is what was actually used even if
	// the ref moves later.
	resolved, err := r.runner.mustRun(ctx, r.repoPath, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return Worktree{}, fmt.Errorf("create worktree %q: base %q does not resolve: %w", branch, base, err)
	}
	_ = strings.TrimSpace(resolved)

	dir := filepath.Join(r.worktreeRoot, worktreeDirName(branch))
	if _, err := r.runner.mustRun(ctx, r.repoPath, "worktree", "add", "-b", branch, dir, base); err != nil {
		return Worktree{}, fmt.Errorf("create worktree %q: %w", branch, err)
	}
	return Worktree{Path: dir, Branch: branch, Base: base}, nil
}

// worktreeDirName turns a branch name into a single directory name, since gravy/<id> contains a
// separator that would otherwise nest directories.
func worktreeDirName(branch string) string {
	return strings.ReplaceAll(branch, "/", "-")
}

// BranchName returns the branch for a ticket: gravy/<ticket-id>-<slug>.
func BranchName(ticketID, slug string) string {
	slug = slugify(slug)
	if slug == "" {
		return "gravy/" + ticketID
	}
	return "gravy/" + ticketID + "-" + slug
}

// slugify reduces a title to a branch-safe fragment.
func slugify(s string) string {
	var b strings.Builder
	lastDash := true // suppress a leading dash
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	const max = 40
	if len(out) > max {
		out = strings.Trim(out[:max], "-")
	}
	return out
}

// RemoveWorktree deletes the working directory and prunes git's metadata for it.
//
// Both halves matter: removing the directory alone leaves a stale administrative entry that
// makes git refuse to reuse the path.
func (r *LocalRepo) RemoveWorktree(ctx context.Context, w Worktree) error {
	res, err := r.runner.run(ctx, r.repoPath, "worktree", "remove", "--force", w.Path)
	if err != nil {
		return err
	}
	if res.code != 0 {
		// The directory may already be gone; prune still needs to run.
		if _, err := r.runner.mustRun(ctx, r.repoPath, "worktree", "prune"); err != nil {
			return fmt.Errorf("remove worktree %q: %w", w.Path, err)
		}
		return nil
	}
	if _, err := r.runner.mustRun(ctx, r.repoPath, "worktree", "prune"); err != nil {
		return fmt.Errorf("remove worktree %q: prune: %w", w.Path, err)
	}
	return nil
}

// ListWorktrees returns the worktree directories git currently knows about, excluding the main
// working copy. Startup cleanup uses this to find worktrees whose tickets are terminal.
func (r *LocalRepo) ListWorktrees(ctx context.Context) ([]Worktree, error) {
	out, err := r.runner.mustRun(ctx, r.repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}

	var (
		list    []Worktree
		current Worktree
		first   = true
	)
	flush := func() {
		if current.Path == "" {
			return
		}
		if first { // the first entry is the main working copy
			first = false
			current = Worktree{}
			return
		}
		list = append(list, current)
		current = Worktree{}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			flush()
			current.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			current.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	flush()
	return list, nil
}

// CommitAll stages everything in the worktree and commits it, returning the commit hash.
//
// It reports an empty hash and no error when there is nothing to commit, which is a normal
// outcome: an agent may finish a turn without changing anything.
func (r *LocalRepo) CommitAll(ctx context.Context, w Worktree, msg string) (string, error) {
	if _, err := r.runner.mustRun(ctx, w.Path, "add", "-A"); err != nil {
		return "", fmt.Errorf("commit in %q: %w", w.Path, err)
	}

	// --cached because everything is staged; exit 0 means no differences.
	res, err := r.runner.run(ctx, w.Path, "diff", "--cached", "--quiet")
	if err != nil {
		return "", err
	}
	if res.code == 0 {
		return "", nil
	}

	commit := append(r.identityArgs(ctx), "commit", "-m", msg)
	if _, err := r.runner.mustRun(ctx, w.Path, commit...); err != nil {
		return "", fmt.Errorf("commit in %q: %w", w.Path, err)
	}
	hash, err := r.runner.mustRun(ctx, w.Path, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(hash), nil
}

// Rebase replays the worktree's branch onto another ref.
//
// On conflict the rebase is aborted before returning, so the worktree is left usable: a human
// resolving a merge conflict must find a working directory, not a half-finished rebase. The
// conflicting paths are reported so they can be shown without the human going looking.
func (r *LocalRepo) Rebase(ctx context.Context, w Worktree, onto string) (RebaseResult, error) {
	before, err := r.runner.mustRun(ctx, w.Path, "rev-parse", "HEAD")
	if err != nil {
		return RebaseResult{}, err
	}

	rebaseArgs := append(r.identityArgs(ctx), "rebase", onto)
	res, err := r.runner.run(ctx, w.Path, rebaseArgs...)
	if err != nil {
		return RebaseResult{}, err
	}
	if res.code != 0 {
		conflicts, cerr := r.conflictFiles(ctx, w)
		if cerr != nil {
			conflicts = nil
		}
		// Abort so the worktree is recoverable, whatever else happens.
		if _, aerr := r.runner.run(ctx, w.Path, "rebase", "--abort"); aerr != nil {
			return RebaseResult{}, fmt.Errorf("rebase onto %q failed and could not be aborted: %w", onto, aerr)
		}

		// A rebase git declined to begin is not a conflict. Reporting it as one produced
		// "merge_conflict files=<nil>" on a branch whose target had never moved, and threw
		// away the one line that said what was actually wrong — usually unstaged changes left
		// in the worktree by whoever last ran the tests there.
		detail := strings.TrimSpace(res.stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.stdout)
		}
		return RebaseResult{
			Clean:         false,
			ConflictFiles: conflicts,
			Refused:       len(conflicts) == 0,
			Detail:        firstLine(detail),
		}, nil
	}

	after, err := r.runner.mustRun(ctx, w.Path, "rev-parse", "HEAD")
	if err != nil {
		return RebaseResult{}, err
	}
	// If HEAD did not move, the branch was already on top of the target and there is nothing
	// to re-validate. Green-against-yesterday is only stale evidence when the target moved.
	return RebaseResult{Clean: true, Replayed: strings.TrimSpace(before) != strings.TrimSpace(after)}, nil
}

// conflictFiles lists paths with unresolved conflicts during a rebase.
func (r *LocalRepo) conflictFiles(ctx context.Context, w Worktree) ([]string, error) {
	out, err := r.runner.mustRun(ctx, w.Path, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// Diff compares the worktree's branch against a base ref.
//
// Line counts come from --numstat, so they match `git diff --numstat` by construction rather
// than by a parser that reimplements counting.
func (r *LocalRepo) Diff(ctx context.Context, w Worktree, base string) (Diff, error) {
	spec := base + "..."

	numstat, err := r.runner.mustRun(ctx, w.Path, "diff", "--numstat", "-z", "--find-renames", spec)
	if err != nil {
		return Diff{}, err
	}
	status, err := r.runner.mustRun(ctx, w.Path, "diff", "--name-status", "-z", "--find-renames", spec)
	if err != nil {
		return Diff{}, err
	}
	statuses := map[string]string{}
	fields := strings.Split(status, "\x00")
	for i := 0; i+1 < len(fields); {
		code, path := fields[i], fields[i+1]
		i += 2
		if strings.HasPrefix(code, "R") || strings.HasPrefix(code, "C") {
			if i >= len(fields) {
				return Diff{}, fmt.Errorf("malformed git rename status")
			}
			path = fields[i]
			i++
		}
		statuses[path] = statusName(code)
	}

	var out Diff
	entries := strings.Split(numstat, "\x00")
	for i := 0; i < len(entries) && entries[i] != ""; i++ {
		counts := strings.SplitN(entries[i], "\t", 3)
		if len(counts) != 3 {
			return Diff{}, fmt.Errorf("malformed git numstat")
		}
		path := counts[2]
		paths := []string{path}
		// With -z, a rename has an empty path followed by separate old and new names.
		if path == "" {
			if i+2 >= len(entries) {
				return Diff{}, fmt.Errorf("malformed git rename numstat")
			}
			paths = []string{entries[i+1], entries[i+2]}
			path = entries[i+2]
			i += 2
		}
		f := FileDiff{Path: path, Status: statuses[path], Additions: atoiOrZero(counts[0]), Deletions: atoiOrZero(counts[1])}
		if f.Status == "" {
			// The two commands are run with the same flags and should agree, but a file with
			// no status renders as a gap in the review card, and "modified" is what a numstat
			// entry means when nothing says otherwise.
			f.Status = "modified"
		}
		args := append([]string{"--literal-pathspecs", "diff", "--find-renames", spec, "--"}, paths...)
		patch, err := r.runner.mustRun(ctx, w.Path, args...)
		if err != nil {
			return Diff{}, err
		}
		f.Patch = patch
		out.Files = append(out.Files, f)
	}
	return out, nil
}

func statusName(code string) string {
	if code == "" {
		return "modified"
	}
	switch code[0] {
	case 'A':
		return "added"
	case 'D':
		return "deleted"
	case 'R':
		return "renamed"
	case 'C':
		return "copied"
	default:
		return "modified"
	}
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// LandResult reports what landing did.
type LandResult struct {
	// MergeCommit is the squash commit created on the target branch.
	MergeCommit string
	// Pushed reports whether the target was pushed to the remote.
	Pushed bool
}

// DirtyCheckoutError reports that landing was refused because the repository's main checkout
// has uncommitted changes.
//
// It is a distinct type because it is a distinct situation: nothing conflicts, nothing is red,
// and the branch is fine. What is wrong sits in a directory the caller never mentioned — so the
// caller needs the path and the file list to say so, and must not file this as a conflict.
type DirtyCheckoutError struct {
	// Checkout is the main working copy that has to be cleaned.
	Checkout string
	// Files are the tracked paths with uncommitted changes.
	Files []string
}

func (e *DirtyCheckoutError) Error() string {
	return fmt.Sprintf("land: main checkout has uncommitted changes: %s", strings.Join(e.Files, ", "))
}

// SquashMerge squashes a worktree's branch into the target branch and pushes.
//
// The merge happens in the main working copy rather than the worktree, because a worktree has
// the ticket's own branch checked out and git refuses to check out a branch that is already
// checked out elsewhere.
func (r *LocalRepo) SquashMerge(ctx context.Context, w Worktree, target, message string, push bool) (LandResult, error) {
	var out LandResult

	// Git can carry unrelated staged changes into a squash commit. Refuse before
	// switching branches or moving the target, preserving the user's checkout.
	dirty, err := r.trackedChanges(ctx, r.repoPath)
	if err != nil {
		return out, err
	}
	if len(dirty) != 0 {
		return out, &DirtyCheckoutError{Checkout: r.repoPath, Files: dirty}
	}

	// The main copy must be on the target branch to merge into it. Its state is left as found:
	// Gravy never leaves a user's checkout somewhere they did not put it.
	original, err := r.currentBranch(ctx)
	if err != nil {
		return out, err
	}
	if original != target {
		if _, err := r.runner.mustRun(ctx, r.repoPath, "checkout", target); err != nil {
			return out, fmt.Errorf("land: check out %s: %w", target, err)
		}
		defer func() {
			// Best effort: a failure to restore is not worth losing a successful merge over,
			// but it must not be silent either.
			_, _ = r.runner.run(ctx, r.repoPath, "checkout", original)
		}()
	}

	// Fast-forward the target to the freshly-fetched remote state first, so the squash lands
	// on top of whatever else merged while this ticket was in review.
	if remote, err := r.TargetRef(ctx, target); err == nil && remote != target {
		if _, err := r.runner.mustRun(ctx, r.repoPath, "merge", "--ff-only", remote); err != nil {
			return out, fmt.Errorf("land: fast-forward %s to %s: %w", target, remote, err)
		}
	}

	if _, err := r.runner.mustRun(ctx, r.repoPath, "merge", "--squash", w.Branch); err != nil {
		return out, fmt.Errorf("land: squash %s into %s: %w", w.Branch, target, err)
	}
	squash := append(r.identityArgs(ctx), "commit", "-m", message)
	if _, err := r.runner.mustRun(ctx, r.repoPath, squash...); err != nil {
		return out, fmt.Errorf("land: commit squash of %s: %w", w.Branch, err)
	}

	hash, err := r.runner.mustRun(ctx, r.repoPath, "rev-parse", "HEAD")
	if err != nil {
		return out, err
	}
	out.MergeCommit = strings.TrimSpace(hash)

	// Push only when there is a remote to push to, and only when asked. A local-only project
	// is legitimate, and so is an approval that deliberately keeps the merge off the remote:
	// the target is ahead of origin afterwards, which the fast-forward above tolerates because
	// --ff-only on a branch that already contains the remote is a no-op.
	if !push {
		return out, nil
	}
	hasRemote, err := r.hasRemote(ctx)
	if err != nil {
		return out, err
	}
	if !hasRemote {
		return out, nil
	}
	if _, err := r.runner.mustRun(ctx, r.repoPath, "push", "origin", target); err != nil {
		return out, fmt.Errorf("land: push %s: %w", target, err)
	}
	out.Pushed = true
	return out, nil
}

// DeleteBranch removes a branch from the repository.
func (r *LocalRepo) DeleteBranch(ctx context.Context, branch string) error {
	if _, err := r.runner.mustRun(ctx, r.repoPath, "branch", "-D", branch); err != nil {
		return fmt.Errorf("delete branch %q: %w", branch, err)
	}
	return nil
}

// currentBranch returns the branch checked out in the main working copy.
func (r *LocalRepo) currentBranch(ctx context.Context) (string, error) {
	out, err := r.runner.mustRun(ctx, r.repoPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read current branch: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// hasRemote reports whether an origin remote is configured.
func (r *LocalRepo) hasRemote(ctx context.Context) (bool, error) {
	res, err := r.runner.run(ctx, r.repoPath, "remote", "get-url", "origin")
	if err != nil {
		return false, err
	}
	return res.code == 0, nil
}

// OpenWorktree returns the worktree checked out on a branch, if one exists.
//
// It exists because a ticket sent back for another attempt keeps its worktree: recreating it
// would fail on the existing branch, and removing it would throw away the work the reviewer
// asked to be built on.
//
// `git worktree list --porcelain` is the source of truth rather than the recorded path, since
// the path can be stale — deleted by hand, or left behind by a daemon that was killed.
func (r *LocalRepo) OpenWorktree(ctx context.Context, branch string) (Worktree, bool, error) {
	if branch == "" {
		return Worktree{}, false, fmt.Errorf("open worktree: no branch given")
	}

	out, err := r.runner.mustRun(ctx, r.repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return Worktree{}, false, fmt.Errorf("open worktree %q: %w", branch, err)
	}

	want := "refs/heads/" + branch
	var path string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		case strings.TrimSpace(line) == "":
			path = ""
		case strings.HasPrefix(line, "branch "):
			if strings.TrimSpace(strings.TrimPrefix(line, "branch ")) != want || path == "" {
				continue
			}
			// The entry can name a directory that is no longer there.
			if _, statErr := r.runner.mustRun(ctx, path, "rev-parse", "--is-inside-work-tree"); statErr != nil {
				return Worktree{}, false, nil
			}
			return Worktree{Path: path, Branch: branch}, true, nil
		}
	}
	return Worktree{}, false, nil
}

// ReviewCheckoutDirName is the directory a ticket's review checkout lives in, beside its
// worktree.
func ReviewCheckoutDirName(ticketID string) string { return "review-" + ticketID }

// ReviewCheckout creates a throwaway checkout that shows a ticket's work as uncommitted changes.
//
// Gravy commits what the agent produced, which makes the diff durable but leaves the worktree
// clean — and every editor's git integration (gutter marks, changed-file list, click-to-diff)
// reports uncommitted changes, so it has nothing to show. Reading the work in an editor
// therefore means reading files with no indication of what moved.
//
// The trick is to check out the commit and then reset the index back to its base: the files on
// disk are the new versions, git sees every one of them as modified, and the editor lights up
// exactly as it would for work you had just typed. It is a view, not a branch — nothing is
// committed here and nothing is read back from it.
func (r *LocalRepo) ReviewCheckout(ctx context.Context, ticketID, commit, base string) (Worktree, error) {
	if commit == "" || base == "" {
		return Worktree{}, fmt.Errorf("review checkout %s: need both a commit and its base", ticketID)
	}

	// Freeze both refs and use their merge base, so every ticket commit is visible.
	head, err := r.runner.mustRun(ctx, r.repoPath, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		return Worktree{}, err
	}
	commit = strings.TrimSpace(head)
	ancestor, err := r.runner.mustRun(ctx, r.repoPath, "merge-base", commit, base)
	if err != nil {
		return Worktree{}, err
	}
	base = strings.TrimSpace(ancestor)
	dir := filepath.Join(r.worktreeRoot, ReviewCheckoutDirName(ticketID))

	if _, err := os.Stat(dir); err == nil {
		// Mixed reset records the original commit in ORIG_HEAD and leaves HEAD at
		// the base. Reuse only a view of these exact revisions.
		previous, perr := r.runner.mustRun(ctx, dir, "rev-parse", "ORIG_HEAD")
		previousBase, berr := r.runner.mustRun(ctx, dir, "rev-parse", "HEAD")
		if perr == nil && berr == nil && strings.TrimSpace(previous) == commit && strings.TrimSpace(previousBase) == base {
			return Worktree{Path: dir, Base: base}, nil
		}
		if err := r.RemoveWorktree(ctx, Worktree{Path: dir}); err != nil {
			return Worktree{}, err
		}
	}

	// Detached: this checkout is a view of a commit, and giving it a branch would put a second
	// name on work that already has one.
	if _, err := r.runner.mustRun(ctx, r.repoPath, "worktree", "add", "--detach", dir, commit); err != nil {
		return Worktree{}, fmt.Errorf("review checkout %s: %w", ticketID, err)
	}

	// A mixed reset: the working tree keeps the new content, the index goes back to the base,
	// so every file the ticket touched reads as an unstaged modification.
	if _, err := r.runner.mustRun(ctx, dir, "reset", "--quiet", base); err != nil {
		// Leaving a half-made checkout behind would make the next attempt reuse it.
		_ = r.RemoveWorktree(ctx, Worktree{Path: dir})
		return Worktree{}, fmt.Errorf("review checkout %s: reset to base: %w", ticketID, err)
	}

	return Worktree{Path: dir, Base: base}, nil
}

// DiscardReviewCheckout removes a review checkout. Removing one that is not there is not an
// error: it is a view, and the point is that it can be thrown away at any time.
func (r *LocalRepo) DiscardReviewCheckout(ctx context.Context, ticketID string) error {
	dir := filepath.Join(r.worktreeRoot, ReviewCheckoutDirName(ticketID))
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	return r.RemoveWorktree(ctx, Worktree{Path: dir})
}

// DirtyFiles lists paths that are modified, staged or untracked in a worktree.
//
// Landing checks this before rebasing. Git refuses to rebase a dirty tree, and discovering that
// from the rebase means discovering it as a failure with a misleading name — the caller can say
// which files instead, which is the only thing the human needs to know.
func (r *LocalRepo) DirtyFiles(ctx context.Context, w Worktree) ([]string, error) {
	return r.statusFiles(ctx, w.Path, true)
}

// trackedChanges lists modified or staged tracked files, ignoring untracked ones.
//
// Landing's guard on the main checkout uses this rather than DirtyFiles. A squash-merge stages
// through that checkout's index, so a tracked edit sitting there can ride into the commit — but
// an untracked file cannot, and refusing to land because a build artefact or a crash dump is
// lying around would block every ticket over a file git is never going to touch. The rare case
// where an untracked file is genuinely in the merge's way is git's own to refuse, which it does
// before changing anything.
func (r *LocalRepo) trackedChanges(ctx context.Context, path string) ([]string, error) {
	return r.statusFiles(ctx, path, false)
}

func (r *LocalRepo) statusFiles(ctx context.Context, path string, untracked bool) ([]string, error) {
	args := []string{"status", "--porcelain"}
	if !untracked {
		args = append(args, "--untracked-files=no")
	}
	out, err := r.runner.mustRun(ctx, path, args...)
	if err != nil {
		return nil, fmt.Errorf("status in %q: %w", path, err)
	}
	var files []string
	// Split before trimming: the status is two columns and the first is often a space, so
	// trimming the whole output eats the leading space of the first line and takes the first
	// character of its path with it.
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 4 {
			continue
		}
		// "XY path", where XY is the two-character status.
		files = append(files, strings.TrimSpace(line[3:]))
	}
	return files, nil
}
