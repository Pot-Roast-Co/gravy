// Command lintlayers enforces the two layering rules from ARCHITECTURE.md §1.1.
//
// Rule 1: only internal/host may import os/exec — every command run goes through the Host
// interface, which is what makes remote hosts a later addition rather than a rewrite.
//
// Rule 2: only internal/tui and cmd/ may import a terminal UI library — core is a library and
// the TUI is merely one client of it.
//
// These are checked mechanically because a convention nobody can violate by accident is worth
// far more than one everybody agrees with. Run via `make lint-layers`.
package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// rule is one layering constraint: which directories it applies to, and which imports it
// forbids there.
type rule struct {
	name   string
	reason string
	// scope describes, for humans, where the rule bites.
	scope string
	// applies reports whether a package directory is subject to this rule.
	applies func(dir string) bool
	// forbidden reports whether an import path violates it.
	forbidden func(importPath string) bool
}

// except returns a predicate matching every directory outside the given prefixes.
func except(prefixes ...string) func(string) bool {
	return func(dir string) bool { return !under(dir, prefixes) }
}

// only returns a predicate matching directories at or beneath the given prefixes.
func only(prefixes ...string) func(string) bool {
	return func(dir string) bool { return under(dir, prefixes) }
}

// under reports whether dir sits at or beneath one of the prefixes. It compares whole path
// segments, so internal/hostile does not match internal/host.
func under(dir string, prefixes []string) bool {
	for _, p := range prefixes {
		if dir == p || strings.HasPrefix(dir, p+"/") {
			return true
		}
	}
	return false
}

// isStdlib reports whether an import path belongs to the standard library. Every module path
// has a dot in its first segment (github.com/..., modernc.org/...); no standard library package
// does. Gravy's own packages are module-qualified, so they are correctly treated as non-stdlib.
func isStdlib(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

func anyPrefix(importPath string, prefixes []string) bool {
	for _, p := range prefixes {
		if importPath == p || strings.HasPrefix(importPath, p+"/") {
			return true
		}
	}
	return false
}

var rules = []rule{
	{
		name:    "Rule 1 (execution)",
		reason:  "all execution must go through internal/host - see ARCHITECTURE.md 1.1",
		scope:   "permitted only under: internal/host",
		applies: except("internal/host"),
		forbidden: func(p string) bool {
			return p == "os/exec"
		},
	},
	{
		name:    "Rule 2 (presentation)",
		reason:  "core must never import presentation - see ARCHITECTURE.md 1.1",
		scope:   "permitted only under: internal/tui, cmd",
		applies: except("internal/tui", "cmd"),
		forbidden: func(p string) bool {
			return anyPrefix(p, []string{
				"github.com/charmbracelet/bubbletea",
				"github.com/charmbracelet/lipgloss",
				"github.com/charmbracelet/bubbles",
			})
		},
	},
	{
		// ARCHITECTURE.md 2: core depends on nothing. It is the vocabulary of the whole
		// system, so anything it imports every other package inherits.
		name:      "Rule 3 (core purity)",
		reason:    "internal/core may import only the standard library - see ARCHITECTURE.md 2",
		scope:     "applies to: internal/core",
		applies:   only("internal/core"),
		forbidden: func(p string) bool { return !isStdlib(p) },
	},
}

type violation struct {
	file string
	line int
	imp  string
	rule string
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}

	violations, err := check(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lint-layers:", err)
		os.Exit(2)
	}

	if len(violations) == 0 {
		fmt.Println("lint-layers: ok — all layering rules hold")
		return
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].line < violations[j].line
	})
	for _, v := range violations {
		fmt.Fprintf(os.Stderr, "%s:%d: %s violates %s\n", v.file, v.line, strconv.Quote(v.imp), v.rule)
	}
	for _, r := range rules {
		fmt.Fprintf(os.Stderr, "\n%s: %s\n  %s\n", r.name, r.reason, r.scope)
	}
	fmt.Fprintf(os.Stderr, "\n%d violation(s).\n", len(violations))
	os.Exit(1)
}

func check(root string) ([]violation, error) {
	var out []violation
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(filepath.Dir(rel))

		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}

		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			for _, r := range rules {
				if !r.applies(dir) || !r.forbidden(imp) {
					continue
				}
				out = append(out, violation{
					file: filepath.ToSlash(rel),
					line: fset.Position(spec.Pos()).Line,
					imp:  imp,
					rule: r.name,
				})
			}
		}
		return nil
	})
	return out, err
}
