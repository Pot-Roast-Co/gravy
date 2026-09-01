package agentrun

import (
	"path/filepath"

	"github.com/bobbybrady/gravy/internal/core"
	"github.com/bobbybrady/gravy/internal/git"
	"github.com/bobbybrady/gravy/internal/host"
)

// LocalRepos builds a git repository manager per project, rooted under the Gravy home.
type LocalRepos struct {
	Host host.Host
	// Home is ~/.gravy. Worktrees live at Home/projects/<slug>/worktrees, deliberately outside
	// the repository being worked on.
	Home string
}

// For returns the repository manager for a project.
func (r LocalRepos) For(p core.Project) (Repo, error) {
	worktreeRoot := filepath.Join(r.Home, "projects", p.Slug, "worktrees")
	return git.NewLocalRepo(r.Host, p.RepoPath, worktreeRoot), nil
}

var _ Repos = LocalRepos{}
