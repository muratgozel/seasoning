// Package relayapi holds the relay's HTTP API types (SPEC §14.2), shared by
// the server and the client. Compatibility rule (SPEC §15.3): fields are only
// ever added, and both sides tolerate fields they don't know.
package relayapi

import "encoding/json"

// Error codes, stable across versions.
const (
	CodeBadRequest      = "bad_request"
	CodeUnauthorized    = "unauthorized"
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeSessionEnded    = "session_ended"
	CodeUnknownSessions = "unknown_sessions"
	CodeNoRecipients    = "no_recipients"
	CodePayloadTooLarge = "payload_too_large"
	CodeQueueFull       = "queue_full"
	CodeLimitReached    = "limit_reached"
	CodeRateLimited     = "rate_limited"
	CodeInternal        = "internal"
)

// ErrorBody is every error response.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail explains what went wrong and what to do about it.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// CreateProjectRequest registers a project. Authorized by the relay token.
type CreateProjectRequest struct {
	ProjectID string `json:"project_id"`
	AuthHash  string `json:"auth_hash"` // keys.EncodeAuthHash
}

// CreateProjectResponse reports whether the project is new.
type CreateProjectResponse struct {
	ProjectID string `json:"project_id"`
	Created   bool   `json:"created"`
}

// Session states.
const (
	StateLive  = "live"
	StateAway  = "away" // registered, but no heartbeat for a while
	StateEnded = "ended"
)

// PutSessionRequest registers a session or sends its heartbeat. Presence is
// an encrypted "presence" envelope; omit it to keep the stored one.
type PutSessionRequest struct {
	Presence json.RawMessage `json:"presence,omitempty"`
}

// Session is one session as the relay sees it.
type Session struct {
	Session    string          `json:"session"`
	State      string          `json:"state"`
	Presence   json.RawMessage `json:"presence,omitempty"`
	CreatedAt  string          `json:"created_at"`
	LastSeenAt string          `json:"last_seen_at"`
	EndedAt    string          `json:"ended_at,omitempty"`
}

// SessionsResponse lists a project's sessions with the relay's clock.
type SessionsResponse struct {
	Now      string    `json:"now"`
	Sessions []Session `json:"sessions"`
}

// Recipient delivery statuses.
const (
	StatusQueued    = "queued"
	StatusQueueFull = "queue_full"
)

// Recipient is one recipient's outcome for a posted message.
type Recipient struct {
	Session string `json:"session"`
	State   string `json:"state"`
	Status  string `json:"status"`
}

// PostMessageResponse answers POST …/messages.
type PostMessageResponse struct {
	Seq        int64       `json:"seq"`
	ReceivedAt string      `json:"received_at"`
	Recipients []Recipient `json:"recipients"`
}

// RosterEntry is one stored (encrypted) roster record.
type RosterEntry struct {
	Seq       int64           `json:"seq"`
	Record    json.RawMessage `json:"record"`
	CreatedAt string          `json:"created_at"`
}

// RosterResponse lists a project's roster in seq order.
type RosterResponse struct {
	Entries []RosterEntry `json:"entries"`
}

// PostRosterResponse answers POST …/roster.
type PostRosterResponse struct {
	Seq int64 `json:"seq"`
}

// RetireRequest schedules a project's deletion after a rotation.
type RetireRequest struct {
	AfterSeconds int64 `json:"after_seconds"`
}

// RetireResponse says when the project will be deleted.
type RetireResponse struct {
	RetireAt string `json:"retire_at"`
}

// AckRequest acknowledges everything up to and including UpToSeq.
type AckRequest struct {
	UpToSeq int64 `json:"up_to_seq"`
}

// Server-Sent Event types on GET …/sessions/{s}/events.
const (
	EventMessage = "message" // data: MessageEvent
	EventReceipt = "receipt" // data: ReceiptEvent
	EventSession = "session" // data: Session
	EventRoster  = "roster"  // data: RosterEntry
	// EventResync means broadcast events the client missed were pruned: it
	// must refetch sessions and roster. It carries no id.
	EventResync = "resync" // data: ResyncEvent
)

// MessageEvent delivers a stored envelope.
type MessageEvent struct {
	Seq        int64           `json:"seq"`
	ReceivedAt string          `json:"received_at"`
	Envelope   json.RawMessage `json:"envelope"`
}

// Receipt statuses.
const (
	ReceiptDelivered = "delivered"
	ReceiptExpired   = "expired"
)

// ReceiptEvent tells a sender what happened to one recipient's copy.
type ReceiptEvent struct {
	Seq       int64  `json:"seq"`
	RefSeq    int64  `json:"ref_seq"`
	Recipient string `json:"recipient"`
	Status    string `json:"status"`
	At        string `json:"at"`
}

// ResyncEvent explains a resync.
type ResyncEvent struct {
	Reason string `json:"reason"`
}

// Health answers GET /healthz.
type Health struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	Schema  int    `json:"schema"`
}
