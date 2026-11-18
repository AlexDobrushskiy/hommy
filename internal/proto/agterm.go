package proto

// Agent-session job types (phase 04). Payloads and result data are the request
// and result structs in internal/agterm (TypeRequest/TypeResult, …).
const (
	JobType        = "type"
	JobInboxAppend = "inbox_append"
	JobNewSession  = "new_session"
	JobPeek        = "peek"
	JobFiles       = "files"
	JobPause       = "pause"
	JobDone        = "done"
)

// EventTypeOutcome reports what became of a line a `type` job left queued
// (typed on a retry, expired, failed). JobID is the original job's id; Data is
// TypeOutcome.
const EventTypeOutcome = "type_outcome"

// TypeOutcome is EventTypeOutcome's Data.
type TypeOutcome struct {
	Outcome string `json:"outcome"` // delivered | unconfirmed | expired | failed
	Session string `json:"session,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// NoSession: expired without ever finding a live session (kept for a successor).
	NoSession bool `json:"no_session,omitempty"`
}
