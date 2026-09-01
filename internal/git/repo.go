package git

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bobbybrady/gravy/internal/host"
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

	if _, err := r.runner.mustRun(ctx, w.Path, "commit", "-m", msg); err != nil {
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

	res, err := r.runner.run(ctx, w.Path, "rebase", onto)
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
		return RebaseResult{Clean: false, ConflictFiles: conflicts}, nil
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

	numstat, err := r.runner.mustRun(ctx, w.Path, "diff", "--numstat", spec)
	if err != nil {
		return Diff{}, err
	}
	status, err := r.runner.mustRun(ctx, w.Path, "diff", "--name-status", spec)
	if err != nil {
		return Diff{}, err
	}

	statuses := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(status), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 2 {
			continue
		}
		// A rename is "R100\told\tnew"; the new path is what the diff is about.
		path := fields[len(fields)-1]
		statuses[path] = statusName(fields[0])
	}

	var out Diff
	for _, line := range strings.Split(strings.TrimSpace(numstat), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 3 {
			continue
		}
		path := fields[len(fields)-1]
		f := FileDiff{Path: path, Status: statuses[path]}
		if f.Status == "" {
			f.Status = "modified"
		}
		// A binary file reports "-" rather than a count.
		f.Additions = atoiOrZero(fields[0])
		f.Deletions = atoiOrZero(fields[1])

		patch, err := r.runner.mustRun(ctx, w.Path, "diff", spec, "--", path)
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
