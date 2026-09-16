package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"

	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
)

// Client is a Service backed by the daemon's unix socket.
//
// It satisfies the same interface as Local, so a caller cannot tell in-process from remote. That
// is the entire point of the seam: the TUI, the CLI subcommands and a future GUI are all just
// clients, and none of them can quietly grow a shortcut into the store.
type Client struct {
	path string

	// Each call gets its own connection, as Events and StreamLogs already do.
	//
	// Calls used to share one, serialised by mu. That made every slow method a freeze of
	// everything else: a planning turn runs an agent — minutes, and bounded only by the run
	// timeout — and the TUI's next refresh sat behind it holding a lock, so the whole screen
	// stopped until the planner answered. A unix socket connect costs microseconds; head-of-
	// line blocking on a UI's only transport costs the UI.
	//
	// mu now guards nothing but the liveness connection Dial opened and Close closes.
	mu     sync.Mutex
	conn   net.Conn
	closed bool
	nextID int64
}

// Dial connects to the daemon at path.
func Dial(path string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("connect to the gravy daemon at %s: %w", path, err)
	}
	// The connection is kept so that Dial means what its callers read it as — the daemon is
	// up and answering — and so Close has something to close.
	return &Client{path: path, conn: conn}, nil
}

// Close hangs up.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn, c.closed = nil, true
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
	closed := c.closed
	id := c.nextID + 1
	c.nextID = id
	c.mu.Unlock()
	if closed {
		return fmt.Errorf("%s: the connection is closed", method)
	}

	conn, err := net.Dial("unix", c.path)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer conn.Close()

	// A cancelled context must not leave the caller blocked on a daemon that is not answering.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	w := bufio.NewWriter(conn)
	req := rpcRequest{JSONRPC: rpcVersion, ID: &id, Method: method, Params: raw}
	b, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}

	var resp rpcResponse
	if err := json.NewDecoder(bufio.NewReaderSize(conn, 64*1024)).Decode(&resp); err != nil {
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

func (c *Client) ListProjects(ctx context.Context, f ProjectFilter) ([]core.Project, error) {
	var out []core.Project
	return out, c.call(ctx, mListProjects, projectFilterParams{Filter: f}, &out)
}

func (c *Client) ArchiveProject(ctx context.Context, id string, archived bool) error {
	return c.call(ctx, mArchiveProject, archiveProjectParams{ID: id, Archived: archived}, nil)
}

func (c *Client) AddProject(ctx context.Context, req AddProjectReq) (core.Project, error) {
	var out core.Project
	return out, c.call(ctx, mAddProject, addProjectParams{Req: req}, &out)
}

func (c *Client) UpdateProject(ctx context.Context, p core.Project) error {
	return c.call(ctx, mUpdateProject, updateProjectParams{Project: p}, nil)
}

func (c *Client) GetSettings(ctx context.Context) (Settings, error) {
	var out Settings
	return out, c.call(ctx, mGetSettings, nil, &out)
}

func (c *Client) UpdateSettings(ctx context.Context, cfg config.Config) (Settings, error) {
	var out Settings
	return out, c.call(ctx, mUpdateSettings, settingsParams{Config: cfg}, &out)
}

func (c *Client) ListTickets(ctx context.Context, f TicketFilter) ([]core.Ticket, error) {
	var out []core.Ticket
	return out, c.call(ctx, mListTickets, listTicketsParams{Filter: f}, &out)
}

func (c *Client) Rereview(ctx context.Context, ticketID string) error {
	return c.call(ctx, mRereview, checkoutParams{TicketID: ticketID}, nil)
}

func (c *Client) ReviewCheckout(ctx context.Context, ticketID string) (string, error) {
	var out string
	return out, c.call(ctx, mReviewCheckout, checkoutParams{TicketID: ticketID}, &out)
}

func (c *Client) DiscardReviewCheckout(ctx context.Context, ticketID string) error {
	return c.call(ctx, mDiscardCheckout, checkoutParams{TicketID: ticketID}, nil)
}

func (c *Client) DeleteProject(ctx context.Context, id string) error {
	return c.call(ctx, mDeleteProject, checkoutParams{TicketID: id}, nil)
}

func (c *Client) DetectAgents(ctx context.Context) []AgentStatus {
	var out []AgentStatus
	// Probing is slow enough to fail on a busy daemon, and a failure here is "we could not
	// tell" rather than an error onboarding should stop for.
	_ = c.call(ctx, mDetectAgents, struct{}{}, &out)
	return out
}

func (c *Client) Plan(ctx context.Context, req PlanReq) (PlanReply, error) {
	var out PlanReply
	return out, c.call(ctx, mPlan, planParams{Req: req}, &out)
}

func (c *Client) ApprovePlan(ctx context.Context, req ApprovePlanReq) ([]core.Ticket, error) {
	var out []core.Ticket
	return out, c.call(ctx, mApprovePlan, approvePlanParams{Req: req}, &out)
}

func (c *Client) CreateTicket(ctx context.Context, req CreateTicketReq) (core.Ticket, error) {
	var out core.Ticket
	return out, c.call(ctx, mCreateTicket, createTicketParams{Req: req}, &out)
}

func (c *Client) MoveTicket(ctx context.Context, id string, ev core.Event) (core.State, error) {
	var out core.State
	return out, c.call(ctx, mMoveTicket, moveTicketParams{ID: id, Event: string(ev)}, &out)
}

func (c *Client) ListQueue(ctx context.Context, f TicketFilter) ([]TicketDetail, error) {
	var out []TicketDetail
	return out, c.call(ctx, mListQueue, listTicketsParams{Filter: f}, &out)
}

func (c *Client) UpdateTicket(ctx context.Context, t core.Ticket) error {
	return c.call(ctx, mUpdateTicket, updateTicketParams{Ticket: t}, nil)
}

func (c *Client) ReorderTicket(ctx context.Context, id, before, after string) error {
	return c.call(ctx, mReorderTicket, reorderParams{ID: id, Before: before, After: after}, nil)
}

func (c *Client) DeleteTicket(ctx context.Context, id string) error {
	return c.call(ctx, mDeleteTicket, idParams{ID: id}, nil)
}

func (c *Client) ListRuns(ctx context.Context, ticketID string) ([]core.Run, error) {
	var out []core.Run
	return out, c.call(ctx, mListRuns, ticketIDParams{TicketID: ticketID}, &out)
}

func (c *Client) KillRun(ctx context.Context, runID string) error {
	return c.call(ctx, mKillRun, runIDParams{RunID: runID}, nil)
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

func (c *Client) Continue(ctx context.Context, ticketID string) error {
	return c.call(ctx, mContinue, ticketIDParams{TicketID: ticketID}, nil)
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

func (c *Client) Status(ctx context.Context, f ProjectFilter) (SystemStatus, error) {
	var out SystemStatus
	return out, c.call(ctx, mStatus, projectFilterParams{Filter: f}, &out)
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

// SetupInfo reads the setup draft's starting state.
func (c *Client) SetupInfo(ctx context.Context) (SetupInfo, error) {
	var out SetupInfo
	return out, c.call(ctx, "SetupInfo", nil, &out)
}

// PreviewSetup inspects a repository without saving it.
func (c *Client) PreviewSetup(ctx context.Context, req AddProjectReq) (SetupPreview, error) {
	var out SetupPreview
	return out, c.call(ctx, "PreviewSetup", req, &out)
}

// ApplySetup saves an explicitly approved setup draft.
func (c *Client) ApplySetup(ctx context.Context, req SetupRequest) (Settings, error) {
	var out Settings
	return out, c.call(ctx, "ApplySetup", req, &out)
}
