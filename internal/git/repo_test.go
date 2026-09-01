package git

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bobbybrady/gravy/internal/host"
)

// testRepo builds a real git repository with one commit on main, plus the worktree root Gravy
// would use. Integration against real git is the point: these operations are exactly where a
// mocked git would prove nothing.
func testRepo(t *testing.T) (*LocalRepo, string) {
	t.Helper()
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	worktreeRoot := filepath.Join(root, "worktrees")

	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, repoPath)
	writeFile(t, repoPath, "README.md", "# test\n")
	writeFile(t, repoPath, "main.go", "package main\n\nfunc main() {}\n")
	git(t, repoPath, "add", "-A")
	git(t, repoPath, "commit", "-m", "initial")

	h := host.NewLocal("test", 4)
	return NewLocalRepo(h, repoPath, worktreeRoot), repoPath
}

// fixtureHost runs the git commands that arrange and inspect test fixtures.
//
// These go through Host.Exec like everything else rather than reaching for os/exec directly.
// That is not ceremony: GR-009's fifth acceptance criterion is that every git call in this
// package goes through the Host, asserted by the layering check — and an exemption for test
// files would make that assertion decorative. It also means the fixtures exercise LocalHost.
var fixtureHost = host.NewLocal("fixture", 8)

// git runs a git command for arranging or inspecting fixtures, returning its combined output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	p, err := fixtureHost.Exec(context.Background(), host.ExecSpec{
		Cmd:     "git",
		Args:    args,
		Dir:     dir,
		Timeout: 30 * time.Second,
		Env: map[string]string{
			"GIT_AUTHOR_NAME":     "test",
			"GIT_AUTHOR_EMAIL":    "test@example.com",
			"GIT_COMMITTER_NAME":  "test",
			"GIT_COMMITTER_EMAIL": "test@example.com",
			"GIT_TERMINAL_PROMPT": "0",
		},
	})
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	// Drain both streams concurrently, or a command that fills one pipe deadlocks.
	outCh, errCh := readPipe(p.Stdout()), readPipe(p.Stderr())
	st, waitErr := p.Wait()
	out, errOut := <-outCh, <-errCh

	if waitErr != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), waitErr)
	}
	if st.Code != 0 {
		t.Fatalf("git %s: exit %d\n%s%s", strings.Join(args, " "), st.Code, out, errOut)
	}
	return out + errOut
}

func readPipe(r io.Reader) <-chan string {
	ch := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		ch <- string(b)
	}()
	return ch
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.name", "test")
	git(t, dir, "config", "user.email", "test@example.com")
}

func writeFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTwoWorktreesAreIndependent is AC1.
func TestTwoWorktreesAreIndependent(t *testing.T) {
	repo, _ := testRepo(t)
	ctx := context.Background()

	w1, err := repo.CreateWorktree(ctx, "gravy/gr-001-first", "main")
	if err != nil {
		t.Fatalf("first worktree: %v", err)
	}
	w2, err := repo.CreateWorktree(ctx, "gravy/gr-002-second", "main")
	if err != nil {
		t.Fatalf("second worktree: %v", err)
	}

	if w1.Path == w2.Path {
		t.Fatal("both tickets got the same directory")
	}
	for _, w := range []Worktree{w1, w2} {
		if _, err := os.Stat(filepath.Join(w.Path, "README.md")); err != nil {
			t.Errorf("worktree %s was not checked out: %v", w.Branch, err)
		}
	}

	// Editing one must not affect the other. This is the isolation the whole design rests on.
	writeFile(t, w1.Path, "main.go", "package main\n\nfunc main() { println(\"one\") }\n")
	b, err := os.ReadFile(filepath.Join(w2.Path, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "one") {
		t.Error("a change in one worktree was visible in the other")
	}

	// Both branched from target.
	for _, w := range []Worktree{w1, w2} {
		base := strings.TrimSpace(git(t, w.Path, "merge-base", w.Branch, "main"))
		mainHead := strings.TrimSpace(git(t, w.Path, "rev-parse", "main"))
		if base != mainHead {
			t.Errorf("%s is not branched from main", w.Branch)
		}
	}
}

// TestCreateWorktreeHonoursBase is AC6: base is never silently replaced by the target branch.
//
// Without this, stacked tickets would require rewriting this package rather than changing the
// scheduler.
func TestCreateWorktreeHonoursBase(t *testing.T) {
	repo, repoPath := testRepo(t)
	ctx := context.Background()

	// First ticket adds a file and commits, as a finished ticket would.
	w1, err := repo.CreateWorktree(ctx, "gravy/gr-001-first", "main")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, w1.Path, "from-first.txt", "written by ticket one\n")
	hash, err := repo.CommitAll(ctx, w1, "gravy: ticket one")
	if err != nil {
		t.Fatalf("CommitAll: %v", err)
	}
	if hash == "" {
		t.Fatal("CommitAll reported nothing to commit")
	}

	// Second ticket branches from the FIRST TICKET'S BRANCH, not from main.
	w2, err := repo.CreateWorktree(ctx, "gravy/gr-002-second", "gravy/gr-001-first")
	if err != nil {
		t.Fatalf("stacked worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w2.Path, "from-first.txt")); err != nil {
		t.Error("the stacked worktree does not contain the first ticket's work; base was ignored")
	}
	if w2.Base != "gravy/gr-001-first" {
		t.Errorf("recorded base = %q, want the branch that was asked for", w2.Base)
	}

	// And a worktree from main must NOT see it, proving the two really differ.
	w3, err := repo.CreateWorktree(ctx, "gravy/gr-003-third", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w3.Path, "from-first.txt")); err == nil {
		t.Error("a worktree based on main contains the first ticket's work")
	}
	_ = repoPath
}

