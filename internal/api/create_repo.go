package api

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
)

// CreateRepoReq says where and how to create a project's repository.
type CreateRepoReq struct {
	// Path is the directory to create. It must not exist, or be empty.
	Path string `json:"path"`
	// GitHub also creates a GitHub repository with `gh` and pushes the first commit to it.
	GitHub bool `json:"github,omitempty"`
	// Public makes that GitHub repository public. Private otherwise; only meaningful with GitHub.
	Public bool `json:"public,omitempty"`
}

// CreateRepository makes a new repository for a project that has none, then attaches it.
//
// Before this, a planned project's tickets sat in Ready behind "no repository yet" until a human
// made a directory, ran git init, made a first commit and came back to set-repo — four steps
// that are always the same. The first commit matters: a repository with no commits has no
// branch for worktrees to start from, so the first ticket would fail on the one thing every
// ticket needs.
//
// Everything runs on the project's own host, as its tickets will. Attaching goes through
// AttachRepository, so the target branch and allowlist are settled exactly as they are for a
// repository a human made.
func (l *Local) CreateRepository(ctx context.Context, projectID string, req CreateRepoReq) (core.Project, error) {
	p, err := l.db.GetProject(ctx, projectID)
	if err != nil {
		return core.Project{}, err
	}
	if existing := strings.TrimSpace(p.RepoPath); existing != "" {
		return core.Project{}, fmt.Errorf("%s already has a repository at %s", p.Slug, existing)
	}
	if req.Public && !req.GitHub {
		return core.Project{}, fmt.Errorf("--public only applies with --github")
	}

	h, err := l.hostFor(p.HostID)
	if err != nil {
		return core.Project{}, err
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		return core.Project{}, fmt.Errorf("a new repository needs a path")
	}
	local := p.HostID == "" || p.HostID == h.ID()
	if local {
		if path, err = filepath.Abs(path); err != nil {
			return core.Project{}, fmt.Errorf("resolve %q: %w", path, err)
		}
	} else if !filepath.IsAbs(path) {
		return core.Project{}, fmt.Errorf("a path on %s must be absolute: %q", p.HostID, path)
	}

	// Never initialise over somebody's files: a directory with anything in it is either a
	// repository already (set-repo is the command for that) or something that is not ours.
	if h.FS().Exists(path) {
		if _, code, err := runGit(ctx, h, path, "rev-parse", "--git-dir"); err == nil && code == 0 {
			return core.Project{}, fmt.Errorf(
				"%s is already a git repository; use `gravy project set-repo %s %s`", path, p.Slug, path)
		}
		out, _, code, err := runOn(ctx, h, path, "ls", "-A")
		if err != nil || code != 0 {
			return core.Project{}, fmt.Errorf("%s: could not check the directory is empty", path)
		}
		if strings.TrimSpace(out) != "" {
			return core.Project{}, fmt.Errorf("%s already exists and is not empty", path)
		}
	} else if err := h.FS().MkdirAll(path, 0o755); err != nil {
		return core.Project{}, fmt.Errorf("create %s: %w", path, err)
	}

	if err := h.FS().WriteFile(filepath.Join(path, "README.md"), []byte(readme(p)), 0o644); err != nil {
		return core.Project{}, fmt.Errorf("write README: %w", err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"add", "README.md"},
		{"commit", "-m", "Initial commit"},
	} {
		if _, errOut, code, err := runOn(ctx, h, path, "git", args...); err != nil || code != 0 {
			return core.Project{}, fmt.Errorf("git %s in %s failed: %s",
				args[0], path, firstNonEmpty(errOut, errString(err)))
		}
	}

	if req.GitHub {
		visibility := "--private"
		if req.Public {
			visibility = "--public"
		}
		// The directory's name is the repository's: it is what the human chose, and what
		// they will look for on GitHub.
		if _, errOut, code, err := runOn(ctx, h, path, "gh", "repo", "create", filepath.Base(path),
			visibility, "--source", path, "--remote", "origin", "--push"); err != nil || code != 0 {
			// The local repository stands; say so, so the retry is `gh`, not this command.
			return core.Project{}, fmt.Errorf(
				"created %s locally, but GitHub failed: %s (the project is not attached; fix gh and run "+
					"`gh repo create --source %s --push`, then `gravy project set-repo %s %s`)",
				path, firstNonEmpty(errOut, errString(err)), path, p.Slug, path)
		}
	}

	return l.AttachRepository(ctx, projectID, path)
}

// readme is the first commit's only file: the project's name, and its notes when it has them,
// so the repository says what it is for from the start.
func readme(p core.Project) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", firstNonEmpty(p.Name, p.Slug))
	if notes := strings.TrimSpace(p.Notes); notes != "" {
		b.WriteString("\n" + notes + "\n")
	}
	return b.String()
}

// runOn runs a command on a host and returns its stdout, its stderr and its exit code.
//
// runGit throws stderr away, which is right for its reads and wrong here: when `git commit`
// fails for want of an identity, or `gh` is not logged in, stderr is the whole explanation.
func runOn(ctx context.Context, h host.Host, dir, cmd string, args ...string) (string, string, int, error) {
	p, err := h.Exec(ctx, host.ExecSpec{
		Cmd: cmd, Args: args, Dir: dir, Timeout: 2 * time.Minute,
		Env: map[string]string{"GIT_TERMINAL_PROMPT": "0"},
	})
	if err != nil {
		return "", "", 0, err
	}
	outCh := slurp(p.Stdout())
	errCh := slurp(p.Stderr())
	st, waitErr := p.Wait()
	out, errOut := <-outCh, <-errCh
	if waitErr != nil {
		return "", strings.TrimSpace(errOut), 0, waitErr
	}
	return out, strings.TrimSpace(errOut), st.Code, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func errString(err error) string {
	if err == nil {
		return "exit status non-zero"
	}
	return err.Error()
}
