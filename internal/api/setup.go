package api

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
)

// SetupInfo is the read-only starting point for a setup draft.
type SetupInfo struct {
	Settings Settings
	Agents   []AgentStatus
	Caps     core.Caps
	Projects []core.Project
}

// SetupPreview contains editable suggestions, not persisted configuration.
type SetupPreview struct {
	Project  core.Project
	Evidence []string
}

// SetupRequest carries the original settings and explicitly approved draft.
// Existing projects are never rewritten by this flow; Project adds a repository or is nil.
type SetupRequest struct {
	Original config.Config
	Config   config.Config
	Project  *AddProjectReq
}

// SetupInfo detects agents and local capabilities without saving any findings.
func (l *Local) SetupInfo(ctx context.Context) (SetupInfo, error) {
	var out SetupInfo
	var err error
	if out.Settings, err = l.GetSettings(ctx); err != nil {
		return out, err
	}
	if out.Projects, err = l.ListProjects(ctx); err != nil {
		return out, err
	}
	out.Agents = l.DetectAgents(ctx)
	if h, err := l.hostFor(""); err == nil {
		out.Caps, _ = h.Capabilities(ctx)
	}
	return out, nil
}

// PreviewSetup validates a repository and proposes commands from the shared toolchain table.
func (l *Local) PreviewSetup(ctx context.Context, req AddProjectReq) (SetupPreview, error) {
	p, err := l.prepareProject(ctx, req)
	if err != nil {
		return SetupPreview{}, err
	}
	h, err := l.hostFor(req.Host)
	if err != nil {
		return SetupPreview{}, err
	}
	files, code, err := runGit(ctx, h, p.RepoPath, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return SetupPreview{}, err
	}
	if code != 0 {
		return SetupPreview{}, fmt.Errorf("could not inspect repository files")
	}
	p.Validation = nil
	p.Requirements = core.Requirements{Tools: map[string]string{}}
	p.Allowlist = core.Allowlist{ReadPaths: []string{"**"}, WritePaths: []string{"**"}}
	for _, cmd := range []string{"ls", "cat", "grep", "find", "git status", "git diff", "git log", "cd"} {
		p.Allowlist.Commands = append(p.Allowlist.Commands, core.Pattern{Match: cmd, Note: "setup suggestion"})
	}
	out := SetupPreview{Project: p}
	seen := map[string]bool{}
	for _, file := range strings.Split(files, "\x00") {
		if file == "" {
			continue
		}
		for _, tc := range toolchains {
			if filepath.Base(file) != tc.marker {
				continue
			}
			depth := strings.Count(filepath.ToSlash(file), "/")
			if tc.marker == "project.pbxproj" {
				if !strings.HasSuffix(filepath.Dir(file), ".xcodeproj") || depth > 2 {
					continue
				}
			} else if depth > 1 {
				continue
			}
			out.Evidence = append(out.Evidence, file+": "+tc.note)
			dir := filepath.Dir(file)
			if tc.marker == "project.pbxproj" {
				dir = filepath.Dir(dir)
			}
			for _, cmd := range tc.commands {
				if !seen[cmd] {
					seen[cmd] = true
					out.Project.Allowlist.Commands = append(out.Project.Allowlist.Commands, core.Pattern{Match: cmd, Note: "detected " + tc.note})
				}
			}
			for _, tool := range tc.tools {
				out.Project.Requirements.Tools[tool] = ""
			}
			if tc.os != "" {
				out.Project.Requirements.OS = []string{tc.os}
			}
			for _, check := range tc.checks {
				command := check
				if dir != "." {
					command = "cd '" + strings.ReplaceAll(dir, "'", "'\\''") + "' && " + check
				}
				duplicate := false
				for _, st := range out.Project.Validation {
					if st.Cmd == command {
						duplicate = true
					}
				}
				if !duplicate {
					out.Project.Validation = append(out.Project.Validation, core.Step{Name: fmt.Sprintf("check-%d", len(out.Project.Validation)+1), Cmd: command, Required: true, Timeout: 10 * time.Minute})
				}
			}
		}
	}
	return out, nil
}

// ApplySetup saves only an explicitly approved draft, rejecting stale settings. All
// validation happens before writes. A configuration-save failure rolls back a new project.
func (l *Local) ApplySetup(ctx context.Context, req SetupRequest) (Settings, error) {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	if !reflect.DeepEqual(req.Original, l.cfg) {
		return Settings{}, fmt.Errorf("settings changed while setup was open; reopen setup to keep those changes")
	}
	if err := req.Config.Validate(); err != nil {
		return Settings{}, err
	}
	var project core.Project
	var err error
	if req.Project != nil {
		if req.Project.Allowlist == nil {
			return Settings{}, fmt.Errorf("review and approve project permissions first")
		}
		project, err = l.prepareProject(ctx, *req.Project)
		if err != nil {
			return Settings{}, err
		}
		for _, step := range project.Validation {
			if strings.TrimSpace(step.Cmd) == "" {
				return Settings{}, fmt.Errorf("validation command cannot be empty")
			}
		}
		if err := l.db.CreateProject(ctx, project); err != nil {
			return Settings{}, err
		}
	}
	st, err := l.updateSettings(req.Config)
	if err != nil && project.ID != "" {
		if rollback := l.db.DeleteProject(context.WithoutCancel(ctx), project.ID); rollback != nil {
			return Settings{}, fmt.Errorf("save failed: %w; remove unfinished project %s: %w", err, project.ID, rollback)
		}
	}
	return st, err
}