func TestCreateWorktreeRejectsBadInput(t *testing.T) {
	repo, _ := testRepo(t)
	ctx := context.Background()

	if _, err := repo.CreateWorktree(ctx, "", "main"); err == nil {
		t.Error("accepted an empty branch")
	}
	if _, err := repo.CreateWorktree(ctx, "gravy/x", ""); err == nil {
		t.Error("accepted an empty base")
	}
	if _, err := repo.CreateWorktree(ctx, "gravy/x", "no-such-ref"); err == nil {
		t.Error("accepted a base that does not resolve")
	}
}

// TestRebaseConflict is AC2: conflicts are reported with their paths, and the worktree is left
// recoverable rather than mid-rebase.
func TestRebaseConflict(t *testing.T) {
	repo, repoPath := testRepo(t)
	ctx := context.Background()

	w, err := repo.CreateWorktree(ctx, "gravy/gr-001-conflict", "main")
	if err != nil {
		t.Fatal(err)
	}
	// The ticket edits main.go.
	writeFile(t, w.Path, "main.go", "package main\n\nfunc main() { println(\"ticket\") }\n")
	if _, err := repo.CommitAll(ctx, w, "gravy: ticket edit"); err != nil {
		t.Fatal(err)
	}

	// Meanwhile main moves, touching the same lines.
	writeFile(t, repoPath, "main.go", "package main\n\nfunc main() { println(\"target moved\") }\n")
	git(t, repoPath, "add", "-A")
	git(t, repoPath, "commit", "-m", "target moves")

	res, err := repo.Rebase(ctx, w, "main")
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if res.Clean {
		t.Fatal("Rebase reported clean on a conflicting rebase")
	}
	if len(res.ConflictFiles) == 0 {
		t.Error("no conflicting paths were reported")
	} else if res.ConflictFiles[0] != "main.go" {
		t.Errorf("conflicting paths = %v, want [main.go]", res.ConflictFiles)
	}

	// The worktree must be usable: the rebase is aborted, not left half-applied. A human is
	// about to resolve this by hand.
	status := git(t, w.Path, "status", "--porcelain=v2", "--branch")
	if strings.Contains(status, "rebase") {
		t.Errorf("worktree is still mid-rebase:\n%s", status)
	}
	if out := strings.TrimSpace(git(t, w.Path, "status", "--porcelain")); out != "" {
		t.Errorf("worktree is dirty after the aborted rebase:\n%s", out)
	}
	// The ticket's own commit must still be there.
	log := git(t, w.Path, "log", "--oneline", "-1")
	if !strings.Contains(log, "ticket edit") {
		t.Errorf("the ticket's commit was lost: %s", log)
	}
}

