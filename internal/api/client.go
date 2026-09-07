package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/bobbybrady/gravy/internal/core"
)

// Client is a Service backed by the daemon's unix socket.
//
// It satisfies the same interface as Local, so a caller cannot tell in-process from remote. That
// is the entire point of the seam: the TUI, the CLI subcommands and a future GUI are all just
// clients, and none of them can quietly grow a shortcut into the store.
type Client struct {
	path string

	// mu serialises calls over the single request connection: one request, one reply, in
	// order. Concurrency, when it is wanted, is a second Client.
	mu     sync.Mutex
	conn   net.Conn
	dec    *json.Decoder
	w      *bufio.Writer
	nextID int64
}

// Dial connects to the daemon at path.
func Dial(path string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("connect to the gravy daemon at %s: %w", path, err)
	}
	return &Client{
		path: path,
		conn: conn,
		dec:  json.NewDecoder(bufio.NewReaderSize(conn, 64*1024)),
		w:    bufio.NewWriter(conn),
	}, nil
}

// Close hangs up.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// call sends one request and decodes its reply into out, which may be nil for methods that
// return only an error.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("%s: the connection is closed", method)
	}

	// A cancelled context must not leave the caller blocked on a daemon that is not answering.
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(deadline)
		defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	}

	c.nextID++
	id := c.nextID
	req := rpcRequest{JSONRPC: rpcVersion, ID: &id, Method: method, Params: raw}
	b, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if err := c.w.Flush(); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}

	var resp rpcResponse
	if err := c.dec.Decode(&resp); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if resp.Error != nil {
		return resp.Error.toError()
	}
	if out == nil || len(resp.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("%s: decode result: %w", method, err)
	}
	return nil
}

func (c *Client) ListProjects(ctx context.Context) ([]core.Project, error) {
	var out []core.Project
	return out, c.call(ctx, mListProjects, nil, &out)
}

func (c *Client) AddProject(ctx context.Context, req AddProjectReq) (core.Project, error) {
	var out core.Project
	return out, c.call(ctx, mAddProject, addProjectParams{Req: req}, &out)
}

func (c *Client) ListTickets(ctx context.Context, f TicketFilter) ([]core.Ticket, error) {
	var out []core.Ticket
	return out, c.call(ctx, mListTickets, listTicketsParams{Filter: f}, &out)
}

func (c *Client) CreateTicket(ctx context.Context, req CreateTicketReq) (core.Ticket, error) {
	var out core.Ticket
	return out, c.call(ctx, mCreateTicket, createTicketParams{Req: req}, &out)
}

func (c *Client) MoveTicket(ctx context.Context, id string, ev core.Event) (core.State, error) {
	var out core.State
	return out, c.call(ctx, mMoveTicket, moveTicketParams{ID: id, Event: string(ev)}, &out)
}

func (c *Client) ListRuns(ctx context.Context, ticketID string) ([]core.Run, error) {
	var out []core.Run
	return out, c.call(ctx, mListRuns, ticketIDParams{TicketID: ticketID}, &out)
}

func (c *Client) GetReview(ctx context.Context, ticketID string) (ReviewBundle, error) {
	var out ReviewBundle
	return out, c.call(ctx, mGetReview, ticketIDParams{TicketID: ticketID}, &out)
}

func (c *Client) Approve(ctx context.Context, ticketID string) error {
	return c.call(ctx, mApprove, ticketIDParams{TicketID: ticketID}, nil)
}

func (c *Client) RequestChanges(ctx context.Context, ticketID, feedback string) error {
	return c.call(ctx, mRequestChanges,
		requestChangesParams{TicketID: ticketID, Feedback: feedback}, nil)
}

func (c *Client) Reject(ctx context.Context, ticketID string) error {
	return c.call(ctx, mReject, ticketIDParams{TicketID: ticketID}, nil)
}

func (c *Client) ListAttention(ctx context.Context) ([]core.Attention, error) {
	var out []core.Attention
	return out, c.call(ctx, mListAttention, nil, &out)
}

