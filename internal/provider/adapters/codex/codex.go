// Package codex drives the Codex CLI as a managed, non-interactive worker.
//
// Every flag used here was verified against codex-cli 0.152.1; the captured event stream and the
// observed failure output are in docs/fixtures/codex/.
//
// Gravy drives the user's existing authenticated CLI. It does not want credentials, does not
// store them, and never touches the OpenAI API directly.
package codex

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
)

// ID is the provider's registry key.
const ID = "codex"

// DefaultCommand is the executable name, looked up on PATH.
const DefaultCommand = "codex"

// DefaultSandbox is how tool use is gated in the absence of the permission broker.
//
// workspace-write lets the agent edit and run commands inside the directory it was given, which
// is the worktree, and denies everything outside it. That is as close to the product's default
// allowlist — read and write anywhere in the worktree, everything else escalates — as this CLI's
// flags can express.
const DefaultSandbox = "workspace-write"

// Provider implements provider.Provider over the codex CLI.
type Provider struct {
	command string
	sandbox string
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

// WithSandbox overrides the sandbox policy.
//
// danger-full-access removes the gate entirely. That is a deliberate choice for a trusted
// project, never a default: agents run as the user, and the worktree is the blast radius.
func WithSandbox(mode string) Option {
	return func(p *Provider) {
		if mode != "" {
			p.sandbox = mode
		}
	}
}

// New returns the codex provider.
func New(opts ...Option) *Provider {
	p := &Provider{command: DefaultCommand, sandbox: DefaultSandbox}
	for _, o := range opts {
		o(p)
	}
	return p
}

// ID returns the provider id.
func (p *Provider) ID() string { return ID }

// DefaultModel is the sentinel meaning "whatever your Codex login provides".
//
// It is not a model name and is never sent to the CLI; see Models.
const DefaultModel = "default"

// Models reports what this provider can be routed to, which is deliberately almost nothing.
//
// The CLI does not enumerate models, and a ChatGPT-account login rejects explicit ones outright:
// both `gpt-5-codex` and `gpt-5` were observed returning
//
//	status 400: The '<name>' model is not supported when using Codex with a ChatGPT account.
//
// while the same prompt with no --model succeeded. So the honest answer is one entry meaning
// "the account's default", rather than a list of names invented from the outside that fail at
// run time. A user on an API-key login can still name a model in their route; it is passed
// through untouched.
func (p *Provider) Models(context.Context) ([]provider.Model, error) {
	return []provider.Model{
		{ID: DefaultModel, Name: "Codex default (chosen by your Codex login)"},
	}, nil
}

// Detect reports whether the CLI is installed and authenticated.
//
// Authentication is read from `codex login status`, which answers without spending a model call
// — unlike claude-code, where a trivial run is the only honest probe. Observed: exit 0 and
// "Logged in using ChatGPT" on an authenticated machine.
func (p *Provider) Detect(ctx context.Context, h host.Host) (provider.Availability, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	version, code, stderr, err := p.capture(ctx, h, 20*time.Second, "--version")
	if err != nil || code != 0 {
		detail := fmt.Sprintf("%s was not found on PATH. Install Codex, then run `%s login`.",
			p.command, p.command)
		if err == nil && strings.TrimSpace(stderr) != "" {
			detail = strings.TrimSpace(stderr)
		}
		return provider.Availability{Installed: false, Detail: detail}, nil //nolint:nilerr // absence is a state, not an error
	}

	av := provider.Availability{Installed: true, Version: firstLine(version)}

	out, code, stderr, err := p.capture(ctx, h, 20*time.Second, "login", "status")
	if err != nil {
		av.Detail = fmt.Sprintf("could not probe authentication: %v", err)
		return av, nil //nolint:nilerr // report rather than fail detection
	}
	// `codex login status` was observed answering on stderr, not stdout, so both are read.
	// Taking the detail from stdout alone left it empty on an authenticated machine.
	combined := strings.TrimSpace(out + "\n" + stderr)
	switch {
	case code == 0 && strings.Contains(strings.ToLower(combined), "logged in"):
		av.Authenticated = true
		av.Detail = firstNonEmptyLine(combined)
	default:
		av.Detail = fmt.Sprintf("not authenticated: run `%s login`", p.command)
	}
	return av, nil
}

// Run starts an agent on a ticket.
func (p *Provider) Run(ctx context.Context, h host.Host, t provider.AgentTask) (provider.Handle, error) {
	if t.WorktreePath == "" {
		return nil, errors.New("codex: no worktree path")
	}
	return p.launch(ctx, h, t, p.runArgs(t))
}

// runArgs builds the CLI invocation for a task.
//
// Two of AgentTask's fields have no equivalent here and are honestly ignored rather than
// emulated (GR-013 AC2):
//
//   - MaxTurns: `codex exec` has no turn cap. The wall-clock timeout is the only bound, which
//     host.Exec already applies.
//   - Allowlist: codex gates by sandbox policy over a directory, not by command pattern, so a
//     per-command allowlist cannot be expressed. workspace-write is the closest equivalent and
//     the worktree is the boundary.
func (p *Provider) runArgs(t provider.AgentTask) []string {
	args := []string{
		"exec",
		"--json",
		// The worktree is a checkout, not a repository root, and codex refuses to run outside
		// what it recognises as a git repo without this.
		"--skip-git-repo-check",
		"-C", t.WorktreePath,
		"-s", p.sandbox,
	}
	// The sentinel means "do not pass --model", which is the only thing that works on a
	// ChatGPT-account login. Any other value is the user's explicit choice and is passed
	// through, since an API-key login can select models this one cannot.
	if t.Model != "" && t.Model != DefaultModel {
		args = append(args, "-m", t.Model)
	}
	// The prompt goes last as a positional argument. Passing it on stdin would work too, but
	// an argument keeps the invocation visible in the run log.
	return append(args, t.Prompt)
}

// Resume continues a prior thread with an injected message.
func (p *Provider) Resume(ctx context.Context, h host.Host, s provider.SessionRef, msg string) (provider.Handle, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("codex: cannot resume invalid session %+v", s)
	}
	if s.ProviderID != ID {
		return nil, fmt.Errorf("codex: session belongs to provider %q", s.ProviderID)
	}
	args := []string{"exec", "resume", s.ID, "--json", "--skip-git-repo-check", "-s", p.sandbox, msg}
	return p.launch(ctx, h, provider.AgentTask{}, args)
}