// TestRebaseCleanDistinguishesReplayFromNoop covers the conditional re-validation rule: if the
// branch was already on top of target there is nothing to re-validate, and Gravy should not
// pretend otherwise.
func TestRebaseCleanDistinguishesReplayFromNoop(t *testing.T) {
	repo, repoPath := testRepo(t)
	ctx := context.Background()

	w, err := repo.CreateWorktree(ctx, "gravy/gr-001-clean", "main")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, w.Path, "new.txt", "ticket work\n")
	if _, err := repo.CommitAll(ctx, w, "gravy: ticket work"); err != nil {
		t.Fatal(err)
	}

	// Target has not moved: rebasing must be a clean no-op.
	res, err := repo.Rebase(ctx, w, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Clean {
		t.Fatal("rebase onto an unmoved target was not clean")
	}
	if res.Replayed {
		t.Error("Replayed is true although the target had not moved")
	}

	// Now move target in a non-conflicting way; the rebase must replay.
	writeFile(t, repoPath, "other.txt", "unrelated change\n")
	git(t, repoPath, "add", "-A")
	git(t, repoPath, "commit", "-m", "unrelated")

	res, err = repo.Rebase(ctx, w, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Clean {
		t.Fatalf("non-conflicting rebase was not clean: %+v", res)
	}
	if !res.Replayed {
		t.Error("Replayed is false although the target moved and commits were replayed")
	}
	if _, err := os.Stat(filepath.Join(w.Path, "other.txt")); err != nil {
		t.Error("the worktree did not pick up the target's new commit")
	}
}

// TestRemoveWorktree is AC3.
func TestRemoveWorktree(t *testing.T) {
	repo, repoPath := testRepo(t)
	ctx := context.Background()

	w, err := repo.CreateWorktree(ctx, "gravy/gr-001-remove", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Path); err != nil {
		t.Fatal("worktree was not created")
	}

	if err := repo.RemoveWorktree(ctx, w); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Error("the worktree directory still exists")
	}
	// git's metadata must be pruned too, or the path cannot be reused.
	if out := git(t, repoPath, "worktree", "list", "--porcelain"); strings.Contains(out, w.Path) {
		t.Errorf("git still lists the removed worktree:\n%s", out)
	}
	// The path must be reusable, which is the practical consequence of pruning.
	if _, err := repo.CreateWorktree(ctx, "gravy/gr-001-remove-again", "main"); err != nil {
		t.Errorf("could not create a worktree after removing one: %v", err)
	}
}

func TestRemoveWorktreeWhenDirectoryIsAlreadyGone(t *testing.T) {
	repo, _ := testRepo(t)
	ctx := context.Background()

	w, err := repo.CreateWorktree(ctx, "gravy/gr-001-gone", "main")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash between deleting the directory and pruning.
	if err := os.RemoveAll(w.Path); err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveWorktree(ctx, w); err != nil {
		t.Errorf("RemoveWorktree on an already-deleted directory: %v", err)
	}
}

func TestListWorktrees(t *testing.T) {
	repo, _ := testRepo(t)
	ctx := context.Background()

	got, err := repo.ListWorktrees(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a fresh repository reported %d worktrees, want 0 (the main copy is excluded)", len(got))
	}

	w1, _ := repo.CreateWorktree(ctx, "gravy/gr-001-a", "main")
	if _, err := repo.CreateWorktree(ctx, "gravy/gr-002-b", "main"); err != nil {
		t.Fatal(err)
	}

	got, err = repo.ListWorktrees(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d worktrees, want 2: %+v", len(got), got)
	}
	branches := map[string]bool{}
	for _, w := range got {
		branches[w.Branch] = true
	}
	if !branches["gravy/gr-001-a"] || !branches["gravy/gr-002-b"] {
		t.Errorf("branches = %v", branches)
	}

	if err := repo.RemoveWorktree(ctx, w1); err != nil {
		t.Fatal(err)
	}
	got, _ = repo.ListWorktrees(ctx)
	if len(got) != 1 {
		t.Errorf("after removing one, got %d worktrees, want 1", len(got))
	}
}

