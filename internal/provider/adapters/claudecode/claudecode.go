// Package claudecode drives the Claude Code CLI as a managed, non-interactive worker.
//
// Every flag used here was verified against the real binary during the GR-000 spike; see
// docs/SPIKE-claude-code.md for what was tested and what the observed output looks like.
//
// Gravy drives the user's existing authenticated CLI. It does not want credentials, does not
// store them, and never touches the Anthropic API directly.
package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
)

// ID is the provider's registry key.
const ID = "claude-code"

// DefaultCommand is the executable name, looked up on PATH.
const DefaultCommand = "claude"

// DefaultPermissionMode is how tool use is gated in the absence of the permission broker.
//
// acceptEdits permits file edits, which the worktree bounds, while still gating shell commands.
// That matches the product's default allowlist — read and write anywhere in the worktree,
// everything else escalates — as closely as a CLI flag can before GR-035 lands.
//
// It is not merely a convenience: with no permission mode at all, the CLI denies edits and then
// reports the run as successful, so an agent that was refused every tool call is indistinguishable
// from one that had nothing to do.
const DefaultPermissionMode = "acceptEdits"

// Provider implements provider.Provider over the claude CLI.
type Provider struct {
	command        string
	permissionMode string
	// newSessionID generates the session identifier passed to --session-id. Overridable for
	// deterministic tests.
	newSessionID func() string
}

// Option configures the provider.
type Option func(*Provider)

// WithCommand overrides the executable name or path.
func WithCommand(cmd string) Option {
	return func(p *Provider) {
		if cmd != "" {
			p.command = cmd
		}
	}
}

// WithPermissionMode overrides how tool use is gated.
//
// bypassPermissions removes the gate entirely. That is a deliberate choice for a trusted
// project, never a default: agents run as the user, and the worktree is the blast radius.
func WithPermissionMode(mode string) Option {
	return func(p *Provider) {
		if mode != "" {
			p.permissionMode = mode
		}
	}
}

// WithSessionIDFunc overrides session id generation.
func WithSessionIDFunc(f func() string) Option {
	return func(p *Provider) { p.newSessionID = f }
}

