package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/provider"
	"github.com/pot-roast-co/gravy/internal/runlog"
)

// LogLine is one line of a run's output.
type LogLine struct {
	RunID string `json:"run_id"`
	// Stream is "agent" for raw provider output, "event" for a parsed progress event.
	Stream string `json:"stream"`
	// Kind is the event's kind, empty for raw output and for an event line that did not
	// decode.
	Kind provider.EventKind `json:"kind,omitempty"`
	// Tool is the tool an event concerns, for tool use and tool results.
	Tool string `json:"tool,omitempty"`
	// Text is the line rendered for a human. A line that did not decode is its raw text.
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// WithLogs gives the service access to run logs. A service without them can still run the
// queue; it simply cannot show anyone what an agent is doing.
func (l *Local) WithLogs(s *runlog.Store) *Local {
	l.logs = s
	return l
}

// StreamLogs returns a run's output: what has been written so far, then what follows while it is
// still going. The channel closes when the run ends or ctx is cancelled.
//
// Event lines are rendered here rather than in a client, so the TUI, the CLI and anything written
// later all read the same sentence for the same event.
func (l *Local) StreamLogs(ctx context.Context, runID string) (<-chan LogLine, func(), error) {
	if l.logs == nil {
		return nil, nil, fmt.Errorf("run logs are not available on this service")
	}
	src, stop, err := l.logs.Tail(ctx, runID)
	if err != nil {
		return nil, nil, err
	}

	// Only a rate-limit line needs the provider, and a run without a row (a plan run that has
	// not recorded one yet) still streams: it just cannot name who reported the limit.
	var providerID string
	if run, err := l.db.GetRun(ctx, runID); err == nil {
		providerID = run.ProviderID
	}

	out := make(chan LogLine, 64)
	go func() {
		defer close(out)
		for line := range src {
			ll := LogLine{RunID: line.RunID, Stream: line.Stream, Text: line.Text, At: line.At}
			if line.Stream == runlog.StreamEvent {
				ll.Kind, ll.Tool, ll.Text = renderEvent(line.Text, providerID)
			}
			select {
			case out <- ll:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, stop, nil
}

// renderEvent turns one events.jsonl line into its kind, tool and a line a human can read.
//
// A line that does not decode as an event, or whose kind this build does not know, comes back
// with an empty kind and its raw text: showing the JSON is worse than a sentence but better than
// showing nothing.
func renderEvent(raw, providerID string) (provider.EventKind, string, string) {
	var ev provider.Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil || ev.Kind == "" {
		return "", "", raw
	}
	text := strings.TrimSpace(ev.Text)

	switch ev.Kind {
	case provider.EventToolUse:
		return ev.Kind, ev.Tool, toolLine(ev.Tool, toolArgument(ev.Fields), text)

	case provider.EventToolResult:
		if first := firstLine(text); first != "" {
			return ev.Kind, ev.Tool, join(ev.Tool, "→", truncateLine(first, 160))
		}
		return ev.Kind, ev.Tool, join(ev.Tool, "done")

	case provider.EventMessage:
		return ev.Kind, ev.Tool, text

	case provider.EventError:
		if text == "" {
			text = stringOf(ev.Fields, "message")
		}
		if text == "" {
			return ev.Kind, ev.Tool, "error"
		}
		return ev.Kind, ev.Tool, "error: " + text

	case provider.EventRateLimit:
		who := providerID
		if who == "" {
			who = "the provider"
		}
		s := "rate limit reported by " + who
		if status := stringOf(ev.Fields, "status"); status != "" {
			s += ": " + status
		}
		return ev.Kind, ev.Tool, s

	case provider.EventThinking:
		// The reasoning itself is long and not what anyone scanning a run is looking for; that
		// the agent is thinking, rather than stalled, is.
		return ev.Kind, ev.Tool, "… thinking"

	case provider.EventUsage:
		in := intOf(ev.Fields, "input_tokens", "inputTokens")
		out := intOf(ev.Fields, "output_tokens", "outputTokens")
		return ev.Kind, ev.Tool, fmt.Sprintf("usage: %d tokens in, %d out", in, out)

	case provider.EventStarted:
		s := "started"
		if text != "" {
			s += " " + text
		} else if id := stringOf(ev.Fields, "sessionId", "session_id"); id != "" {
			s += " session " + id
		}
		if model := stringOf(ev.Fields, "model", "selectedModel"); model != "" {
			s += " (" + model + ")"
		}
		return ev.Kind, ev.Tool, s

	case provider.EventFinished:
		s := "finished"
		if turns := intOf(ev.Fields, "num_turns"); turns > 0 {
			s += fmt.Sprintf(" after %d turns", turns)
		}
		if reason := stringOf(ev.Fields, "terminal_reason"); reason != "" {
			s += " (" + reason + ")"
		}
		return ev.Kind, ev.Tool, s
	}
	return "", "", raw
}

// toolLine renders a tool call as "Edit internal/foo/bar.go".
//
// The argument comes from the call's structured fields when there are any. Otherwise the
// adapter's own text is used, without the tool name repeated if the adapter already led with it.
func toolLine(tool, arg, text string) string {
	if arg == "" {
		arg = strings.TrimSpace(strings.TrimPrefix(text, tool))
		if tool == "" {
			arg = text
		}
	}
	return join(tool, truncateLine(arg, 160))
}

// toolArgument picks the one argument that says what a tool call is doing.
//
// Adapters disagree on where the arguments live — claude-code puts the tool input in Fields
// directly, copilot nests it under "arguments" — so both are looked at.
func toolArgument(fields map[string]any) string {
	args := fields
	if nested, ok := fields["arguments"].(map[string]any); ok {
		args = nested
	}
	for _, key := range []string{"file_path", "path", "command", "pattern", "url", "query", "description"} {
		if s := stringOf(args, key); s != "" {
			return s
		}
	}
	// No known key: the first string argument, in a stable order so the same call always
	// renders the same way. Copilot's envelope keys name the call rather than describe it.
	keys := make([]string, 0, len(args))
	for k := range args {
		if k != "toolName" && k != "toolCallId" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := args[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func stringOf(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// intOf reads a number that has been through JSON, and so arrives as a float64.
func intOf(m map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case string:
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
	}
	return 0
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(s)
}

// truncateLine keeps a rendered line to one line of a reasonable width.
func truncateLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// join is strings.Join over the non-empty parts.
func join(parts ...string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}
