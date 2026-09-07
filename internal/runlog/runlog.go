// Package runlog stores and streams a run's output.
//
// Two files per run: agent.log, the provider's raw stdout, and events.jsonl, one parsed event
// per line. Both are append-only, which is what lets a reader follow a run that is still going
// and read the same bytes back after a restart.
package runlog

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Stream names the file a line came from.
const (
	StreamAgent = "agent"
	StreamEvent = "event"
)

// AgentFile and EventFile are the two files a run writes.
const (
	AgentFile = "agent.log"
	EventFile = "events.jsonl"
)

// subscriberBuffer is how far a reader may fall behind before it starts losing lines.
//
// Dropping a slow reader is the only acceptable answer: the alternative is a writer blocked on a
// UI that stopped reading, which stalls the run itself. A dropped reader loses output; a blocked
// writer loses the run.
const subscriberBuffer = 256

// Line is one line of a run's output.
type Line struct {
	RunID  string    `json:"run_id"`
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
}

// Store owns the per-run log directories under a root.
type Store struct {
	root string

	mu   sync.Mutex
	live map[string]*Writer
}

// New returns a store rooted at dir, normally ~/.gravy/runs.
func New(dir string) *Store { return &Store{root: dir, live: map[string]*Writer{}} }

// Root is the directory the store writes under.
func (s *Store) Root() string { return s.root }

// Dir is one run's directory.
func (s *Store) Dir(runID string) string { return filepath.Join(s.root, runID) }

// Open starts writing a run's logs. The caller closes the writer when the run ends.
func (s *Store) Open(runID string) (*Writer, error) {
	dir := s.Dir(runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("runlog: create %s: %w", dir, err)
	}

	agent, err := openAppend(filepath.Join(dir, AgentFile))
	if err != nil {
		return nil, err
	}
	events, err := openAppend(filepath.Join(dir, EventFile))
	if err != nil {
		agent.Close()
		return nil, err
	}

	w := &Writer{
		runID: runID, store: s, agent: agent, events: events,
		subs: map[int]chan Line{},
	}
	s.mu.Lock()
	s.live[runID] = w
	s.mu.Unlock()
	return w, nil
}

func openAppend(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("runlog: open %s: %w", path, err)
	}
	return f, nil
}

// Tail returns a run's output: everything written so far, then whatever follows while it is
// still running. The channel closes when the run ends or ctx is cancelled.
//
// History and subscription are taken under the writer's lock, so a line written between the two
// can be neither missed nor delivered twice.
func (s *Store) Tail(ctx context.Context, runID string) (<-chan Line, func(), error) {
	s.mu.Lock()
	w := s.live[runID]
	s.mu.Unlock()

	if w == nil {
		// Nothing is writing: the run finished, possibly under a previous daemon. Serve the
		// files and close.
		history, err := s.History(runID)
		if err != nil {
			return nil, nil, err
		}
		out := make(chan Line, len(history))
		for _, l := range history {
			out <- l
		}
		close(out)
		return out, func() {}, nil
	}

	w.mu.Lock()
	history, err := s.History(runID)
	if err != nil {
		w.mu.Unlock()
		return nil, nil, err
	}
	sub, stop := w.subscribeLocked()
	w.mu.Unlock()

	out := make(chan Line, subscriberBuffer)
	go func() {
		defer close(out)
		defer stop()
		for _, l := range history {
			select {
			case out <- l:
			case <-ctx.Done():
				return
			}
		}
		for {
			select {
			case l, ok := <-sub:
				if !ok {
					return
				}
				select {
				case out <- l:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, stop, nil
}

// History reads a run's stored lines, oldest first. It works after a restart, because the files
// are the record and the in-memory fanout is only a convenience for live readers.
func (s *Store) History(runID string) ([]Line, error) {
	dir := s.Dir(runID)
	var out []Line

	agent, err := readLines(filepath.Join(dir, AgentFile))
	if err != nil {
		return nil, err
	}
	for _, t := range agent {
		out = append(out, Line{RunID: runID, Stream: StreamAgent, Text: t})
	}

	events, err := readLines(filepath.Join(dir, EventFile))
	if err != nil {
		return nil, err
	}
	for _, t := range events {
		l := Line{RunID: runID, Stream: StreamEvent, Text: t}
		// events.jsonl carries a timestamp; agent.log does not, so only events can be
		// ordered by time.
		var probe struct {
			At time.Time `json:"at"`
		}
		if json.Unmarshal([]byte(t), &probe) == nil {
			l.At = probe.At
		}
		out = append(out, l)
	}
	return out, nil
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("runlog: read %s: %w", path, err)
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	// A single agent line can be long: a stack trace or a pasted file.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("runlog: read %s: %w", path, err)
	}
	return out, nil
}

// Prune removes run directories last modified before cutoff.
//
// It deletes directories only. Summaries live in the database and are the durable record a
// dependent ticket reads, so pruning logs can never take one with it — the two are kept in
// different places precisely so that retention on one is not a decision about the other.
func (s *Store) Prune(cutoff time.Time, keep func(runID string) bool) (int, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("runlog: prune: %w", err)
	}

	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		runID := e.Name()
		if keep != nil && keep(runID) {
			continue
		}

		s.mu.Lock()
		_, isLive := s.live[runID]
		s.mu.Unlock()
		if isLive {
			continue // never prune a run that is still writing
		}

		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.root, runID)); err != nil {
			return removed, fmt.Errorf("runlog: prune %s: %w", runID, err)
		}
		removed++
	}
	return removed, nil
}

