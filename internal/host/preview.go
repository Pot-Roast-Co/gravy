package host

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// previewCommands resolves every service before any process is started.
func previewCommands(root string, services []core.PreviewService) ([]*exec.Cmd, error) {
	if err := core.ValidatePreviewServices(services); err != nil {
		return nil, err
	}
	var commands []*exec.Cmd
	for _, service := range services {
		dir := filepath.Join(root, service.Dir)
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil, fmt.Errorf("%s directory: %w", service.Name, err)
		}
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(resolvedRoot, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%s directory is outside the ticket worktree", service.Name)
		}
		script := `exec "$1" -c "$2"`
		args := []string{"-c", script, "gravy-preview", Shell(), service.Command}
		if service.EnvFile != "" {
			envFile := service.EnvFile
			if !filepath.IsAbs(envFile) {
				envFile = filepath.Join(dir, envFile)
			}
			file, err := os.Open(envFile)
			if err != nil {
				return nil, fmt.Errorf("%s environment file: %w", service.Name, err)
			}
			info, err := file.Stat()
			_ = file.Close()
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s environment file must be a regular file", service.Name)
			}
			script = `set -a; . "$3" || exit; exec "$1" -c "$2"`
			args = []string{"-c", script, "gravy-preview", Shell(), service.Command, envFile}
		}
		cmd := exec.Command(Shell(), args...) //nolint:gosec // explicit user-configured preview command
		cmd.Dir = dir
		setProcessGroup(cmd)
		commands = append(commands, cmd)
	}
	return commands, nil
}

// runServices starts all configured services, stopping the entire preview when one exits.
func (r *TerminalRun) runServices(ctx context.Context) error {
	commands, err := previewCommands(r.Dir, r.Services)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	var mu sync.Mutex
	type result struct {
		name string
		err  error
	}
	done := make(chan result, len(commands))
	var started []*exec.Cmd
	defer func() {
		for _, cmd := range started {
			_ = terminateGroup(cmd.Process.Pid)
		}
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		remaining := len(started)
	draining:
		for remaining > 0 {
			select {
			case <-done:
				remaining--
			case <-timer.C:
				break draining
			}
		}
		// Always reach descendants, even when their parent has already exited.
		for _, cmd := range started {
			_ = killGroup(cmd.Process.Pid)
		}
		for ; remaining > 0; remaining-- {
			<-done
		}
	}()
	for i, cmd := range commands {
		name := r.Services[i].Name
		cmd.Stdout = &previewWriter{mu: &mu, out: r.stdout, name: name, atStart: true}
		cmd.Stderr = &previewWriter{mu: &mu, out: r.stderr, name: name, atStart: true}
		cmd.WaitDelay = 2 * time.Second
		// No shared terminal input: services must run unattended while the user tests the app.
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start %s: %w", name, err)
		}
		started = append(started, cmd)
		go func() { done <- result{name, cmd.Wait()} }()
	}
	select {
	case <-ctx.Done():
		return ErrInterrupted
	case result := <-done:
		// Leave one completion per started process for the cleanup above.
		done <- result
		if result.err != nil {
			return fmt.Errorf("%s: %w", result.name, result.err)
		}
		return fmt.Errorf("%s exited; stopped the other preview services", result.name)
	}
}

// previewWriter serializes output from all services and labels each line.
type previewWriter struct {
	mu      *sync.Mutex
	out     io.Writer
	name    string
	atStart bool
}

func (w *previewWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, part := range strings.SplitAfter(string(p), "\n") {
		if part == "" {
			continue
		}
		if w.atStart {
			if _, err := fmt.Fprintf(w.out, "[%s] ", w.name); err != nil {
				return 0, err
			}
		}
		if _, err := io.WriteString(w.out, part); err != nil {
			return 0, err
		}
		w.atStart = strings.HasSuffix(part, "\n")
	}
	return len(p), nil
}
