package api

import (
	"context"
	"path/filepath"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
)

// toolchains maps a marker file to the commands that project type needs.
//
// The allowlist is one of the three real boundaries in PRODUCT.md, and it ships empty — which
// does not fail loudly, it just refuses every build command an agent tries. Observed: an Elixir
// project where three consecutive runs burned turns being denied `mix deps.get`, one of them
// giving up after ninety-four.
//
// Seeded, not silent: what is detected is written to the project where the human can see and
// change it, rather than applied invisibly at run time.
var toolchains = []struct {
	marker   string
	commands []string
	note     string
}{
	{"mix.exs", []string{"mix", "elixir", "iex"}, "an Elixir project"},
	{"go.mod", []string{"go", "gofmt"}, "a Go module"},
	{"package.json", []string{"npm", "npx", "node", "yarn", "pnpm"}, "a Node project"},
	{"Cargo.toml", []string{"cargo", "rustc"}, "a Rust crate"},
	{"pyproject.toml", []string{"python3", "pip", "pytest", "uv"}, "a Python project"},
	{"requirements.txt", []string{"python3", "pip", "pytest"}, "a Python project"},
	{"Package.swift", []string{"swift", "xcodebuild"}, "a Swift package"},
	{"Gemfile", []string{"bundle", "ruby", "rake"}, "a Ruby project"},
}

// seedAllowlist detects a project's toolchain and returns the commands it needs.
//
// Looked for at the repository root and one level down, because a backend in a subdirectory is
// the ordinary shape of a project with a web front end — and that is exactly the layout that
// defeated the detection this replaces.
func seedAllowlist(ctx context.Context, h host.Host, repoPath string) core.Allowlist {
	if h == nil || repoPath == "" {
		return core.Allowlist{}
	}

	subdirs := []string{"", "backend", "server", "api", "app", "src"}
	seen := map[string]bool{}
	var out []core.Pattern

	for _, tc := range toolchains {
		for _, dir := range subdirs {
			if !h.FS().Exists(filepath.Join(repoPath, dir, tc.marker)) {
				continue
			}
			for _, cmd := range tc.commands {
				if seen[cmd] {
					continue
				}
				seen[cmd] = true
				out = append(out, core.Pattern{Match: cmd, Note: "detected " + tc.note})
			}
			break
		}
	}
	if len(out) == 0 {
		return core.Allowlist{}
	}

	// cd, because a toolchain in a subdirectory is reached by changing into it, and every
	// command above is useless without it.
	if !seen["cd"] {
		out = append(out, core.Pattern{Match: "cd", Note: "to reach the project directory"})
	}
	return core.Allowlist{Commands: out}
}
