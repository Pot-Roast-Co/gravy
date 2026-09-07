package claudecode

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pot-roast-co/gravy/internal/provider"
)

// settings is the per-run settings document passed via --settings.
//
// It is written next to the run's other artefacts, never into the worktree. An in-worktree
// settings file would be swept up by the WIP commit that precedes an escalation, land in the
// merge, and dirty git status in exactly the tree a human may be resolving a conflict in.
type settings struct {
	Hooks *hooks `json:"hooks,omitempty"`
}

type hooks struct {
	PreToolUse []hookMatcher `json:"PreToolUse,omitempty"`
}

type hookMatcher struct {
	Matcher string      `json:"matcher"`
	Hooks   []hookEntry `json:"hooks"`
}

type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// PermissionHookCommand is the executable the pre-tool hook invokes.
//
// GR-035 defines the contract and supplies the real broker. Until it lands this is empty and no
// hook is installed, so the CLI's own permission handling applies via DefaultPermissionMode.
//
// GR-012's scope asked for "a stub that always allows" here. That is deliberately not what
// ships. An always-allow hook would approve every tool call, removing the only brake that exists
// before the broker lands — and measurement shows the CLI's own acceptEdits mode already
// approximates the product's intended default allowlist far better: worktree reads and writes
// and ordinary shell are permitted, while network access is denied and destructive commands
// stop the agent (docs/SPIKE-claude-code.md, F6). Shipping the looser stub would have been a
// regression in safety dressed as ticket compliance.
//
// The mechanism itself is verified: the spike confirmed a PreToolUse hook receives the tool call
// on stdin and can deny it by writing a permissionDecision, that the denial is reported back in
// the result's permission_denials array with the tool name and arguments, and that a decision of
// "ask" degrades to a denial in headless mode rather than hanging on a prompt nobody can see.
var PermissionHookCommand = ""

// writeSettings writes the per-run settings file, returning its path and a cleanup function.
//
// It returns an empty path when there is nothing to configure, in which case no --settings flag
// is passed at all.
func writeSettings(t provider.AgentTask) (string, func(), error) {
	if PermissionHookCommand == "" {
		return "", nil, nil
	}

	doc := settings{Hooks: &hooks{PreToolUse: []hookMatcher{{
		// An empty matcher applies to every tool; the broker decides, not the matcher.
		Matcher: "",
		Hooks:   []hookEntry{{Type: "command", Command: PermissionHookCommand}},
	}}}}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", nil, fmt.Errorf("claude-code: encode settings: %w", err)
	}

	dir := runArtifactDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("claude-code: create run directory: %w", err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", nil, fmt.Errorf("claude-code: write settings: %w", err)
	}
	return path, func() { os.Remove(path) }, nil
}

// runArtifactDir returns where per-run files belong: beside the run's log, which the orchestrator
// has already placed under ~/.gravy/runs/<run-id>/.
func runArtifactDir(t provider.AgentTask) string {
	switch {
	case t.LogPath != "":
		return filepath.Dir(t.LogPath)
	case t.AskPath != "":
		return filepath.Dir(t.AskPath)
	default:
		return filepath.Join(os.TempDir(), "gravy-run-"+t.RunID)
	}
}

// newUUID returns a random RFC 4122 version 4 UUID.
//
// The CLI requires --session-id to be a valid UUID. Gravy generates it rather than scraping one
// from output, so the id is known before the process starts and can be recorded on the run row
// immediately.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a condition worth degrading for.
		panic(fmt.Sprintf("claude-code: generate session id: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
