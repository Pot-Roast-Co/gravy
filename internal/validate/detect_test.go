package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureRepo builds a directory containing the given files.
func fixtureRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestDetectGo is half of AC4.
func TestDetectGo(t *testing.T) {
	repo := fixtureRepo(t, map[string]string{
		"go.mod":  "module example.com/thing\n\ngo 1.23\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})

	proposals := Detect(repo)
	if len(proposals) != 1 {
		t.Fatalf("got %d proposals, want 1: %+v", len(proposals), proposals)
	}
	p := proposals[0]
	if p.Type != TypeGo {
		t.Errorf("type = %q, want go", p.Type)
	}

	cmds := map[string]bool{}
	required := map[string]bool{}
	for _, s := range p.Steps {
		cmds[s.Cmd] = true
		required[s.Cmd] = s.Required
	}
	for _, want := range []string{"go build ./...", "go test ./...", "go vet ./..."} {
		if !cmds[want] {
			t.Errorf("proposal is missing %q; got %+v", want, p.Steps)
		}
	}
	if !required["go build ./..."] || !required["go test ./..."] {
		t.Error("build and test must be required steps")
	}
	// vet catches real bugs but also flags patterns a project may have chosen to live with;
	// failing a ticket on that is noise.
	if required["go vet ./..."] {
		t.Error("go vet should be optional, not required")
	}
	if len(p.Evidence) == 0 {
		t.Error("no evidence recorded; a human cannot judge the proposal")
	}
}

// TestDetectNode is the other half of AC4.
func TestDetectNode(t *testing.T) {
	repo := fixtureRepo(t, map[string]string{
		"package.json": `{"name":"thing","scripts":{"build":"tsc","test":"vitest run","lint":"eslint ."}}`,
	})

	proposals := Detect(repo)
	if len(proposals) != 1 || proposals[0].Type != TypeNode {
		t.Fatalf("proposals = %+v", proposals)
	}

	cmds := map[string]bool{}
	required := map[string]bool{}
	for _, s := range proposals[0].Steps {
		cmds[s.Cmd] = true
		required[s.Cmd] = s.Required
	}
	for _, want := range []string{"npm run build", "npm run test", "npm run lint"} {
		if !cmds[want] {
			t.Errorf("missing %q; got %+v", want, proposals[0].Steps)
		}
	}
	if required["npm run lint"] {
		t.Error("lint should be optional")
	}
	if !required["npm run test"] {
		t.Error("test should be required")
	}
}

// TestDetectNodeOnlyProposesScriptsThatExist is the case that bites in practice.
//
// Proposing `npm test` for a project with no test script produces a step that fails for a reason
// unrelated to the agent's work — the worst kind of validation failure, because it looks exactly
// like a real one and sends the ticket back to an agent that did nothing wrong.
func TestDetectNodeOnlyProposesScriptsThatExist(t *testing.T) {
	repo := fixtureRepo(t, map[string]string{
		"package.json": `{"name":"thing","scripts":{"build":"tsc"}}`,
	})

	proposals := Detect(repo)
	if len(proposals) != 1 {
		t.Fatalf("proposals = %+v", proposals)
	}
	for _, s := range proposals[0].Steps {
		if strings.Contains(s.Cmd, "test") || strings.Contains(s.Cmd, "lint") {
			t.Errorf("proposed %q for a project that does not define that script", s.Cmd)
		}
	}
	if len(proposals[0].Steps) != 1 {
		t.Errorf("got %d steps, want only build: %+v", len(proposals[0].Steps), proposals[0].Steps)
	}
}

func TestDetectNodeWithUnparseablePackageJSON(t *testing.T) {
	repo := fixtureRepo(t, map[string]string{"package.json": "{not json"})
	proposals := Detect(repo)
	// It is still a Node project; the scripts simply cannot be read.
	if len(proposals) != 1 || proposals[0].Type != TypeNode {
		t.Fatalf("proposals = %+v", proposals)
	}
	if len(proposals[0].Steps) != 0 {
		t.Errorf("steps were proposed from unreadable scripts: %+v", proposals[0].Steps)
	}
}

func TestDetectOthers(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  ProjectType
		cmd   string
	}{
		{"python pyproject", map[string]string{"pyproject.toml": "[project]\nname='x'\n"}, TypePython, "pytest"},
		{"python setup.py", map[string]string{"setup.py": "from setuptools import setup\n"}, TypePython, "pytest"},
		{"python requirements", map[string]string{"requirements.txt": "pytest\n"}, TypePython, "pytest"},
		{"rust", map[string]string{"Cargo.toml": "[package]\nname='x'\n"}, TypeRust, "cargo test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proposals := Detect(fixtureRepo(t, tt.files))
			if len(proposals) != 1 || proposals[0].Type != tt.want {
				t.Fatalf("proposals = %+v, want %q", proposals, tt.want)
			}
			found := false
			for _, s := range proposals[0].Steps {
				if strings.Contains(s.Cmd, tt.cmd) {
					found = true
				}
			}
			if !found {
				t.Errorf("no step containing %q: %+v", tt.cmd, proposals[0].Steps)
			}
		})
	}
}