func (c *Client) ResolveAttention(ctx context.Context, id string) error {
	return c.call(ctx, mResolveAttention, idParams{ID: id}, nil)
}

func (c *Client) Status(ctx context.Context) (SystemStatus, error) {
	var out SystemStatus
	return out, c.call(ctx, mStatus, nil, &out)
}

func (c *Client) ExplainTicket(ctx context.Context, ticketID string) (Explanation, error) {
	var out Explanation
	return out, c.call(ctx, mExplainTicket, ticketIDParams{TicketID: ticketID}, &out)
}

// Events opens a second connection and streams notifications from it.
//
// A separate connection rather than multiplexing: the stream writes for as long as it lives, and
// interleaving that with request replies on one socket would need framing and a write lock to buy
// back exactly one file descriptor.
func (c *Client) Events(ctx context.Context) (<-chan Event, func(), error) {
	conn, err := net.Dial("unix", c.path)
	if err != nil {
		return nil, nil, fmt.Errorf("subscribe to events: %w", err)
	}

	id := int64(1)
	req := rpcRequest{JSONRPC: rpcVersion, ID: &id, Method: mEvents}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("subscribe to events: %w", err)
	}

	dec := json.NewDecoder(bufio.NewReaderSize(conn, 64*1024))

	// The acknowledgement means the subscription is live, so a caller that sees no error knows
	// it will not miss events raised from here on.
	var ack rpcResponse
	if err := dec.Decode(&ack); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("subscribe to events: %w", err)
	}
	if ack.Error != nil {
		conn.Close()
		return nil, nil, ack.Error.toError()
	}

	out := make(chan Event, eventBufferSize)
	var once sync.Once
	stop := func() { once.Do(func() { conn.Close() }) }

	go func() {
		<-ctx.Done()
		stop()
	}()

	go func() {
		defer close(out)
		defer stop()
		for {
			var note rpcResponse
			if err := dec.Decode(&note); err != nil {
				return
			}
			if note.Method != nEvent {
				continue
			}
			var ev Event
			if err := json.Unmarshal(note.Params, &ev); err != nil {
				continue
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, stop, nil
}

// Both implementations satisfy the same interface. AC1 of GR-006, asserted at compile time so a
// method added to one and not the other cannot reach a commit.
var (
	_ Service = (*Local)(nil)
	_ Service = (*Client)(nil)
)

// StreamLogs follows a run's output over its own connection, for the same reason Events does:
// a stream that writes for as long as it lives would otherwise need framing and a write lock to
// share a socket with request replies.
func (c *Client) StreamLogs(ctx context.Context, runID string) (<-chan LogLine, func(), error) {
	conn, err := net.Dial("unix", c.path)
	if err != nil {
		return nil, nil, fmt.Errorf("stream logs: %w", err)
	}

	id := int64(1)
	params, err := marshalParams(runIDParams{RunID: runID})
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	req := rpcRequest{JSONRPC: rpcVersion, ID: &id, Method: mStreamLogs, Params: params}
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("stream logs: %w", err)
	}

	dec := json.NewDecoder(bufio.NewReaderSize(conn, 64*1024))
	var ack rpcResponse
	if err := dec.Decode(&ack); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("stream logs: %w", err)
	}
	if ack.Error != nil {
		conn.Close()
		return nil, nil, ack.Error.toError()
	}

	out := make(chan LogLine, 64)
	var once sync.Once
	stop := func() { once.Do(func() { conn.Close() }) }

	go func() {
		<-ctx.Done()
		stop()
	}()

	go func() {
		defer close(out)
		defer stop()
		for {
			var note rpcResponse
			if err := dec.Decode(&note); err != nil {
				return
			}
			if note.Method != nLogLine {
				continue
			}
			var line LogLine
			if err := json.Unmarshal(note.Params, &line); err != nil {
				continue
			}
			select {
			case out <- line:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, stop, nil
}