// launch starts the CLI and wires up event parsing.
func (p *Provider) launch(ctx context.Context, h host.Host, t provider.AgentTask, args []string) (provider.Handle, error) {
	proc, err := h.Exec(ctx, host.ExecSpec{
		Cmd:     p.command,
		Args:    args,
		Dir:     t.WorktreePath,
		Timeout: t.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("codex: start: %w", err)
	}

	hd := &handle{
		proc:     proc,
		events:   make(chan provider.Event, 64),
		done:     make(chan struct{}),
		provider: p,
		logPath:  t.LogPath,
	}
	// A resumed run already knows its thread; a fresh one learns it from thread.started.
	if t.WorktreePath == "" && len(args) > 2 {
		hd.threadID = args[2]
	}
	go hd.run()
	return hd, nil
}

// handle is a running codex process.
type handle struct {
	proc     host.Process
	events   chan provider.Event
	done     chan struct{}
	provider *Provider
	logPath  string

	mu       sync.Mutex
	threadID string
	outcome  provider.Outcome
	err      error
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

func (h *handle) run() {
	defer close(h.done)
	defer close(h.events)

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
		stdoutSB strings.Builder
		usage    *usageTotals
	)

	scanner := bufio.NewScanner(h.proc.Stdout())
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if logFile != nil {
			fmt.Fprintln(logFile, line)
		}
		stdoutSB.WriteString(line)
		stdoutSB.WriteByte('\n')

		if id := threadIDFrom(line); id != "" {
			h.mu.Lock()
			h.threadID = id
			h.mu.Unlock()
		}
		if u := usageFrom(line); u != nil {
			usage = u
		}

		for _, e := range parseLine(line) {
			select {
			case h.events <- e:
			default:
				// A slow consumer must not stall the run. Dropping a display event is
				// preferable to blocking the agent; the raw log keeps everything.
			}
		}
	}

	status, waitErr := h.proc.Wait()
	stderr := <-stderrCh

	h.mu.Lock()
	defer h.mu.Unlock()

	if waitErr != nil {
		h.err = fmt.Errorf("codex: %w", waitErr)
		return
	}
	h.outcome = computeOutcome(h.provider, status, usage, stdoutSB.String(), stderr, h.threadID)
}

func readAll(r interface{ Read([]byte) (int, error) }) <-chan string {
	ch := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		ch <- sb.String()
	}()
	return ch
}

// capture runs the CLI to completion and returns its output. Used for probes, never for runs.
func (p *Provider) capture(ctx context.Context, h host.Host, timeout time.Duration, args ...string) (stdout string, code int, stderr string, err error) {
	proc, err := h.Exec(ctx, host.ExecSpec{Cmd: p.command, Args: args, Timeout: timeout})
	if err != nil {
		return "", 0, "", err
	}
	outCh, errCh := readAll(proc.Stdout()), readAll(proc.Stderr())
	status, waitErr := proc.Wait()
	out, errOut := <-outCh, <-errCh
	if waitErr != nil {
		return out, status.Code, errOut, waitErr
	}
	return out, status.Code, errOut, nil
}

// firstNonEmptyLine is used for detail text, where an empty answer would be worse than none:
// Availability.Detail is shown in onboarding and in Needs You and has to say something.
func firstNonEmptyLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln
		}
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
