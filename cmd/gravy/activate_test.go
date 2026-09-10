package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func activationHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gvui")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestActivateExistingReusesLiveEndpoint(t *testing.T) {
	home := activationHome(t)
	opened := make(chan string, 2)
	stop, err := listenActivation(home, "0x123", func(id string) { opened <- id })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for _, id := range []string{"first ticket", "second; $(false)"} {
		reply, ok := activateExisting(home, id)
		if !ok || reply.Window != "0x123" {
			t.Fatalf("activation: %+v %v", reply, ok)
		}
		if got := <-opened; got != id {
			t.Fatalf("got %q, want %q", got, id)
		}
	}
	other := activationHome(t)
	if _, ok := activateExisting(other, "ticket"); ok {
		t.Fatal("crossed data directories")
	}
	stop()
	if _, ok := activateExisting(home, "ticket"); ok {
		t.Fatal("closed window treated as live")
	}
}

func TestActivateIgnoresStaleEndpoint(t *testing.T) {
	home := activationHome(t)
	dir := filepath.Join(home, "ui")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale.sock"), []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := activateExisting(home, "ticket"); ok {
		t.Fatal("stale endpoint prevented fallback")
	}
}

type focusRunner struct {
	calls  [][]string
	legacy bool
}

func (r *focusRunner) Run(_ context.Context, cmd string, args ...string) error {
	r.calls = append(r.calls, append([]string{cmd}, args...))
	if r.legacy && len(r.calls) == 1 {
		return errors.New("legacy compositor")
	}
	return nil
}

func TestFocusSupportsBothHyprlandDispatchers(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		r := &focusRunner{legacy: legacy}
		if err := focusGravyWindow(context.Background(), r, "0x123abc"); err != nil {
			t.Fatal(err)
		}
		if r.calls[0][2] != `hl.dsp.focus({ window = "address:0x123abc" })` {
			t.Fatal(r.calls)
		}
		if legacy && (len(r.calls) != 2 || strings.Join(r.calls[1], " ") != "hyprctl dispatch focuswindow address:0x123abc") {
			t.Fatal(r.calls)
		}
	}
	r := &focusRunner{}
	if err := focusGravyWindow(context.Background(), r, `0x1" }); malicious()`); err == nil || len(r.calls) != 0 {
		t.Fatal("invalid address reached dispatcher")
	}
}
