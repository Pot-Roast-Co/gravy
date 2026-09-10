package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/host"
)

type activation struct{ TicketID string }
type activationReply struct {
	Window string
	OK     bool
}

// Each TUI owns a private socket, separate from the daemon and scoped to GRAVY_HOME.
// A successful round trip proves the window is live; stale sockets cannot prevent fallback.
func listenActivation(home, window string, open func(string)) (func(), error) {
	dir := filepath.Join(home, "ui")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, strconv.Itoa(os.Getpid())+".sock")
	// Only this process's endpoint can be stale from PID reuse.
	if c, err := net.DialTimeout("unix", path, 100*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("TUI endpoint already active")
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		ln.Close()
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			var req activation
			if json.NewDecoder(io.LimitReader(c, 8192)).Decode(&req) == nil {
				open(req.TicketID)
				_ = json.NewEncoder(c).Encode(activationReply{Window: window, OK: true})
			}
			c.Close()
		}
	}()
	return func() { ln.Close() }, nil
}

func activateExisting(home, ticketID string) (activationReply, bool) {
	paths, _ := filepath.Glob(filepath.Join(home, "ui", "*.sock"))
	for _, path := range paths {
		c, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err != nil {
			continue
		}
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		var reply activationReply
		err = json.NewEncoder(c).Encode(activation{TicketID: ticketID})
		if err == nil {
			err = json.NewDecoder(io.LimitReader(c, 8192)).Decode(&reply)
		}
		c.Close()
		if err == nil && reply.OK {
			return reply, true
		}
	}
	return activationReply{}, false
}

func runActivate(ctx context.Context, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: gravy activate [ticket-id]")
	}
	home, err := config.Home()
	if err != nil {
		return err
	}
	id := ""
	if len(args) == 1 {
		id = args[0]
	}
	h := host.NewLocal("desktop", 1)
	if reply, ok := activateExisting(home, id); ok {
		if reply.Window != "" {
			return focusGravyWindow(ctx, hostRunner{h: h}, reply.Window)
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	argv := []string{"env", config.EnvHome + "=" + home, exe}
	if id != "" {
		argv = append(argv, "open", id)
	}
	command := "xdg-terminal-exec"
	// On Omarchy, let the desktop session own the terminal's lifetime rather than
	// inheriting the short-lived notification action's process scope.
	if h.FS().Exists("/usr/share/omarchy/bin/omarchy-launch-terminal") {
		command = "omarchy"
		argv = append([]string{"launch", "terminal"}, argv...)
	}
	_, err = h.StartDetached(host.ExecSpec{Cmd: command, Args: argv}, filepath.Join(home, "ui-launch.log"))
	return err
}

// terminalWindow matches the TUI's process ancestors to Hyprland's terminal PID.
// It avoids title matching, which can focus an unrelated shell or another Gravy home.
func terminalWindow(ctx context.Context, h host.Host) string {
	if os.Getenv("HYPRLAND_INSTANCE_SIGNATURE") == "" {
		return ""
	}
	p, err := h.Exec(ctx, host.ExecSpec{Cmd: "hyprctl", Args: []string{"clients", "-j"}, Timeout: 2 * time.Second})
	if err != nil {
		return ""
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, p.Stderr()); close(done) }()
	raw, _ := io.ReadAll(p.Stdout())
	st, err := p.Wait()
	<-done
	if err != nil || st.Code != 0 {
		return ""
	}
	var clients []struct {
		Address string
		PID     int `json:"pid"`
	}
	if json.Unmarshal(raw, &clients) != nil {
		return ""
	}
	for pid := os.Getpid(); pid > 1; {
		for _, client := range clients {
			if client.PID == pid {
				return client.Address
			}
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			break
		}
		end := strings.LastIndexByte(string(raw), ')')
		if end < 0 {
			break
		}
		fields := strings.Fields(string(raw[end+1:]))
		if len(fields) < 2 {
			break
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil || parent == pid {
			break
		}
		pid = parent
	}
	return ""
}

// focusGravyWindow supports both Lua and legacy Hyprland dispatchers.
func focusGravyWindow(ctx context.Context, runner interface {
	Run(context.Context, string, ...string) error
}, address string) error {
	if !strings.HasPrefix(address, "0x") {
		return fmt.Errorf("invalid window address")
	}
	if _, err := strconv.ParseUint(strings.TrimPrefix(address, "0x"), 16, 64); err != nil {
		return fmt.Errorf("invalid window address: %w", err)
	}
	if err := runner.Run(ctx, "hyprctl", "dispatch", "hl.dsp.focus({ window = \"address:"+address+"\" })"); err == nil {
		return nil
	}
	return runner.Run(ctx, "hyprctl", "dispatch", "focuswindow", "address:"+address)
}
