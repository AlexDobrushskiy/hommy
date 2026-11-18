// Package proto is the hub ⇄ host wire format: one JSON Frame per WebSocket
// text message.
package proto

import (
	"encoding/json"
	"time"
)

// Job is one unit of work the hub pushes to a host.
type Job struct {
	ID      string          `json:"id"`       // hub-assigned
	EventID string          `json:"event_id"` // idempotency key
	Type    string          `json:"type"`     // run_p, save_file, … (04 adds type, new_session, inbox_append, peek, files, pause, done)
	Payload json.RawMessage `json:"payload"`
}

// Result statuses.
const (
	StatusOK        = "ok"
	StatusInbox     = "inbox"
	StatusNoSession = "no_session"
	StatusError     = "error"
)

// Result is the host's answer to a Job; it doubles as the job's ack.
type Result struct {
	JobID  string          `json:"job_id"`
	Status string          `json:"status"` // ok | inbox | no_session | error
	Data   json.RawMessage `json:"data,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Event kinds a handler emits while a job runs.
const (
	EventStarted  = "started"
	EventProgress = "progress" // Data: Progress
)

// Progress is a run_p snapshot: the full state so far, never a delta, so a
// dropped or replayed event can't garble the stream. Seq grows per job.
type Progress struct {
	Seq    int    `json:"seq"`
	Status string `json:"status"`         // e.g. "⚙ Read · inbox/today.md"
	Text   string `json:"text,omitempty"` // answer text so far (current assistant message)
}

// Event is progress on a running job (started, then run_p Progress snapshots).
type Event struct {
	JobID string          `json:"job_id"`
	Kind  string          `json:"kind"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Heartbeat is the host's health report, sent every HeartbeatEvery.
type Heartbeat struct {
	Host           string          `json:"host"`
	Version        string          `json:"version"`
	StartedAt      time.Time       `json:"started_at"`
	RunningJobs    int             `json:"running_jobs"`
	ClaudeSessions int             `json:"claude_sessions"`         // live claude processes on the host
	AgentFolders   []string        `json:"agent_folders,omitempty"` // ~/agent-briefs/<dir> names, newest first
	Agterm         json.RawMessage `json:"agterm,omitempty"`        // agterm tree + sessions (phase 04)
	Hooks          []HookSession   `json:"hooks,omitempty"`         // sessions the hooks reported (phase 06)
	// Checks is the host's own health: vault ok|missing|denied, agterm ok|down.
	Checks map[string]string `json:"checks,omitempty"`
}

// HeartbeatEvery is how often a host reports; the hub drops a connection that
// stays silent for ReadTimeout.
const (
	HeartbeatEvery = 10 * time.Second
	ReadTimeout    = 35 * time.Second
)

// Hello is the host's first frame on a connection.
type Hello struct {
	Host    string `json:"host"`
	Version string `json:"version"`
}

// Frame types.
const (
	FrameHello     = "hello"
	FrameJob       = "job"
	FrameResult    = "result"
	FrameEvent     = "event"
	FrameHeartbeat = "heartbeat"
	FrameAck       = "ack" // hub → host: heartbeat received (keeps the host's read deadline fresh)
)

// Frame is the envelope; exactly one payload field matches Type.
type Frame struct {
	Type      string     `json:"type"`
	Hello     *Hello     `json:"hello,omitempty"`
	Job       *Job       `json:"job,omitempty"`
	Result    *Result    `json:"result,omitempty"`
	Event     *Event     `json:"event,omitempty"`
	Heartbeat *Heartbeat `json:"heartbeat,omitempty"`
	Ack       *Ack       `json:"ack,omitempty"` // on FrameAck: a durable event the hub handled
}

// Ack names a durable event (EventTypeOutcome) the hub has handled; the host
// stops re-sending it. A FrameAck without Ack answers a heartbeat.
type Ack struct {
	JobID string `json:"job_id"`
	Kind  string `json:"kind"`
}

// Job types and payloads owned by phase 02.
const (
	JobRunP     = "run_p"
	JobSaveFile = "save_file"
	JobCancel   = "cancel" // Payload: Cancel
)

// Cancel stops the job with EventID on the host (a no-op when it isn't
// running; a later push of it then doesn't run it).
type Cancel struct {
	EventID string `json:"event_id"`
}

// CancelResult is Cancel's Result.Data.
type CancelResult struct {
	WasRunning bool `json:"was_running"`
}

// Error texts the hub tells apart.
const (
	ErrStopped     = "stopped"
	ErrInterrupted = "interrupted: the host restarted during the run"
)

// RunP runs `claude -p` in Dir ("" = the host's vault dir), resuming Resume.
type RunP struct {
	Prompt             string `json:"prompt"`
	Dir                string `json:"dir,omitempty"`
	Resume             string `json:"resume,omitempty"`
	AppendSystemPrompt string `json:"append_system_prompt,omitempty"`
	// NewDir: the first run of a new -p session; Dir must satisfy the new-session
	// folder rule and is created when missing.
	NewDir bool `json:"new_dir,omitempty"`
}

// RunPResult is RunP's Result.Data (also on errors, for SessionID). A stopped
// run is Status error, Error ErrStopped, Stopped true, Text = the partial answer.
type RunPResult struct {
	Text      string  `json:"text,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	Stopped   bool    `json:"stopped,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
}

// SaveFile writes bytes from Telegram to the host. Dest "" = the host's media
// dir (where the bot has always saved photos and files).
type SaveFile struct {
	Name string `json:"name,omitempty"` // original file name, if any
	Ext  string `json:"ext"`            // e.g. ".jpg"
	Data []byte `json:"data"`
	Dest string `json:"dest,omitempty"`
}

// SaveFileResult is SaveFile's Result.Data.
type SaveFileResult struct {
	Path string `json:"path"`
}

// MaxFrameBytes bounds one frame (Telegram bot downloads cap at 20 MB; base64 adds a third).
const MaxFrameBytes = 64 << 20
