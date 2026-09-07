package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"

	"github.com/pot-roast-co/gravy/internal/core"
)

// SocketName is the daemon's socket, inside the Gravy home directory.
const SocketName = "gravyd.sock"

// Server serves a Service over a unix socket.
//
// One connection carries either ordinary calls or the event stream, never both: a stream takes
// the connection over and writes notifications until it is closed. That keeps every write to a
// connection sequential without a per-connection mutex, and costs a client one extra dial.
type Server struct {
	svc  Service
	path string
	log  *slog.Logger

	ln net.Listener
	wg sync.WaitGroup

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// NewServer returns a server that will listen at path.
func NewServer(svc Service, path string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{svc: svc, path: path, log: log, conns: map[net.Conn]struct{}{}}
}

// Listen binds the socket, clearing a stale file left by a crash.
//
// A socket file outliving its process is the ordinary case after a kill -9, and refusing to start
// because of one would mean hand-deleting a file after every hard stop. Dialling it first is what
// tells a corpse apart from a daemon that is genuinely already running.
func (s *Server) Listen() error {
	if _, err := os.Stat(s.path); err == nil {
		c, derr := net.Dial("unix", s.path)
		if derr == nil {
			c.Close()
			return fmt.Errorf("a gravy daemon is already listening on %s", s.path)
		}
		if err := os.Remove(s.path); err != nil {
			return fmt.Errorf("remove stale socket %s: %w", s.path, err)
		}
		s.log.Info("removed a stale socket", "path", s.path)
	}

	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.path, err)
	}
	// The socket is the whole authorisation story: anything that can open it can approve a
	// merge, so it is the owner's alone.
	if err := os.Chmod(s.path, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("secure socket %s: %w", s.path, err)
	}
	s.ln = ln
	return nil
}

// Addr is the socket path being served, useful when a test binds a temporary one.
func (s *Server) Addr() string { return s.path }

// Serve accepts connections until ctx is cancelled or Close is called.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		return errors.New("api: Serve called before Listen")
	}

	go func() {
		<-ctx.Done()
		s.Close()
	}()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				s.wg.Wait()
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}

		s.track(conn)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrack(conn)
			s.handle(ctx, conn)
		}()
	}
}

// Close stops accepting, drops live connections and removes the socket file.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	if s.ln != nil {
		s.ln.Close()
	}
	for _, c := range conns {
		c.Close()
	}
	// Leaving the file behind would make the next start dial a socket nobody is serving.
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove socket %s: %w", s.path, err)
	}
	return nil
}

func (s *Server) track(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns[c] = struct{}{}
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// A line can be long: a review bundle carries a whole diff.
	r := bufio.NewReaderSize(conn, 64*1024)
	w := bufio.NewWriter(conn)
	dec := json.NewDecoder(r)

	for {
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			if err != io.EOF && !errors.Is(err, net.ErrClosed) {
				_ = writeResponse(w, &rpcResponse{
					JSONRPC: rpcVersion,
					Error:   &rpcError{Code: codeParse, Message: err.Error()},
				})
			}
			return
		}
		if req.JSONRPC != rpcVersion {
			_ = writeResponse(w, &rpcResponse{JSONRPC: rpcVersion, ID: req.ID,
				Error: &rpcError{Code: codeInvalidRequest, Message: "expected jsonrpc 2.0"}})
			continue
		}

		// A stream takes the connection over for its lifetime.
		if req.Method == mEvents {
			s.streamEvents(ctx, w, req.ID)
			return
		}
		if req.Method == mStreamLogs {
			var p runIDParams
			if err := unmarshalParams(req.Params, &p); err != nil {
				_ = writeResponse(w, &rpcResponse{JSONRPC: rpcVersion, ID: req.ID,
					Error: &rpcError{Code: codeInvalidParams, Message: err.Error()}})
				return
			}
			s.streamLogs(ctx, w, req.ID, p.RunID)
			return
		}

		result, err := s.dispatch(ctx, req.Method, req.Params)
		resp := &rpcResponse{JSONRPC: rpcVersion, ID: req.ID}
		switch {
		case err != nil:
			resp.Error = toRPCError(err)
		default:
			encoded, merr := json.Marshal(result)
			if merr != nil {
				resp.Error = &rpcError{Code: codeInternal, Message: merr.Error()}
			} else {
				resp.Result = encoded
			}
		}
		if req.ID == nil {
			continue // a notification wants no reply
		}
		if err := writeResponse(w, resp); err != nil {
			return
		}
	}
}

