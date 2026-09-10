package notify

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
)

// chime is an original, soft two-note chime, lasting 800 milliseconds.
//
//go:embed assets/chime.wav
var chime []byte

// Sound installs the bundled chime and returns a player independent of terminal bells.
// Playback uses the local desktop audio session, including when called by the daemon.
func Sound(fs host.FS, runner Runner, dir, goos string) (func(context.Context) error, error) {
	if err := fs.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "chime.wav")
	if err := fs.WriteFile(path, chime, 0600); err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		players := []string{"pw-play", "paplay"}
		if goos == "darwin" {
			players = []string{"afplay"}
		}
		var err error
		for _, cmd := range players {
			if err = runner.Run(ctx, cmd, path); err == nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		return fmt.Errorf("play notification sound: %w", err)
	}, nil
}
