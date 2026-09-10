package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/host"
)

func TestDesktopSoundUsesRateLimitAndAvoidsDuplicateBell(t *testing.T) {
	for _, mode := range []config.NotifyMode{config.NotifyOff, config.NotifyBell, config.NotifyBellAndOS} {
		t.Run(string(mode), func(t *testing.T) {
			r := &fakeRunner{}
			n, buf, _ := newTest(t, mode, time.Minute, "linux", r)
			plays := 0
			WithSound(func(context.Context) error { plays++; return nil })(n)
			for i := 0; i < 5; i++ {
				n.Notify(context.Background(), "title", "body", Normal)
			}
			want := 1
			if mode == config.NotifyOff {
				want = 0
			}
			if plays != want || buf.Len() != 0 {
				t.Fatalf("plays=%d, terminal=%q", plays, buf.String())
			}
			if mode == config.NotifyBellAndOS && !strings.Contains(strings.Join(r.calls[0], " "), "suppress-sound:true") {
				t.Fatal("desktop may play a second sound")
			}
		})
	}
}

func TestSoundFailureFallsBackAndStillNotifies(t *testing.T) {
	r := &fakeRunner{}
	n, buf, _ := newTest(t, config.NotifyBellAndOS, 0, "linux", r)
	WithSound(func(context.Context) error { return errors.New("no audio server") })(n)
	n.Notify(context.Background(), "title", "body", Normal)
	if buf.String() != "\a" || r.count() != 1 {
		t.Fatal("lost fallback alert")
	}
}

func TestClickTargetIsEncodedWithoutShellInterpretation(t *testing.T) {
	r := &fakeRunner{}
	n, _, _ := newTest(t, config.NotifyBellAndOS, 0, "linux", r)
	ticket := "ticket with spaces; $(false)"
	WithClickArgs(func(id string) []string { return []string{"xdg-terminal-exec", "/path with spaces/gravy", "open", id} })(n)
	Deliver(context.Background(), n, "title", "body", Normal, ticket)
	for _, arg := range r.calls[0] {
		if strings.HasPrefix(arg, "--hint=string:omarchy-exec-argv:") {
			var argv []string
			if err := json.Unmarshal([]byte(strings.TrimPrefix(arg, "--hint=string:omarchy-exec-argv:")), &argv); err != nil {
				t.Fatal(err)
			}
			if len(argv) != 4 || argv[3] != ticket {
				t.Fatalf("wrong destination: %q", argv)
			}
			return
		}
	}
	t.Fatal("no click action")
}

type fallbackPlayer struct{ calls []string }

func (r *fallbackPlayer) Run(_ context.Context, cmd string, args ...string) error {
	r.calls = append(r.calls, cmd)
	if cmd == "pw-play" {
		return errors.New("not installed")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(data, []byte("RIFF")) {
		return errors.New("invalid sound")
	}
	return nil
}

func TestSoundInstallsAssetAndFallsBackToPulseAudio(t *testing.T) {
	r := &fallbackPlayer{}
	play, err := Sound(host.NewLocal("test", 1).FS(), r, t.TempDir(), "linux")
	if err != nil {
		t.Fatal(err)
	}
	if err := play(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.calls, ",") != "pw-play,paplay" {
		t.Fatal(r.calls)
	}
}
