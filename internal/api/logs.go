package api

import (
	"context"
	"fmt"
	"time"

	"github.com/pot-roast-co/gravy/internal/runlog"
)

// LogLine is one line of a run's output.
type LogLine struct {
	RunID string `json:"run_id"`
	// Stream is "agent" for raw provider output, "event" for a parsed progress event.
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
}

// WithLogs gives the service access to run logs. A service without them can still run the
// queue; it simply cannot show anyone what an agent is doing.
func (l *Local) WithLogs(s *runlog.Store) *Local {
	l.logs = s
	return l
}

// StreamLogs returns a run's output: what has been written so far, then what follows while it is
// still going. The channel closes when the run ends or ctx is cancelled.
func (l *Local) StreamLogs(ctx context.Context, runID string) (<-chan LogLine, func(), error) {
	if l.logs == nil {
		return nil, nil, fmt.Errorf("run logs are not available on this service")
	}
	src, stop, err := l.logs.Tail(ctx, runID)
	if err != nil {
		return nil, nil, err
	}

	out := make(chan LogLine, 64)
	go func() {
		defer close(out)
		for line := range src {
			select {
			case out <- LogLine{RunID: line.RunID, Stream: line.Stream, Text: line.Text, At: line.At}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, stop, nil
}
