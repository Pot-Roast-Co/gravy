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

// rule forbids a set of imports everywhere except under an allowed set of path prefixes.
type rule struct {
	name    string
	reason  string
	allowed []string // slash-separated dir prefixes, relative to module root
	// forbidden reports whether an import path violates this rule.
	forbidden func(importPath string) bool
}

var rules = []rule{
	{
		name:    "Rule 1 (execution)",
		reason:  "all execution must go through internal/host — see ARCHITECTURE.md §1.1",
		allowed: []string{"internal/host"},
		forbidden: func(p string) bool {
			return p == "os/exec"
		},
	},
	{
		name:    "Rule 2 (presentation)",
		reason:  "core must never import presentation — see ARCHITECTURE.md §1.1",
		allowed: []string{"internal/tui", "cmd"},
		forbidden: func(p string) bool {
			for _, ui := range []string{
				"github.com/charmbracelet/bubbletea",
				"github.com/charmbracelet/lipgloss",
				"github.com/charmbracelet/bubbles",
			} {
				if p == ui || strings.HasPrefix(p, ui+"/") {
					return true
				}
			}
			return false
		},
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
		fmt.Println("lint-layers: ok — both layering rules hold")
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
		fmt.Fprintf(os.Stderr, "\n%s: %s\n  permitted only under: %s\n", r.name, r.reason, strings.Join(r.allowed, ", "))
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
				if !r.forbidden(imp) || allows(r, dir) {
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

// allows reports whether dir sits under one of the rule's permitted prefixes.
func allows(r rule, dir string) bool {
	for _, a := range r.allowed {
		if dir == a || strings.HasPrefix(dir, a+"/") {
			return true
		}
	}
	return false
}
