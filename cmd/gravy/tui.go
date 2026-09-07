package main

import (
	"context"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/tui"
)

// runTUI is the default command: the frame, against the in-process service.
//
// The TUI consumes api.Service, not a socket, so swapping in the JSON-RPC client when the daemon
// lands (GR-007) is a change to this function and nothing in internal/tui.
func runTUI(ctx context.Context) error {
	// Piped or redirected output has no frame to draw: say what the commands are instead of
	// failing on a TTY that was never going to exist.
	if !isTerminal(os.Stdout) {
		usage()
		return nil
	}

	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	// The TUI is a client of the daemon, not a second writer to the database. Starting one if
	// none is running is what makes `gravy` a single command rather than two terminals.
	c, err := connect(ctx, a.home, a.host, true)
	if err != nil {
		return err
	}
	defer c.Close()

	p := tea.NewProgram(tui.New(c), tea.WithAltScreen(), tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}

// isTerminal reports whether f is a character device, which is the stdlib's answer to "is this
// a terminal" and avoids a dependency for one bit of information.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
