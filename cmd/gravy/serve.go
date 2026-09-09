package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/daemon"
	"github.com/pot-roast-co/gravy/internal/host"
)

// runServe is the daemon: it owns the socket, the scheduler loop and the database.
func runServe(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("serve takes no arguments")
	}

	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Retention runs on the way up rather than on a timer: a daemon that is never restarted is
	// one whose disk was never a problem.
	if keep := a.cfg.Retention.RunLogs.D(); keep > 0 {
		n, err := a.logs.Prune(time.Now().Add(-keep), nil)
		switch {
		case err != nil:
			log.Warn("could not prune old run logs", "error", err)
		case n > 0:
			log.Info("pruned old run logs", "count", n, "older_than", keep)
		}
	}

	d := daemon.New(a.home, a.svc, a.loop(), a.db, newID, log).WithNotifier(a.notifier)
	return d.Run(ctx)
}

// connect returns a service backed by the daemon, starting one if none is running.
//
// Clients talk to the daemon rather than opening the database themselves: it is the single
// writer, and a second scheduler in another process would happily start a ticket the first one
// had already claimed.
func connect(ctx context.Context, home string, h host.Host, autostart bool) (*api.Client, error) {
	socket := daemon.SocketPath(home)

	if c, err := api.Dial(socket); err == nil {
		return c, nil
	}
	if !autostart {
		return nil, fmt.Errorf("no gravy daemon is running — start one with `gravy serve`")
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate the gravy binary: %w", err)
	}
	if _, err := h.StartDetached(host.ExecSpec{Cmd: exe, Args: []string{"serve"}}, daemon.LogPath(home)); err != nil {
		return nil, fmt.Errorf("start the daemon: %w", err)
	}

	// Binding a socket takes milliseconds; polling beats a fixed sleep that is either a stall
	// or a race depending on the machine.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := api.Dial(socket); err == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("the daemon did not come up within 5s — see %s", daemon.LogPath(home))
}

// runStop shuts the daemon down.
func runStop(args []string) error {
	fs := flag.NewFlagSet("gravy stop", flag.ContinueOnError)
	wait := fs.Duration("wait", 10*time.Second, "how long to wait for the daemon to exit")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gravy stop [flags]")
		fmt.Fprintln(os.Stderr, "\nStops the daemon for this Gravy home. Safe to run twice.")
		fmt.Fprintln(os.Stderr, "\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	home, err := config.Home()
	if err != nil {
		return err
	}

	res, err := daemon.Stop(home, *wait)
	if err != nil {
		return err
	}

	// Each outcome gets its own sentence. "stopped" when nothing was running, or when a stale
	// pidfile was cleared, would be a small lie that costs someone an hour the day a daemon
	// does not actually go down.
	switch res.Outcome {
	case daemon.StoppedNothing:
		fmt.Println("no gravy daemon is running")
	case daemon.StoppedStale:
		fmt.Println("no gravy daemon is running (cleared a stale pidfile)")
	case daemon.Stopped:
		fmt.Printf("stopped the gravy daemon (pid %d)\n", res.PID)
	case daemon.StoppedSlow:
		fmt.Printf("asked the gravy daemon to stop (pid %d); it has not exited yet\n", res.PID)
	}

	if res.Outcome == daemon.Stopped || res.Outcome == daemon.StoppedSlow {
		fmt.Println("agents it started are not killed; running gravy again starts a new daemon")
	}
	return nil
}
