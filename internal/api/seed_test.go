package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/host"
)

// seedHost answers Exists from a set of paths, which is all seeding asks of a host.
type seedHost struct {
	host.Host
	files map[string]bool
}

func (h seedHost) FS() host.FS { return seedFS(h.files) }

type seedFS map[string]bool

func (f seedFS) Exists(path string) bool                     { return f[filepath.ToSlash(path)] }
func (f seedFS) ReadFile(string) ([]byte, error)             { return nil, os.ErrNotExist }
func (f seedFS) WriteFile(string, []byte, os.FileMode) error { return os.ErrPermission }
func (f seedFS) Stat(string) (os.FileInfo, error)            { return nil, os.ErrNotExist }
func (f seedFS) MkdirAll(string, os.FileMode) error          { return os.ErrPermission }
func (f seedFS) RemoveAll(string) error                      { return os.ErrPermission }

func commands(a []string) map[string]bool {
	out := map[string]bool{}
	for _, c := range a {
		out[c] = true
	}
	return out
}

// TestSeedAllowlist is the cost of shipping this empty.
//
// An empty allowlist refuses every build command an agent runs, silently. Three consecutive runs
// on an Elixir project burned turns being denied `mix deps.get`; one gave up after ninety-four.
func TestSeedAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  []string
		none  bool
	}{
		{
			name:  "an Elixir app at the root",
			files: []string{"/repo/mix.exs"},
			want:  []string{"mix", "elixir", "iex", "cd"},
		},
		{
			// The layout that defeated detection: a web app with its server in backend/.
			name:  "an Elixir app in backend/",
			files: []string{"/repo/backend/mix.exs", "/repo/README.md"},
			want:  []string{"mix", "cd"},
		},
		{
			name:  "a Go module",
			files: []string{"/repo/go.mod"},
			want:  []string{"go", "gofmt", "cd"},
		},
		{
			name:  "both, because a repository can be both",
			files: []string{"/repo/go.mod", "/repo/package.json"},
			want:  []string{"go", "npm", "node", "cd"},
		},
		{
			name:  "nothing recognised stays empty rather than guessing",
			files: []string{"/repo/README.md"},
			none:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]bool{}
			for _, f := range tc.files {
				files[f] = true
			}
			got := seedAllowlist(context.Background(), seedHost{files: files}, "/repo")

			if tc.none {
				if len(got.Commands) != 0 {
					t.Fatalf("seeded %+v for an unrecognised project", got.Commands)
				}
				return
			}

			names := make([]string, 0, len(got.Commands))
			for _, c := range got.Commands {
				names = append(names, c.Match)
				if c.Note == "" {
					t.Errorf("command %q has no note saying why it is allowed", c.Match)
				}
			}
			have := commands(names)
			for _, w := range tc.want {
				if !have[w] {
					t.Errorf("missing %q; got %s", w, strings.Join(names, ", "))
				}
			}
		})
	}
}

// TestSeedAllowlistWithoutAHost: a project registered with no host to inspect gets nothing,
// rather than a guess.
func TestSeedAllowlistWithoutAHost(t *testing.T) {
	if got := seedAllowlist(context.Background(), nil, "/repo"); len(got.Commands) != 0 {
		t.Errorf("seeded %+v with no host", got.Commands)
	}
}