// streamEvents writes notifications until the client goes away.
func (s *Server) streamEvents(ctx context.Context, w *bufio.Writer, id *int64) {
	ch, stop, err := s.svc.Events(ctx)
	if err != nil {
		_ = writeResponse(w, &rpcResponse{JSONRPC: rpcVersion, ID: id, Error: toRPCError(err)})
		return
	}
	defer stop()

	// Acknowledge the subscription so the client knows it is live before the first event.
	if err := writeResponse(w, &rpcResponse{JSONRPC: rpcVersion, ID: id, Result: json.RawMessage(`{"subscribed":true}`)}); err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			params, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if err := writeResponse(w, &rpcResponse{
				JSONRPC: rpcVersion, Method: nEvent, Params: params,
			}); err != nil {
				return
			}
		}
	}
}

func writeResponse(w *bufio.Writer, resp *rpcResponse) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	return w.Flush()
}

// dispatch routes one call to the service.
func (s *Server) dispatch(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case mListProjects:
		return s.svc.ListProjects(ctx)

	case mAddProject:
		var p addProjectParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return s.svc.AddProject(ctx, p.Req)

	case mListTickets:
		var p listTicketsParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return s.svc.ListTickets(ctx, p.Filter)

	case mCreateTicket:
		var p createTicketParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return s.svc.CreateTicket(ctx, p.Req)

	case mMoveTicket:
		var p moveTicketParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return s.svc.MoveTicket(ctx, p.ID, core.Event(p.Event))

	case mKillRun:
		var p runIDParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return nil, s.svc.KillRun(ctx, p.RunID)

	case mListQueue:
		var p listTicketsParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return s.svc.ListQueue(ctx, p.Filter)

	case mUpdateTicket:
		var p updateTicketParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return nil, s.svc.UpdateTicket(ctx, p.Ticket)

	case mReorderTicket:
		var p reorderParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return nil, s.svc.ReorderTicket(ctx, p.ID, p.Before, p.After)

	case mDeleteTicket:
		var p idParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return nil, s.svc.DeleteTicket(ctx, p.ID)

	case mListRuns:
		var p ticketIDParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return s.svc.ListRuns(ctx, p.TicketID)

	case mGetReview:
		var p ticketIDParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return s.svc.GetReview(ctx, p.TicketID)

	case mApprove:
		var p ticketIDParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return nil, s.svc.Approve(ctx, p.TicketID)

	case mRequestChanges:
		var p requestChangesParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return nil, s.svc.RequestChanges(ctx, p.TicketID, p.Feedback)

	case mContinue:
		var p ticketIDParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return nil, s.svc.Continue(ctx, p.TicketID)

	case mReject:
		var p ticketIDParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return nil, s.svc.Reject(ctx, p.TicketID)

	case mListAttention:
		return s.svc.ListAttention(ctx)

	case mResolveAttention:
		var p idParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return nil, s.svc.ResolveAttention(ctx, p.ID)

	case mStatus:
		return s.svc.Status(ctx)

	case mExplainTicket:
		var p ticketIDParams
		if err := unmarshalParams(raw, &p); err != nil {
			return nil, err
		}
		return s.svc.ExplainTicket(ctx, p.TicketID)

	default:
		return nil, fmt.Errorf("unknown method %q", method)
	}
}

// streamLogs writes a run's output until it ends or the client goes away.
func (s *Server) streamLogs(ctx context.Context, w *bufio.Writer, id *int64, runID string) {
	ch, stop, err := s.svc.StreamLogs(ctx, runID)
	if err != nil {
		_ = writeResponse(w, &rpcResponse{JSONRPC: rpcVersion, ID: id, Error: toRPCError(err)})
		return
	}
	defer stop()

	if err := writeResponse(w, &rpcResponse{JSONRPC: rpcVersion, ID: id,
		Result: json.RawMessage(`{"subscribed":true}`)}); err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-ch:
			if !ok {
				// The run finished. Closing the connection is how the client learns that.
				return
			}
			params, err := json.Marshal(line)
			if err != nil {
				continue
			}
			if err := writeResponse(w, &rpcResponse{
				JSONRPC: rpcVersion, Method: nLogLine, Params: params,
			}); err != nil {
				return
			}
		}
	}
}