// TestDiffMatchesNumstat is AC4: line counts must match git's own exactly.
func TestDiffMatchesNumstat(t *testing.T) {
	repo, _ := testRepo(t)
	ctx := context.Background()

	w, err := repo.CreateWorktree(ctx, "gravy/gr-001-diff", "main")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, w.Path, "main.go", "package main\n\nfunc main() {\n\tprintln(\"one\")\n\tprintln(\"two\")\n}\n")
	writeFile(t, w.Path, "added.txt", "line one\nline two\nline three\n")
	if err := os.Remove(filepath.Join(w.Path, "README.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CommitAll(ctx, w, "gravy: changes"); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Diff(ctx, w, "main")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	// Compare against git's own numstat, field by field.
	want := map[string][2]int{}
	raw := git(t, w.Path, "diff", "--numstat", "main...")
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		want[f[2]] = [2]int{atoiOrZero(f[0]), atoiOrZero(f[1])}
	}
	if len(want) == 0 {
		t.Fatal("the fixture produced no diff")
	}
	if len(got.Files) != len(want) {
		t.Fatalf("Diff reported %d files, git reports %d", len(got.Files), len(want))
	}
	for _, f := range got.Files {
		w, ok := want[f.Path]
		if !ok {
			t.Errorf("Diff reported %q, which git does not", f.Path)
			continue
		}
		if f.Additions != w[0] || f.Deletions != w[1] {
			t.Errorf("%s: got +%d-%d, git says +%d-%d", f.Path, f.Additions, f.Deletions, w[0], w[1])
		}
	}

	statuses := map[string]string{}
	for _, f := range got.Files {
		statuses[f.Path] = f.Status
		if f.Patch == "" {
			t.Errorf("%s has no patch", f.Path)
		}
	}
	if statuses["added.txt"] != "added" {
		t.Errorf("added.txt status = %q, want added", statuses["added.txt"])
	}
	if statuses["README.md"] != "deleted" {
		t.Errorf("README.md status = %q, want deleted", statuses["README.md"])
	}
	if statuses["main.go"] != "modified" {
		t.Errorf("main.go status = %q, want modified", statuses["main.go"])
	}

	adds, dels := got.Totals()
	if adds == 0 || dels == 0 {
		t.Errorf("Totals = +%d-%d, want both non-zero", adds, dels)
	}
}

// TestDiffIsAgainstMergeBase: a diff must show the ticket's work, not the target's unrelated
// progress, or every review would include changes the agent never made.
func TestDiffIsAgainstMergeBase(t *testing.T) {
	repo, repoPath := testRepo(t)
	ctx := context.Background()

	w, err := repo.CreateWorktree(ctx, "gravy/gr-001-base", "main")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, w.Path, "ticket.txt", "ticket work\n")
	if _, err := repo.CommitAll(ctx, w, "gravy: ticket work"); err != nil {
		t.Fatal(err)
	}

	// Target moves independently after the worktree was cut.
	writeFile(t, repoPath, "unrelated.txt", "not the agent's doing\n")
	git(t, repoPath, "add", "-A")
	git(t, repoPath, "commit", "-m", "unrelated target work")

	got, err := repo.Diff(ctx, w, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got.Files {
		if f.Path == "unrelated.txt" {
			t.Error("the diff includes the target's unrelated work")
		}
	}
	if len(got.Files) != 1 || got.Files[0].Path != "ticket.txt" {
		t.Errorf("diff = %+v, want only ticket.txt", got.Files)
	}
}

func TestCommitAllWithNothingToCommit(t *testing.T) {
	repo, _ := testRepo(t)
	ctx := context.Background()

	w, err := repo.CreateWorktree(ctx, "gravy/gr-001-empty", "main")
	if err != nil {
		t.Fatal(err)
	}
	// An agent may finish a turn having changed nothing. That is a normal outcome, not an error.
	hash, err := repo.CommitAll(ctx, w, "gravy: nothing")
	if err != nil {
		t.Fatalf("CommitAll with no changes: %v", err)
	}
	if hash != "" {
		t.Errorf("CommitAll returned hash %q with nothing to commit", hash)
	}
}

func TestCommitAllIncludesUntrackedAndDeleted(t *testing.T) {
	repo, _ := testRepo(t)
	ctx := context.Background()

	w, err := repo.CreateWorktree(ctx, "gravy/gr-001-all", "main")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, w.Path, "nested/new.txt", "brand new\n")
	if err := os.Remove(filepath.Join(w.Path, "README.md")); err != nil {
		t.Fatal(err)
	}

	hash, err := repo.CommitAll(ctx, w, "gravy: everything")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		t.Fatal("CommitAll reported nothing to commit")
	}
	if out := strings.TrimSpace(git(t, w.Path, "status", "--porcelain")); out != "" {
		t.Errorf("worktree still dirty after CommitAll:\n%s", out)
	}
	files := git(t, w.Path, "show", "--name-status", "--format=", hash)
	if !strings.Contains(files, "nested/new.txt") {
		t.Error("an untracked file was not committed")
	}
	if !strings.Contains(files, "README.md") {
		t.Error("a deletion was not committed")
	}
}