// New returns the claude-code provider.
func New(opts ...Option) *Provider {
	p := &Provider{
		command:        DefaultCommand,
		permissionMode: DefaultPermissionMode,
		newSessionID:   newUUID,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// ID returns the provider id.
func (p *Provider) ID() string { return ID }

// Models lists the models Gravy will route to.
//
// The CLI accepts aliases as well as full model names, and aliases track the newest model in
// each tier, which is what a route wants.
func (p *Provider) Models(context.Context) ([]provider.Model, error) {
	return []provider.Model{
		{ID: "haiku", Name: "Claude Haiku"},
		{ID: "sonnet", Name: "Claude Sonnet"},
		{ID: "opus", Name: "Claude Opus"},
		{ID: "fable", Name: "Claude Fable"},
	}, nil
}

// Detect reports whether the CLI is installed and appears authenticated.
//
// Authentication is checked without prompting: --print never opens an interactive login, so a
// probe either answers or fails with something diagnosable.
func (p *Provider) Detect(ctx context.Context, h host.Host) (provider.Availability, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	version, code, _, stderr, err := p.capture(ctx, h, "", 20*time.Second, "--version")
	if err != nil || code != 0 {
		detail := fmt.Sprintf("%s was not found on PATH. Install Claude Code, then run `%s login`.",
			p.command, p.command)
		if err == nil && strings.TrimSpace(stderr) != "" {
			detail = strings.TrimSpace(stderr)
		}
		return provider.Availability{Installed: false, Detail: detail}, nil //nolint:nilerr // absence is a state, not an error
	}

	av := provider.Availability{Installed: true, Version: firstLine(version)}

	// A trivial print run is the only honest authentication check: the CLI resolves
	// credentials from several sources and the result is what matters, not where it came from.
	out, code, _, stderr, err := p.capture(ctx, h, "", 25*time.Second,
		"-p", "Reply with the single word: ok", "--output-format", "json")
	if err != nil {
		av.Detail = fmt.Sprintf("could not probe authentication: %v", err)
		return av, nil //nolint:nilerr // report rather than fail detection
	}

	class := p.Classify(code, out, stderr)
	switch {
	case code == 0 && !resultIsError(out):
		av.Authenticated = true
		av.Detail = "authenticated"
	case class.Class == provider.AuthExpired:
		av.Detail = fmt.Sprintf("not authenticated: run `%s login`", p.command)
	default:
		av.Detail = fmt.Sprintf("installed but the probe failed: %s", class.Note())
	}
	return av, nil
}

// resultIsError reports whether a --output-format json result says the run failed.
//
// The subtype field is NOT consulted: a hard 404 and a 401 were both observed arriving as
// subtype "success" alongside is_error true (docs/SPIKE-claude-code.md, F1). Keying off subtype
// would report a failed run as a successful one.
func resultIsError(jsonOut string) bool {
	var r struct {
		IsError bool `json:"is_error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonOut)), &r); err != nil {
		return false
	}
	return r.IsError
}

// Run starts an agent on a ticket.
func (p *Provider) Run(ctx context.Context, h host.Host, t provider.AgentTask) (provider.Handle, error) {
	if t.WorktreePath == "" {
		return nil, errors.New("claude-code: no worktree path")
	}
	sessionID := p.newSessionID()

	args, cleanup, err := p.runArgs(t, sessionID)
	if err != nil {
		return nil, err
	}
	return p.launch(ctx, h, t, args, sessionID, cleanup)
}

// Resume continues a prior session with an injected message.
//
// The session is resumed rather than restarted, so the agent keeps its context: verified in the
// spike, where a separate later process correctly recalled which file it had edited and with
// which tool.
func (p *Provider) Resume(ctx context.Context, h host.Host, s provider.SessionRef, msg string) (provider.Handle, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("claude-code: cannot resume invalid session %+v", s)
	}
	if s.ProviderID != ID {
		return nil, fmt.Errorf("claude-code: session belongs to provider %q", s.ProviderID)
	}
	args := []string{
		"-p", msg,
		"--resume", s.ID,
		"--output-format", "stream-json",
		"--verbose",
	}
	if p.permissionMode != "" {
		args = append(args, "--permission-mode", p.permissionMode)
	}
	return p.launch(ctx, h, provider.AgentTask{}, args, s.ID, nil)
}

// allowedTools renders an allowlist as --allowedTools patterns.
//
// Without this an agent cannot run the very commands its work is judged by: acceptEdits permits
// file edits but gates shell, so `go test ./...` is denied, the agent reports that it needs
// approval, and the run is classified a task failure despite the code being correct. It then
// burns a self-correction retry doing the same thing again at twice the cost.
//
// Each command is granted twice: exactly as written, and with a trailing wildcard so the agent
// may append flags (`go test ./... -run TestThing`). That is deliberately narrow — a pattern
// like `go *` would permit `go run` on anything. GR-035 replaces this with real per-project
// matching and an escalation path.
func allowedTools(a core.Allowlist) []string {
	var out []string
	for _, pattern := range a.Commands {
		cmd := strings.TrimSpace(pattern.Match)
		if cmd == "" {
			continue
		}
		out = append(out, fmt.Sprintf("Bash(%s)", cmd), fmt.Sprintf("Bash(%s *)", cmd))
	}
	return out
}

// runArgs builds the CLI invocation for a task, plus a cleanup for any temporary files.
func (p *Provider) runArgs(t provider.AgentTask, sessionID string) ([]string, func(), error) {
	args := []string{
		"-p", t.Prompt,
		// Gravy assigns the session id rather than scraping it from output. The spike
		// confirmed --session-id is honoured and that --resume returns the same id, so there
		// is no reconciliation step and no parsing to get wrong.
		"--session-id", sessionID,
		"--output-format", "stream-json",
		"--verbose",
	}
	if p.permissionMode != "" {
		args = append(args, "--permission-mode", p.permissionMode)
	}
	if allowed := allowedTools(t.Allowlist); len(allowed) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, allowed...)
	}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if t.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprint(t.MaxTurns))
	}

	// The permission hook is supplied per run through --settings, which accepts a file path.
	// Writing it to the run directory rather than into the repository keeps the promise that
	// Gravy writes nothing into a working tree.
	settingsPath, cleanup, err := writeSettings(t)
	if err != nil {
		return nil, nil, err
	}
	if settingsPath != "" {
		args = append(args, "--settings", settingsPath)
	}
	return args, cleanup, nil
}

// launch starts the CLI and wires up event parsing.
func (p *Provider) launch(ctx context.Context, h host.Host, t provider.AgentTask, args []string, sessionID string, cleanup func()) (provider.Handle, error) {
	proc, err := h.Exec(ctx, host.ExecSpec{
		Cmd:     p.command,
		Args:    args,
		Dir:     t.WorktreePath,
		Timeout: t.Timeout,
		Env: map[string]string{
			// The CLI must never wait for a human it cannot reach.
			"CLAUDE_CODE_NONINTERACTIVE": "1",
		},
	})
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, fmt.Errorf("claude-code: start: %w", err)
	}

	hd := &handle{
		proc:      proc,
		events:    make(chan provider.Event, 64),
		done:      make(chan struct{}),
		sessionID: sessionID,
		provider:  p,
		logPath:   t.LogPath,
		cleanup:   cleanup,
	}
	go hd.run()
	return hd, nil
}

// handle is a running claude process.
type handle struct {
	proc      host.Process
	events    chan provider.Event
	done      chan struct{}
	sessionID string
	provider  *Provider
	logPath   string
	cleanup   func()

	mu      sync.Mutex
	outcome provider.Outcome
	err     error
}

// Events streams parsed progress. It is closed when the run ends.
func (h *handle) Events() <-chan provider.Event { return h.events }

// Kill terminates the run and everything it started.
func (h *handle) Kill() error { return h.proc.Kill() }

// Wait blocks until the run finishes and reports its outcome.
func (h *handle) Wait() (provider.Outcome, error) {
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.outcome, h.err
}

// run consumes the process's output, emitting events as they arrive, and computes the outcome.
func (h *handle) run() {
	defer close(h.done)
	defer close(h.events)
	if h.cleanup != nil {
		defer h.cleanup()
	}

	// The raw stream is kept verbatim so a parsing gap is diagnosable after the fact.
	var logFile *os.File
	if h.logPath != "" {
		if err := os.MkdirAll(filepath.Dir(h.logPath), 0o755); err == nil {
			if f, err := os.Create(h.logPath); err == nil {
				logFile = f
				defer f.Close()
			}
		}
	}

	stderrCh := readAll(h.proc.Stderr())

	var (
		result   *streamEvent
		stdoutSB strings.Builder
	)

	scanner := bufio.NewScanner(h.proc.Stdout())
	// Individual events can be large; the default 64KB limit truncates them.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if logFile != nil {
			fmt.Fprintln(logFile, line)
		}
		stdoutSB.WriteString(line)
		stdoutSB.WriteByte('\n')

		for _, e := range parseLine(line) {
			select {
			case h.events <- e:
			default:
				// A slow consumer must not stall the run. Dropping a display event is
				// preferable to blocking the agent; the raw log keeps everything.
			}
		}
		if se := terminalResult(line); se != nil {
			result = se
		}
	}

	status, waitErr := h.proc.Wait()
	stderr := <-stderrCh

	h.mu.Lock()
	defer h.mu.Unlock()

	if waitErr != nil {
		h.err = fmt.Errorf("claude-code: %w", waitErr)
		return
	}
	h.outcome = computeOutcome(h.provider, status, result, stdoutSB.String(), stderr, h.sessionID)
}

// computeOutcome turns a finished process into an Outcome.
//
// It is separated from run so that the judgement can be tested without a live process. That
// matters more here than anywhere else in the adapter: this function is where a CLI that
// misreports its own success gets corrected, and every correction it makes is invisible to an
// integration test that does not reproduce the exact failure.
func computeOutcome(p *Provider, status host.ExitStatus, result *streamEvent, stdout, stderr, sessionID string) provider.Outcome {
	out := provider.Outcome{
		ExitCode: status.Code,
		TimedOut: status.TimedOut,
		Session:  provider.SessionRef{ProviderID: ID, ID: sessionID},
	}

	if status.TimedOut {
		out.Class = provider.Timeout
		out.Note = fmt.Sprintf("killed after %s without completing", status.Duration.Round(time.Second))
		return out
	}

	if result != nil {
		for _, d := range result.PermissionDenials {
			out.Denials = append(out.Denials, provider.PermissionDenial{
				Tool:  d.ToolName,
				Input: d.ToolInput,
			})
		}
		out.Turns = result.NumTurns
		out.TokensIn = result.Usage.InputTokens + result.Usage.CacheReadInputTokens + result.Usage.CacheCreationInputTokens
		out.TokensOut = result.Usage.OutputTokens
		out.CostUSD = result.TotalCostUSD
		if result.SessionID != "" {
			out.Session.ID = result.SessionID
		}
	}

	// Classification reads the process's exit code and output, not the result object's
	// subtype, which was observed reporting "success" on hard API errors.
	//
	// The result object's own diagnosis is folded into the classified text. The CLI puts its
	// human-readable error there ("Failed to authenticate. API Error: 401 ..."), and relying
	// on that string also appearing in the raw stream is a bet, not a guarantee. Losing it
	// would classify an expired credential as an ordinary bad run: the ticket would burn its
	// self-correction budget on retries that cannot succeed, and provider_auth would never
	// reach Needs You.
	class := p.Classify(status.Code, stdout, stderr+resultDiagnosis(result))

	// A run the CLI reports as finished successfully is never a provider-side failure.
	//
	// Classification matches text against the whole stream, which carries everything the agent
	// read and wrote — so a file mentioning a quota, a rate limit or an outage can trip a rule
	// that is looking for the provider's own error. Exit zero and is_error false is the CLI
	// stating that the request went through; whatever the text resembles, no quota condition
	// happened. Getting this wrong costs the run, the model's availability, and — because a
	// cooldown is fleet-wide — every other ticket routed to it.
	if status.Code == 0 && result != nil && !result.IsError && class.Class.IsQuotaCondition() {
		class = provider.Classification{
			Class:    provider.Success,
			Rule:     "clean exit outranks a quota match in the transcript",
			Evidence: class.Rule,
		}
	}

	// A result object claiming failure overrides a zero exit code. is_error is authoritative;
	// subtype is not.
	if class.Class == provider.Success && result != nil && result.IsError {
		class = provider.Classification{
			Class:    provider.TaskFailure,
			Rule:     "result.is_error",
			Evidence: truncate(result.Result, 200),
		}
	}

	// A run whose tool calls were refused has not done its work, whatever it claims.
	//
	// Observed from the real CLI: exit 0, is_error false, subtype "success", and a
	// permission_denials array containing the single Write the task required. Reporting that
	// as Success would send an empty diff to review described as "no changes" — the agent was
	// not idle, it was blocked, and nothing in the success fields says so.
	//
	// GR-035 turns this into a proper escalation with an allowlist prompt. Until then it is a
	// task failure, which is visible and retried, rather than a silent success.
	if class.Class == provider.Success && len(out.Denials) > 0 {
		class = provider.Classification{
			Class:    provider.TaskFailure,
			Rule:     "permission denied",
			Evidence: denialSummary(out.Denials),
		}
	}

	out.Class = class.Class
	out.Note = class.Note()
	return out
}

// resultDiagnosis renders the result object's error fields as text for classification.
//
// Returns empty for a run with no result or nothing wrong, so a healthy run's classification is
// unaffected.
func resultDiagnosis(result *streamEvent) string {
	if result == nil {
		return ""
	}
	var b strings.Builder
	if result.Result != "" {
		b.WriteString("\n")
		b.WriteString(result.Result)
	}
	if result.APIErrorStatus != nil {
		fmt.Fprintf(&b, "\napi_error_status: %d", *result.APIErrorStatus)
	}
	if result.TerminalReason != "" {
		b.WriteString("\nterminal_reason: ")
		b.WriteString(result.TerminalReason)
	}
	return b.String()
}

// denialSummary renders refused tool calls for the run's failure note.
func denialSummary(denials []provider.PermissionDenial) string {
	parts := make([]string, 0, len(denials))
	for _, d := range denials {
		parts = append(parts, d.Summary())
	}
	return truncate(strings.Join(parts, "; "), 200)
}

// terminalResult returns the parsed result event for a line, or nil.
func terminalResult(line string) *streamEvent {
	if !strings.Contains(line, `"type":"result"`) && !strings.Contains(line, `"type": "result"`) {
		return nil
	}
	var se streamEvent
	if err := json.Unmarshal([]byte(line), &se); err != nil || se.Type != "result" {
		return nil
	}
	return &se
}

func readAll(r io.Reader) <-chan string {
	ch := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		ch <- string(b)
	}()
	return ch
}

// capture runs the CLI to completion and returns its output. Used for probes, never for runs.
func (p *Provider) capture(ctx context.Context, h host.Host, dir string, timeout time.Duration, args ...string) (stdout string, code int, duration time.Duration, stderr string, err error) {
	proc, err := h.Exec(ctx, host.ExecSpec{Cmd: p.command, Args: args, Dir: dir, Timeout: timeout})
	if err != nil {
		return "", 0, 0, "", err
	}
	outCh, errCh := readAll(proc.Stdout()), readAll(proc.Stderr())
	status, waitErr := proc.Wait()
	out, errOut := <-outCh, <-errCh
	if waitErr != nil {
		return out, status.Code, status.Duration, errOut, waitErr
	}
	return out, status.Code, status.Duration, errOut, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

var _ provider.Provider = (*Provider)(nil)
