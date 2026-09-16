package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pot-roast-co/gravy/internal/host"
)

// git runs a command in dir and fails the test if it does not succeed.
func mustGit(t *testing.T, h host.Host, dir string, args ...string) string {
	t.Helper()
	out, code, err := runGit(context.Background(), h, dir, args...)
	if err != nil || code != 0 {
		t.Fatalf("git %v in %s: code=%d err=%v out=%s", args, dir, code, err, out)
	}
	return out
}

// repoWithRemote builds a repository whose origin is a bare repo with the given default branch,
// pushed the way `git init` + `git remote add` + `git push` does it — which, unlike a clone,
// leaves no local refs/remotes/origin/HEAD behind.
func repoWithRemote(t *testing.T, defaultBranch string) (host.Host, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	h := host.NewLocal("local", 1)

	if err := os.MkdirAll(remote, 0700); err != nil {
		t.Fatal(err)
	}
	mustGit(t, h, remote, "init", "--bare", "-b", defaultBranch)

	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, h, repo, "init", "-b", defaultBranch)
	mustGit(t, h, repo, "add", ".")
	mustGit(t, h, repo, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "initial")
	mustGit(t, h, repo, "remote", "add", "origin", remote)
	mustGit(t, h, repo, "push", "-u", "origin", defaultBranch)
	return h, repo
}

// TestResolveTargetBranchAsksTheRemote is the regression this whole file exists for.
//
// A project registered while a feature branch was checked out, in a repository that was pushed
// rather than cloned, used to take the feature branch as its target — and every approval after
// that squash-merged onto it instead of main.
func TestResolveTargetBranchAsksTheRemote(t *testing.T) {
	h, repo := repoWithRemote(t, "main")

	// No local origin/HEAD: this is what `git init` + push leaves behind.
	if _, code, _ := runGit(context.Background(), h, repo, "symbolic-ref", "refs/remotes/origin/HEAD"); code == 0 {
		t.Fatal("fixture has a local origin/HEAD; it would not exercise the remote lookup")
	}

	mustGit(t, h, repo, "checkout", "-b", "feat/planning-projects-and-remote-hosts")

	got, err := resolveTargetBranch(context.Background(), h, repo)
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Fatalf("target = %q, want main (the remote's default, not the checked-out branch)", got)
	}
}

func TestResolveTargetBranchPrefersLocalOriginHead(t *testing.T) {
	h, repo := repoWithRemote(t, "trunk")
	mustGit(t, h, repo, "remote", "set-head", "origin", "trunk")
	mustGit(t, h, repo, "checkout", "-b", "some-feature")

	got, err := resolveTargetBranch(context.Background(), h, repo)
	if err != nil {
		t.Fatal(err)
	}
	if got != "trunk" {
		t.Fatalf("target = %q, want trunk", got)
	}
}

// A repository with no origin is a legitimate local-only project, and still falls back to the
// branch that is checked out.
func TestResolveTargetBranchFallsBackWithoutOrigin(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	h := host.NewLocal("local", 1)
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, h, repo, "init", "-b", "scratch")
	mustGit(t, h, repo, "add", ".")
	mustGit(t, h, repo, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "initial")

	got, err := resolveTargetBranch(context.Background(), h, repo)
	if err != nil {
		t.Fatal(err)
	}
	if got != "scratch" {
		t.Fatalf("target = %q, want scratch", got)
	}
}

// An origin that cannot be read is an error, not a guess at the current branch. Guessing is what
// put approved work on the wrong branch in the first place.
func TestResolveTargetBranchRefusesToGuessWhenOriginIsUnreadable(t *testing.T) {
	h, repo := repoWithRemote(t, "main")
	mustGit(t, h, repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	mustGit(t, h, repo, "checkout", "-b", "feature")

	if _, err := resolveTargetBranch(context.Background(), h, repo); err == nil {
		t.Fatal("want an error asking for --target-branch, got a silent fallback")
	}
}

func TestSymrefHead(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ref: refs/heads/main\tHEAD\nabc123\tHEAD\n", "main"},
		{"ref: refs/heads/feat/a-b\tHEAD\n", "feat/a-b"},
		{"abc123\tHEAD\n", ""},
		{"", ""},
	} {
		if got := symrefHead(tc.in); got != tc.want {
			t.Fatalf("symrefHead(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