func TestFetchOnRepoWithoutRemote(t *testing.T) {
	repo, _ := testRepo(t)
	// A local-only project is legitimate; fetching must not fail it.
	if err := repo.Fetch(context.Background()); err != nil {
		t.Errorf("Fetch on a repository with no remote: %v", err)
	}
}

func TestFetchFromRemote(t *testing.T) {
	repo, repoPath := testRepo(t)
	ctx := context.Background()

	// Build an upstream, point the repo at it, and land a commit there.
	upstream := filepath.Join(t.TempDir(), "upstream")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, upstream)
	writeFile(t, upstream, "upstream.txt", "from upstream\n")
	git(t, upstream, "add", "-A")
	git(t, upstream, "commit", "-m", "upstream commit")

	git(t, repoPath, "remote", "add", "origin", upstream)
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if out := git(t, repoPath, "rev-parse", "--verify", "origin/main"); strings.TrimSpace(out) == "" {
		t.Error("origin/main was not fetched")
	}
}

func TestBranchName(t *testing.T) {
	tests := []struct {
		id, title, want string
	}{
		{"GR-014", "Validation runner", "gravy/GR-014-validation-runner"},
		{"GR-001", "Repo scaffold & CI", "gravy/GR-001-repo-scaffold-ci"},
		{"GR-002", "  Spaces  everywhere  ", "gravy/GR-002-spaces-everywhere"},
		{"GR-003", "", "gravy/GR-003"},
		{"GR-004", "!!!", "gravy/GR-004"},
		{"GR-005", "Add support for \"quoted\" things", "gravy/GR-005-add-support-for-quoted-things"},
	}
	for _, tt := range tests {
		if got := BranchName(tt.id, tt.title); got != tt.want {
			t.Errorf("BranchName(%q, %q) = %q, want %q", tt.id, tt.title, got, tt.want)
		}
	}
	// Long titles are truncated without leaving a trailing dash, which git would reject.
	long := BranchName("GR-006", strings.Repeat("very long title ", 10))
	if strings.HasSuffix(long, "-") {
		t.Errorf("truncated branch name ends with a dash: %q", long)
	}
	if len(long) > 60 {
		t.Errorf("branch name is %d chars, too long: %q", len(long), long)
	}
}

// TestBranchNamesAreValidGitRefs runs the generated names past git itself.
func TestBranchNamesAreValidGitRefs(t *testing.T) {
	repo, repoPath := testRepo(t)
	ctx := context.Background()

	for i, title := range []string{
		"Validation runner", "Repo scaffold & CI", "!!!", "  spaces  ",
		strings.Repeat("long ", 30), "unicode — em dash", "slashes/in/title",
	} {
		branch := BranchName("GR-"+string(rune('a'+i)), title)
		if _, err := repo.CreateWorktree(ctx, branch, "main"); err != nil {
			t.Errorf("git rejected branch %q (from title %q): %v", branch, title, err)
			continue
		}
		if out := git(t, repoPath, "rev-parse", "--verify", branch); strings.TrimSpace(out) == "" {
			t.Errorf("branch %q was not created", branch)
		}
	}
}

// TestWorktreesLiveOutsideTheRepository: Gravy writes nothing into a repository, ever.
func TestWorktreesLiveOutsideTheRepository(t *testing.T) {
	repo, repoPath := testRepo(t)
	ctx := context.Background()

	before := strings.TrimSpace(git(t, repoPath, "status", "--porcelain"))

	w, err := repo.CreateWorktree(ctx, "gravy/gr-001-outside", "main")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(w.Path, repoPath+string(filepath.Separator)) {
		t.Errorf("worktree %q is inside the repository %q", w.Path, repoPath)
	}

	after := strings.TrimSpace(git(t, repoPath, "status", "--porcelain"))
	if after != before {
		t.Errorf("creating a worktree dirtied the repository:\n%s", after)
	}
}