// Runs lists the run ids the store holds logs for, newest first.
func (s *Store) Runs() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("runlog: list: %w", err)
	}
	type row struct {
		id string
		at time.Time
	}
	var rows []row
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rows = append(rows, row{e.Name(), info.ModTime()})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].at.After(rows[j].at) })

	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.id)
	}
	return out, nil
}

// Writer appends a run's output and fans it out to live readers.
type Writer struct {
	runID string
	store *Store

	// mu guards the files, the subscriber set and the fanout together, so a reader taking a
	// history snapshot cannot interleave with a write.
	mu     sync.Mutex
	agent  *os.File
	events *os.File
	subs   map[int]chan Line
	nextID int
	closed bool
}

// WriteAgent appends raw agent output. Text may contain newlines; each becomes a line.
func (w *Writer) WriteAgent(text string) error {
	return w.write(StreamAgent, text)
}

// WriteEvent appends one parsed event as JSON.
func (w *Writer) WriteEvent(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("runlog: encode event: %w", err)
	}
	return w.write(StreamEvent, string(b))
}

func (w *Writer) write(stream, text string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("runlog: run %s is closed", w.runID)
	}

	f := w.events
	if stream == StreamAgent {
		f = w.agent
	}

	now := time.Now()
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if _, err := f.WriteString(line + "\n"); err != nil {
			return fmt.Errorf("runlog: write %s: %w", stream, err)
		}
		w.publishLocked(Line{RunID: w.runID, Stream: stream, Text: line, At: now})
	}
	return nil
}

// publishLocked fans a line out, skipping readers that have stopped keeping up.
func (w *Writer) publishLocked(l Line) {
	for _, ch := range w.subs {
		select {
		case ch <- l:
		default:
			// Full. See subscriberBuffer: a dropped reader loses output, a blocked writer
			// loses the run.
		}
	}
}

func (w *Writer) subscribeLocked() (<-chan Line, func()) {
	id := w.nextID
	w.nextID++
	ch := make(chan Line, subscriberBuffer)
	w.subs[id] = ch

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			if c, ok := w.subs[id]; ok {
				delete(w.subs, id)
				close(c)
			}
		})
	}
}

// Close finishes the run's logs and releases its readers.
func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	for id, ch := range w.subs {
		delete(w.subs, id)
		close(ch)
	}
	agentErr := w.agent.Close()
	eventErr := w.events.Close()
	w.mu.Unlock()

	w.store.mu.Lock()
	delete(w.store.live, w.runID)
	w.store.mu.Unlock()

	if agentErr != nil {
		return agentErr
	}
	return eventErr
}
