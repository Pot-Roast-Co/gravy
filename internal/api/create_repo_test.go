package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/host"
)

// gitIdentity gives commits an author where the machine running the tests has none configured.
func gitIdentity(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "T")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.test")
	}
}

// fakeGH puts a `gh` on PATH that records its arguments and exits with code.
func fakeGH(t *testing.T, code int) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "gh.log")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n"
	if code != 0 {
		script += "echo 'gh: not logged in' >&2\nexit " + string(rune('0'+code)) + "\n"
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// TestCreateRepositoryTakesAPlannedProjectToRunnable is the four manual steps gravy used to
// leave to the human — mkdir, git init, a first commit, set-repo — done in one.
func TestCreateRepositoryTakesAPlannedProjectToRunnable(t *testing.T) {
	gitIdentity(t)
	l, _, p := projectWithoutARepo(t)
	dir := filepath.Join(t.TempDir(), "jobApplicationHelper")

	saved, err := l.CreateRepository(context.Background(), p.ID, CreateRepoReq{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	if saved.RepoPath != dir || saved.TargetBranch != "main" {
		t.Fatalf("saved = path %q branch %q, want %q on main", saved.RepoPath, saved.TargetBranch, dir)
	}

	// A first commit, because worktrees need a branch to start from.
	h := host.NewLocal("local", 1)
	out, code, err := runGit(context.Background(), h, dir, "log", "--oneline")
	if err != nil || code != 0 || strings.Count(strings.TrimSpace(out), "\n") != 0 || out == "" {
		t.Fatalf("want exactly one commit, got %q (%d, %v)", out, code, err)
	}
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil || !strings.HasPrefix(string(readme), "# fantasyHockeyAid\n") {
		t.Errorf("README = %q, %v", readme, err)
	}
}

// An existing empty directory is fine; one with anything in it is never initialised over.
func TestCreateRepositoryNeverInitialisesOverFiles(t *testing.T) {
	gitIdentity(t)

	l, _, p := projectWithoutARepo(t)
	empty := t.TempDir()
	if _, err := l.CreateRepository(context.Background(), p.ID, CreateRepoReq{Path: empty}); err != nil {
		t.Fatalf("an empty directory was refused: %v", err)
	}

	l, _, p = projectWithoutARepo(t)
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "notes.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.CreateRepository(context.Background(), p.ID, CreateRepoReq{Path: full}); err == nil ||
		!strings.Contains(err.Error(), "not empty") {
		t.Errorf("a directory with files in it: err = %v", err)
	}

	// An existing repository is set-repo's job, and the error says so.
	l, _, p = projectWithoutARepo(t)
	repo := gitRepo(t, "main")
	if _, err := l.CreateRepository(context.Background(), p.ID, CreateRepoReq{Path: repo}); err == nil ||
		!strings.Contains(err.Error(), "set-repo") {
		t.Errorf("an existing repository: err = %v", err)
	}
}

func TestCreateRepositoryRefusesAProjectThatHasOne(t *testing.T) {
	gitIdentity(t)
	l, _, p := projectWithoutARepo(t)
	if _, err := l.CreateRepository(context.Background(), p.ID,
		CreateRepoReq{Path: filepath.Join(t.TempDir(), "a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.CreateRepository(context.Background(), p.ID,
		CreateRepoReq{Path: filepath.Join(t.TempDir(), "b")}); err == nil ||
		!strings.Contains(err.Error(), "already has a repository") {
		t.Errorf("err = %v", err)
	}
}

func TestCreateRepositoryPushesToGitHubWhenAsked(t *testing.T) {
	gitIdentity(t)
	log := fakeGH(t, 0)
	l, _, p := projectWithoutARepo(t)
	dir := filepath.Join(t.TempDir(), "jobApplicationHelper")

	if _, err := l.CreateRepository(context.Background(), p.ID, CreateRepoReq{Path: dir, GitHub: true}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(log)
	for _, want := range []string{"repo create jobApplicationHelper", "--private", "--source " + dir, "--push"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("gh called with %q, want %q in it", got, want)
		}
	}
}

// A GitHub failure leaves the local repository and says how to finish by hand, instead of
// attaching a project whose remote does not exist.
func TestCreateRepositoryReportsAGitHubFailure(t *testing.T) {
	gitIdentity(t)
	fakeGH(t, 1)
	l, _, p := projectWithoutARepo(t)
	dir := filepath.Join(t.TempDir(), "x")

	_, err := l.CreateRepository(context.Background(), p.ID, CreateRepoReq{Path: dir, GitHub: true})
	if err == nil || !strings.Contains(err.Error(), "not logged in") || !strings.Contains(err.Error(), "set-repo") {
		t.Fatalf("err = %v", err)
	}
	if !host.NewLocal("local", 1).FS().Exists(filepath.Join(dir, ".git")) {
		t.Error("the local repository was not left in place")
	}
	projects, _ := l.ListProjects(context.Background(), ProjectFilter{})
	for _, got := range projects {
		if got.ID == p.ID && got.RepoPath != "" {
			t.Errorf("a project whose GitHub step failed was attached anyway: %q", got.RepoPath)
		}
	}
}

func TestCreateRepositoryPublicNeedsGitHub(t *testing.T) {
	l, _, p := projectWithoutARepo(t)
	if _, err := l.CreateRepository(context.Background(), p.ID,
		CreateRepoReq{Path: filepath.Join(t.TempDir(), "x"), Public: true}); err == nil {
		t.Error("--public without --github was accepted")
	}
}
