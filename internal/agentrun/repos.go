package agentrun

import (
	"fmt"
	"path/filepath"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/git"
	"github.com/pot-roast-co/gravy/internal/host"
)

// LocalRepos resolves a project to the repository manager for the machine it lives on.
//
// "Local" now means "on the host that owns the project" rather than "on this machine". A project
// pinned to another host has its clone, its worktrees and its git binary there, and running its
// git on this machine points a local process at a path that does not exist here — which surfaces
// as `fork/exec /usr/bin/git: no such file or directory`, an error about a missing working
// directory that reads like a missing binary.
type LocalRepos struct {
	// Hosts are every machine work may run on, by id.
	Hosts map[string]host.Host
	// Default is the host for a project that names none.
	Default host.Host
	// Home is ~/.gravy. Worktrees live at Home/projects/<slug>/worktrees, deliberately outside
	// the repository being worked on.
	//
	// On a remote host this path is interpreted there, so a Mac's worktrees land in its own
	// home directory rather than in a Linux path that means nothing to it.
	Home string
	// Homes is the Gravy home directory on each host that is not this machine, by host id.
	// A host with no entry falls back to Home.
	Homes map[string]string
}

// For returns the repository manager for a project, on the host it belongs to.
func (r LocalRepos) For(p core.Project) (Repo, error) {
	h := r.Default
	if p.HostID != "" {
		var ok bool
		if h, ok = r.Hosts[p.HostID]; !ok {
			return nil, fmt.Errorf("project %s is on host %q, which is not configured", p.Slug, p.HostID)
		}
	}
	if h == nil {
		return nil, fmt.Errorf("project %s has no host to run on", p.Slug)
	}

	home := r.Home
	if remote, ok := r.Homes[p.HostID]; ok && remote != "" {
		home = remote
	}
	worktreeRoot := filepath.Join(home, "projects", p.Slug, "worktrees")
	return git.NewLocalRepo(h, p.RepoPath, worktreeRoot), nil
}

var _ Repos = LocalRepos{}
