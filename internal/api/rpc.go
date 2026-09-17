package api

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/store"
)

// JSON-RPC 2.0 over a unix socket, one JSON object per line.
//
// Line-delimited rather than framed: the payloads are small, every value is already JSON, and a
// newline is a boundary both a Go client and a shell one-liner can find. The event stream reuses
// the same connection shape — a request that never returns, answering with notifications.

const rpcVersion = "2.0"

// rpcRequest is one call. A request with no ID is a notification and gets no reply.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is one reply, or one pushed event when Method is set.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	// Method and Params carry server-push notifications, which have no ID.
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// rpcError carries a failure across the wire.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Kind names the error's class so the client can rebuild something errors.Is can match.
	// A "not found" that arrives as an untyped string turns every caller's check into string
	// comparison, which is the kind of thing that works until someone rewords a message.
	Kind string `json:"kind,omitempty"`
}

// JSON-RPC reserved codes, plus one for ordinary application failures.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeApplication    = -32000
)

// Error kinds that survive the wire.
const (
	kindNotFound      = "not_found"
	kindAlreadyLanded = "already_landed"
)

// toRPCError classifies an error for transport.
func toRPCError(err error) *rpcError {
	e := &rpcError{Code: codeApplication, Message: err.Error()}
	switch {
	case errors.Is(err, store.ErrNotFound):
		e.Kind = kindNotFound
	case errors.Is(err, core.ErrAlreadyLanded):
		e.Kind = kindAlreadyLanded
	}
	return e
}

// wireError is the client-side error, rebuilt so errors.Is still answers correctly.
type wireError struct {
	msg  string
	kind string
}

func (e *wireError) Error() string { return e.msg }

// Unwrap returns the sentinel a caller is likely to test for.
func (e *wireError) Unwrap() error {
	switch e.kind {
	case kindNotFound:
		return store.ErrNotFound
	case kindAlreadyLanded:
		return core.ErrAlreadyLanded
	}
	return nil
}

func (e *rpcError) toError() error {
	if e == nil {
		return nil
	}
	return &wireError{msg: e.Message, kind: e.Kind}
}

// Method names. They are constants because the client and the server dispatch table must agree,
// and a typo in a string literal is a runtime failure on one method only.
const (
	mListProjects     = "ListProjects"
	mAddProject       = "AddProject"
	mArchiveProject   = "ArchiveProject"
	mDeleteProject    = "DeleteProject"
	mDetectAgents     = "DetectAgents"
	mPlan             = "Plan"
	mRereview         = "Rereview"
	mReviewCheckout   = "ReviewCheckout"
	mDiscardCheckout  = "DiscardReviewCheckout"
	mApprovePlan      = "ApprovePlan"
	mListTickets      = "ListTickets"
	mCreateTicket     = "CreateTicket"
	mMoveTicket       = "MoveTicket"
	mListRuns         = "ListRuns"
	mGetReview        = "GetReview"
	mApprove          = "Approve"
	mRequestChanges   = "RequestChanges"
	mReject           = "Reject"
	mListAttention    = "ListAttention"
	mResolveAttention = "ResolveAttention"
	mStatus           = "Status"
	mReconnectHost    = "ReconnectHost"
	mExplainTicket    = "ExplainTicket"
	mEvents           = "Events"
	mStreamLogs       = "StreamLogs"
	mKillRun          = "KillRun"
	mContinue         = "Continue"
	mListQueue        = "ListQueue"
	mUpdateTicket     = "UpdateTicket"
	mReorderTicket    = "ReorderTicket"
	mDeleteTicket     = "DeleteTicket"
	mUpdateProject    = "UpdateProject"
	mGetSettings      = "GetSettings"
	mUpdateSettings   = "UpdateSettings"
	// nEvent is the notification the server pushes on the Events stream.
	nEvent = "event"
	// nLogLine is the notification pushed on the StreamLogs stream.
	nLogLine = "logline"
)

// Parameter envelopes. Each method has one so the wire format is self-describing: a params
// object names its fields, where a positional array would silently accept the wrong order.
type (
	runIDParams struct {
		RunID string `json:"run_id"`
	}
	idParams struct {
		ID string `json:"id"`
	}
	ticketIDParams struct {
		TicketID string `json:"ticket_id"`
	}
	hostIDParams struct {
		HostID string `json:"host_id"`
	}
	moveTicketParams struct {
		ID    string `json:"id"`
		Event string `json:"event"`
	}
	requestChangesParams struct {
		TicketID string `json:"ticket_id"`
		Feedback string `json:"feedback"`
	}
	listTicketsParams struct {
		Filter TicketFilter `json:"filter"`
	}
	updateTicketParams struct {
		Ticket core.Ticket `json:"ticket"`
	}
	reorderParams struct {
		ID     string `json:"id"`
		Before string `json:"before"`
		After  string `json:"after"`
	}
	addProjectParams struct {
		Req AddProjectReq `json:"req"`
	}
	planParams struct {
		Req PlanReq `json:"req"`
	}
	checkoutParams struct {
		TicketID string `json:"ticket_id"`
	}
	approvePlanParams struct {
		Req ApprovePlanReq `json:"req"`
	}
	updateProjectParams struct {
		Project core.Project `json:"project"`
	}
	archiveProjectParams struct {
		ID       string `json:"id"`
		Archived bool   `json:"archived"`
	}
	projectFilterParams struct {
		Filter ProjectFilter `json:"filter"`
	}
	settingsParams struct {
		Config config.Config `json:"config"`
	}
	createTicketParams struct {
		Req CreateTicketReq `json:"req"`
	}
)

func marshalParams(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode params: %w", err)
	}
	return b, nil
}

func unmarshalParams(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("decode params: %w", err)
	}
	return nil
}
