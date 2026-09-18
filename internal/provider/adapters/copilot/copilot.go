// Package copilot drives GitHub Copilot CLI in programmatic mode.
package copilot

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
)

// ID is the provider registry key.
const ID = "copilot"

// DefaultCommand is the standalone GitHub Copilot CLI, not the old gh extension.
const DefaultCommand = "copilot"

// DefaultModel lets the CLI use the model selected by the user's configuration.
const DefaultModel = "default"

// Provider implements provider.Provider over GitHub Copilot CLI.
type Provider struct{ command string }

// Option configures the adapter.
type Option func(*Provider)

// WithCommand overrides the executable name or path.
func WithCommand(command string) Option {
	return func(p *Provider) {
		if command != "" {
			p.command = command
		}
	}
}

// New returns a Copilot adapter.
func New(opts ...Option) *Provider {
	p := &Provider{command: DefaultCommand}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// ID identifies this provider.
func (p *Provider) ID() string { return ID }

// Models offers a stable default; explicit model IDs are also supported.
func (p *Provider) Models(context.Context) ([]provider.Model, error) {
	return []provider.Model{{ID: DefaultModel, Name: "Copilot default (configured in Copilot CLI)"}}, nil
}

// ModelsAreOpen lets users select models their Copilot account supports.
func (p *Provider) ModelsAreOpen() bool { return true }

// Detect asks the CLI's SDK protocol for authentication status. No model request is made.
func (p *Provider) Detect(ctx context.Context, h host.Host) (provider.Availability, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	proc, err := h.Exec(ctx, host.ExecSpec{Cmd: p.command, Args: []string{"--version"}, Timeout: 10 * time.Second})
	if err != nil {
		return provider.Availability{Detail: "Install GitHub Copilot CLI, then run `copilot login`."}, nil
	}
	out, stderr := readAll(proc.Stdout()), readAll(proc.Stderr())
	status, waitErr := proc.Wait()
	version, detail := <-out, <-stderr
	if waitErr != nil || status.Code != 0 || status.TimedOut {
		return provider.Availability{Detail: "Copilot version check failed: " + strings.TrimSpace(detail)}, nil
	}
	av := provider.Availability{Installed: true, Version: strings.Split(strings.TrimSpace(version), "\n")[0]}
	authenticated, err := p.authenticated(ctx, h)
	if err != nil {
		av.Detail = fmt.Sprintf("could not check Copilot login: %v; update Copilot CLI and run `copilot login`", err)
		return av, nil
	}
	av.Authenticated = authenticated
	if authenticated {
		av.Detail = "authenticated; model access depends on your Copilot account"
	} else {
		av.Detail = "not authenticated: run `copilot login`"
	}
	return av, nil
}

func (p *Provider) authenticated(ctx context.Context, h host.Host) (bool, error) {
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	proc, err := h.Exec(ctx, host.ExecSpec{Cmd: p.command, Args: []string{"--headless", "--stdio", "--no-auto-update"}, Stdin: input, Timeout: 20 * time.Second})
	if err != nil {
		return false, err
	}
	stderr := readAll(proc.Stderr())
	defer func() { _ = proc.Kill(); _ = writer.Close(); _, _ = proc.Wait(); <-stderr }()
	// Copilot's SDK uses vscode-jsonrpc framing, not newline-delimited JSON.
	request := `{"jsonrpc":"2.0","id":1,"method":"auth.getStatus","params":{}}`
	go func() { _, _ = fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n%s", len(request), request) }()
	reader := bufio.NewReader(proc.Stdout())
	for {
		raw, err := readRPC(reader)
		if err != nil {
			return false, err
		}
		var response struct {
			ID     int `json:"id"`
			Result struct {
				Authenticated bool `json:"isAuthenticated"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			return false, err
		}
		if response.ID != 1 {
			continue
		}
		if response.Error != nil {
			return false, errors.New(response.Error.Message)
		}
		return response.Result.Authenticated, nil
	}
}

func readRPC(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, err
			}
		}
	}
	if length < 0 || length > 8*1024*1024 {
		return nil, fmt.Errorf("invalid Copilot RPC content length %d", length)
	}
	raw := make([]byte, length)
	_, err := io.ReadFull(r, raw)
	return raw, err
}

// Run starts a task in its worktree.
func (p *Provider) Run(ctx context.Context, h host.Host, task provider.AgentTask) (provider.Handle, error) {
	if task.WorktreePath == "" {
		return nil, errors.New("copilot: no worktree path")
	}
	args, cleanup, err := runArgs(task)
	if err != nil {
		return nil, err
	}
	return p.launch(ctx, h, task, args, "", cleanup)
}

// Resume continues an explicit session with the same worktree and permission settings.
// CLI flag permissions are not persisted by Copilot, so the opaque reference carries them.
type sessionRef struct {
	ID   string
	Task provider.AgentTask
}

// Resume restores the context Gravy recorded with the session.
func (p *Provider) Resume(ctx context.Context, h host.Host, session provider.SessionRef, t provider.AgentTask) (provider.Handle, error) {
	if !session.Valid() || session.ProviderID != ID {
		return nil, fmt.Errorf("copilot: invalid session for this provider")
	}
	var ref sessionRef
	if err := json.Unmarshal([]byte(session.ID), &ref); err != nil || ref.ID == "" || ref.Task.WorktreePath == "" {
		return nil, fmt.Errorf("copilot: session is missing its saved worktree and permissions")
	}
	ref.Task.Prompt = t.Prompt
	args, cleanup, err := runArgs(ref.Task)
	if err != nil {
		return nil, err
	}
	args = append(args, "--resume="+ref.ID)
	return p.launch(ctx, h, ref.Task, args, ref.ID, cleanup)
}

func baseArgs() []string {
	return []string{"--output-format", "json", "--no-ask-user", "--no-auto-update", "--no-remote-export", "--no-color"}
}

// Copilot has no ordinary-turn cap; host.Exec enforces the wall-clock limit.
// Permission patterns that cannot be translated are refused instead of widened.
func runArgs(task provider.AgentTask) ([]string, func(), error) {
	args := append(baseArgs(), "--allow-tool=read", "--allow-tool=write")
	for _, rule := range task.Allowlist.Commands {
		command := strings.TrimSpace(rule.Match)
		if command == "" {
			continue
		}
		if strings.HasPrefix(command, "^") || strings.ContainsAny(command, "(),\r\n") {
			return nil, nil, fmt.Errorf("copilot: command permission %q cannot be expressed as a Copilot shell pattern", command)
		}
		args = append(args, "--allow-tool=shell("+command+")", "--allow-tool=shell("+command+" *)")
	}
	if task.Allowlist.Network {
		args = append(args, "--allow-all-urls")
	}
	if task.AskPath != "" {
		args = append(args, "--add-dir", filepath.Dir(task.AskPath))
	}
	if task.Model != "" && task.Model != DefaultModel {
		args = append(args, "--model", task.Model)
	}
	prompt, cleanup, err := promptArg(task)
	if err != nil {
		return nil, nil, err
	}
	return append(args, "-p", prompt), cleanup, nil
}

// promptThreshold is the size above which the prompt is handed over as a file.
//
// Comfortably under host.MaxArgLen, because -p is not the only argument: the allowlist becomes
// one --allow-tool per command, and the margin has to cover them.
const promptThreshold = 96 * 1024

// promptArg returns what to pass to -p, writing the prompt to a file when it is too large to be
// an argument at all.
//
// Copilot has no stdin mode — -p takes a value and its help mentions stdin nowhere — so the
// pipe that fixes this for claude-code and codex is not available. A file is, because gravy
// already grants Copilot directory access with --add-dir, and a path is a few hundred bytes
// whatever the prompt weighs.
//
// The alternative was to truncate, and that is the one thing this must not do: handing an agent
// two thirds of a ROADMAP and letting it plan confidently against the missing third is a failure
// that reports success.
func promptArg(task provider.AgentTask) (string, func(), error) {
	if len(task.Prompt) <= promptThreshold {
		return task.Prompt, nil, nil
	}

	dir := task.WorktreePath
	if task.LogPath != "" {
		// Beside the run's other artefacts rather than in the worktree, so it is never swept
		// into a commit — the same rule the claude-code settings file follows.
		dir = filepath.Dir(task.LogPath)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("copilot: prompt file directory: %w", err)
	}
	f, err := os.CreateTemp(dir, "prompt-*.md")
	if err != nil {
		return "", nil, fmt.Errorf("copilot: write the prompt: %w", err)
	}
	name := f.Name()
	if _, err := f.WriteString(task.Prompt); err != nil {
		f.Close()
		os.Remove(name)
		return "", nil, fmt.Errorf("copilot: write the prompt: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", nil, fmt.Errorf("copilot: write the prompt: %w", err)
	}

	return "Your instructions for this task are in " + name +
		". Read that file first and in full, then carry out what it says. It is the whole of " +
		"your brief; nothing else will be provided.", func() { os.Remove(name) }, nil
}

func (p *Provider) launch(ctx context.Context, h host.Host, task provider.AgentTask, args []string, session string, cleanup func()) (provider.Handle, error) {
	if task.AskPath != "" {
		if err := h.FS().MkdirAll(filepath.Dir(task.AskPath), 0700); err != nil {
			if cleanup != nil {
				cleanup()
			}
			return nil, fmt.Errorf("create Copilot ask directory: %w", err)
		}
	}
	proc, err := h.Exec(ctx, host.ExecSpec{Cmd: p.command, Args: args, Dir: task.WorktreePath, Timeout: task.Timeout, UnsetEnv: []string{"COPILOT_ALLOW_ALL"}})
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, fmt.Errorf("copilot: start: %w", err)
	}
	handle := &handle{proc: proc, events: make(chan provider.Event, 64), done: make(chan struct{}), session: session, cleanup: cleanup}
	go handle.run(p, task)
	return handle, nil
}

type handle struct {
	proc    host.Process
	events  chan provider.Event
	done    chan struct{}
	session string
	outcome provider.Outcome
	err     error
	// cleanup removes anything the invocation needed on disk — the prompt file, when the
	// prompt was too large to be an argument. It runs when the process is finished with it,
	// not when Run returns: Run returns a handle and the agent is still reading.
	cleanup func()
}

func (h *handle) Events() <-chan provider.Event   { return h.events }
func (h *handle) Kill() error                     { return h.proc.Kill() }
func (h *handle) Wait() (provider.Outcome, error) { <-h.done; return h.outcome, h.err }

func (h *handle) run(p *Provider, task provider.AgentTask) {
	logPath := task.LogPath
	// Last, so the prompt file outlives every read of it.
	if h.cleanup != nil {
		defer h.cleanup()
	}
	defer close(h.done)
	defer close(h.events)
	stderr := readAll(h.proc.Stderr())
	var logFile *os.File
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0700); err == nil {
			logFile, _ = os.Create(logPath)
			if logFile != nil {
				defer logFile.Close()
			}
		}
	}
	state := streamState{session: h.session, calls: map[string]provider.PermissionDenial{}}
	scanner := bufio.NewScanner(h.proc.Stdout())
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if logFile != nil {
			_, _ = fmt.Fprintln(logFile, line)
		}
		if event := state.parse(line); event != nil {
			select {
			case h.events <- *event:
			default:
			}
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		_ = h.proc.Kill()
	}
	status, err := h.proc.Wait()
	errText := <-stderr
	h.outcome = state.outcome(p, status, errText)
	if state.session != "" {
		task.Prompt, task.LogPath, task.RunID = "", "", ""
		ref, marshalErr := json.Marshal(sessionRef{ID: state.session, Task: task})
		if marshalErr != nil {
			h.err = marshalErr
		} else {
			h.outcome.Session.ID = string(ref)
		}
	}
	if err != nil {
		h.err = fmt.Errorf("copilot: wait: %w", err)
	}
	if scanErr != nil {
		h.err = fmt.Errorf("copilot: read stream: %w", scanErr)
		h.outcome.Class = provider.TaskFailure
	}
}

func readAll(r io.Reader) <-chan string {
	ch := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); ch <- string(b) }()
	return ch
}