func TestDetectXcodePrefersWorkspace(t *testing.T) {
	repo := t.TempDir()
	for _, d := range []string{"Thing.xcodeproj", "Thing.xcworkspace"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	proposals := Detect(repo)
	if len(proposals) != 1 || proposals[0].Type != TypeXcode {
		t.Fatalf("proposals = %+v", proposals)
	}
	cmd := proposals[0].Steps[0].Cmd
	if !strings.Contains(cmd, "-workspace") {
		t.Errorf("cmd = %q, want the workspace to supersede the project file", cmd)
	}
	// The scheme cannot be guessed, so it is left as a placeholder rather than proposing a
	// command that fails on every project but the author's.
	if !strings.Contains(cmd, "<SCHEME>") {
		t.Errorf("cmd = %q, want a placeholder for the scheme", cmd)
	}
	if !NeedsAttention(proposals) {
		t.Error("NeedsAttention is false despite an unfilled placeholder")
	}
}

func TestDetectMultipleTypes(t *testing.T) {
	// A Go backend with a Node frontend is ordinary. Picking one arbitrarily would silently
	// drop half the project's tests.
	repo := fixtureRepo(t, map[string]string{
		"go.mod":           "module example.com/thing\n\ngo 1.23\n",
		"web/package.json": `{"scripts":{"build":"vite build"}}`,
		"package.json":     `{"scripts":{"test":"vitest run"}}`,
	})

	proposals := Detect(repo)
	if len(proposals) != 2 {
		t.Fatalf("got %d proposals, want 2 (go and node): %+v", len(proposals), proposals)
	}
	types := map[ProjectType]bool{}
	for _, p := range proposals {
		types[p.Type] = true
	}
	if !types[TypeGo] || !types[TypeNode] {
		t.Errorf("types = %v, want both go and node", types)
	}

	steps := Steps(proposals)
	if len(steps) < 4 {
		t.Errorf("flattened steps = %+v, want every proposal's steps", steps)
	}
}

func TestDetectEmptyRepo(t *testing.T) {
	if got := Detect(t.TempDir()); len(got) != 0 {
		t.Errorf("Detect on an empty directory = %+v, want none", got)
	}
	if NeedsAttention(nil) {
		t.Error("NeedsAttention is true for no proposals")
	}
}

// TestDetectWritesNothing is AC5, and the invariant the whole design rests on: detection
// proposes, the human approves.
//
// A wrong guess that silently becomes policy is worse than no guess: it produces a validation
// suite that passes for the wrong reasons, and nobody ever looks at it again.
func TestDetectWritesNothing(t *testing.T) {
	repo := fixtureRepo(t, map[string]string{
		"go.mod":       "module example.com/thing\n\ngo 1.23\n",
		"package.json": `{"scripts":{"test":"vitest run"}}`,
	})

	before := snapshot(t, repo)
	proposals := Detect(repo)
	if len(proposals) == 0 {
		t.Fatal("nothing detected; the test would prove nothing")
	}
	after := snapshot(t, repo)

	if len(before) != len(after) {
		t.Fatalf("detection changed the file list: %v -> %v", before, after)
	}
	for name, content := range before {
		if after[name] != content {
			t.Errorf("detection modified %q", name)
		}
	}
	// Specifically: no config file appeared.
	for name := range after {
		if strings.Contains(name, ".gravy") {
			t.Errorf("detection wrote %q into the repository", name)
		}
	}
}

// snapshot records every file's contents under dir.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return out
}
