package validate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/bobbybrady/gravy/internal/core"
)

// ProjectType is a recognised kind of repository.
type ProjectType string

// The recognised project types.
const (
	TypeGo     ProjectType = "go"
	TypeNode   ProjectType = "node"
	TypePython ProjectType = "python"
	TypeRust   ProjectType = "rust"
	TypeXcode  ProjectType = "xcode"
)

// Proposal is what detection found and what it suggests running.
//
// It is a proposal in the strict sense: detection proposes and the human approves. Nothing here
// is written to a project's configuration without an explicit accept, because a wrong guess that
// silently becomes policy is worse than no guess at all — it produces a validation suite that
// passes for the wrong reasons.
type Proposal struct {
	Type ProjectType
	// Steps are the suggested validation commands, in the order they should run.
	Steps []core.Step
	// Evidence names the files that led to this conclusion, so a human can judge it.
	Evidence []string
}

// Detect examines a repository and proposes validation steps for every project type it finds.
//
// Multiple proposals are possible and are returned in a stable order: a repository with a Go
// backend and a Node frontend is ordinary, and picking one arbitrarily would silently drop half
// the project's tests.
func Detect(repoPath string) []Proposal {
	var out []Proposal

	if p, ok := detectGo(repoPath); ok {
		out = append(out, p)
	}
	if p, ok := detectNode(repoPath); ok {
		out = append(out, p)
	}
	if p, ok := detectPython(repoPath); ok {
		out = append(out, p)
	}
	if p, ok := detectRust(repoPath); ok {
		out = append(out, p)
	}
	if p, ok := detectXcode(repoPath); ok {
		out = append(out, p)
	}
	return out
}

func detectGo(repo string) (Proposal, bool) {
	if !exists(filepath.Join(repo, "go.mod")) {
		return Proposal{}, false
	}
	return Proposal{
		Type:     TypeGo,
		Evidence: []string{"go.mod"},
		Steps: []core.Step{
			{Name: "build", Cmd: "go build ./...", Required: true, Timeout: 5 * time.Minute},
			{Name: "test", Cmd: "go test ./...", Required: true, Timeout: 10 * time.Minute},
			// vet is advisory: it catches real bugs but also flags patterns a project may
			// have decided to live with, and failing a ticket on that would be noise.
			{Name: "vet", Cmd: "go vet ./...", Required: false, Timeout: 5 * time.Minute},
		},
	}, true
}

func detectNode(repo string) (Proposal, bool) {
	path := filepath.Join(repo, "package.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return Proposal{}, false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		// A package.json that does not parse still identifies the project as Node; the
		// scripts simply cannot be read.
		return Proposal{Type: TypeNode, Evidence: []string{"package.json (unparseable)"}}, true
	}

	// Only scripts the project actually defines are proposed. Suggesting `npm test` for a
	// project with no test script produces a step that fails for a reason unrelated to the
	// agent's work — the worst kind of validation failure, because it looks like a real one.
	var steps []core.Step
	for _, s := range []struct {
		script   string
		name     string
		required bool
		timeout  time.Duration
	}{
		{"build", "build", true, 10 * time.Minute},
		{"test", "test", true, 15 * time.Minute},
		{"typecheck", "typecheck", true, 5 * time.Minute},
		{"lint", "lint", false, 5 * time.Minute},
	} {
		if _, ok := pkg.Scripts[s.script]; ok {
			steps = append(steps, core.Step{
				Name:     s.name,
				Cmd:      "npm run " + s.script,
				Required: s.required,
				Timeout:  s.timeout,
			})
		}
	}
	return Proposal{Type: TypeNode, Evidence: []string{"package.json"}, Steps: steps}, true
}

func detectPython(repo string) (Proposal, bool) {
	var evidence []string
	for _, f := range []string{"pyproject.toml", "setup.py", "requirements.txt", "setup.cfg"} {
		if exists(filepath.Join(repo, f)) {
			evidence = append(evidence, f)
		}
	}
	if len(evidence) == 0 {
		return Proposal{}, false
	}
	sort.Strings(evidence)
	return Proposal{
		Type:     TypePython,
		Evidence: evidence,
		Steps: []core.Step{
			{Name: "test", Cmd: "pytest", Required: true, Timeout: 15 * time.Minute},
		},
	}, true
}

func detectRust(repo string) (Proposal, bool) {
	if !exists(filepath.Join(repo, "Cargo.toml")) {
		return Proposal{}, false
	}
	return Proposal{
		Type:     TypeRust,
		Evidence: []string{"Cargo.toml"},
		Steps: []core.Step{
			{Name: "build", Cmd: "cargo build", Required: true, Timeout: 15 * time.Minute},
			{Name: "test", Cmd: "cargo test", Required: true, Timeout: 20 * time.Minute},
			{Name: "clippy", Cmd: "cargo clippy", Required: false, Timeout: 10 * time.Minute},
		},
	}, true
}

func detectXcode(repo string) (Proposal, bool) {
	entries, err := os.ReadDir(repo)
	if err != nil {
		return Proposal{}, false
	}
	var project string
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) == ".xcworkspace" {
			project = name
			break // a workspace supersedes a project file
		}
		if filepath.Ext(name) == ".xcodeproj" && project == "" {
			project = name
		}
	}
	if project == "" {
		return Proposal{}, false
	}

	flag := "-project"
	if filepath.Ext(project) == ".xcworkspace" {
		flag = "-workspace"
	}
	// The scheme cannot be guessed reliably, so the command is left for the human to complete
	// rather than proposing one that will fail on every project but the author's.
	return Proposal{
		Type:     TypeXcode,
		Evidence: []string{project},
		Steps: []core.Step{{
			Name:     "test",
			Cmd:      fmt.Sprintf("xcodebuild test %s %s -scheme <SCHEME>", flag, project),
			Required: true,
			Timeout:  30 * time.Minute,
		}},
	}, true
}

// Steps flattens proposals into a single ordered list, for a human to accept wholesale.
func Steps(proposals []Proposal) []core.Step {
	var out []core.Step
	for _, p := range proposals {
		out = append(out, p.Steps...)
	}
	return out
}

// NeedsAttention reports whether any proposed step still contains a placeholder the human must
// fill in before it can run.
func NeedsAttention(proposals []Proposal) bool {
	for _, p := range proposals {
		for _, s := range p.Steps {
			if containsPlaceholder(s.Cmd) {
				return true
			}
		}
	}
	return false
}

// containsPlaceholder reports whether a command still has an <ANGLE_BRACKET> slot in it.
func containsPlaceholder(cmd string) bool {
	open := false
	for _, r := range cmd {
		switch r {
		case '<':
			open = true
		case '>':
			if open {
				return true
			}
		}
	}
	return false
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
