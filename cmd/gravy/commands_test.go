package main

import (
	"flag"
	"testing"
)

// TestParseFlagsAcceptsInterspersedFlags covers the ordering people actually type.
//
// Go's flag package stops at the first positional argument, so `project add <path> -validate ...`
// parsed with a bare fs.Parse silently drops every flag after the path. Silently is the problem:
// the project registers with no validation steps, and the omission only surfaces later as work
// reaching review unchecked.
func TestParseFlagsAcceptsInterspersedFlags(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantPos  []string
		wantName string
		wantSet  bool
	}{
		{
			name:     "flags before the positional",
			args:     []string{"-name", "proj", "-force", "/repo"},
			wantPos:  []string{"/repo"},
			wantName: "proj",
			wantSet:  true,
		},
		{
			name:     "flags after the positional",
			args:     []string{"/repo", "-name", "proj", "-force"},
			wantPos:  []string{"/repo"},
			wantName: "proj",
			wantSet:  true,
		},
		{
			name:     "flags on both sides",
			args:     []string{"-name", "proj", "/repo", "-force"},
			wantPos:  []string{"/repo"},
			wantName: "proj",
			wantSet:  true,
		},
		{
			name:    "several positionals keep their order",
			args:    []string{"add", "a", "-force", "thing"},
			wantPos: []string{"add", "a", "thing"},
			wantSet: true,
		},
		{
			name:    "no flags at all",
			args:    []string{"/repo"},
			wantPos: []string{"/repo"},
		},
		{
			name:    "no arguments at all",
			args:    nil,
			wantPos: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			name := fs.String("name", "", "")
			force := fs.Bool("force", false, "")

			pos, err := parseFlags(fs, tc.args)
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if len(pos) != len(tc.wantPos) {
				t.Fatalf("positionals = %q, want %q", pos, tc.wantPos)
			}
			for i := range pos {
				if pos[i] != tc.wantPos[i] {
					t.Fatalf("positionals = %q, want %q", pos, tc.wantPos)
				}
			}
			if *name != tc.wantName {
				t.Errorf("-name = %q, want %q", *name, tc.wantName)
			}
			if *force != tc.wantSet {
				t.Errorf("-force = %v, want %v", *force, tc.wantSet)
			}
		})
	}
}

// TestParseFlagsReportsUnknownFlags checks that accepting a looser order does not also swallow
// a typo, which would send the user hunting for a setting that never applied.
func TestParseFlagsReportsUnknownFlags(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(discard{})
	fs.String("name", "", "")

	if _, err := parseFlags(fs, []string{"/repo", "-bogus", "typo"}); err == nil {
		t.Error("parseFlags accepted an unknown flag")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
