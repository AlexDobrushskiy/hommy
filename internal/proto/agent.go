package proto

import "time"

// Agent pings (phase 07): `hommy send` POSTs SendRequest to the hub's host
// API (/api/send) and `hommy inbox ack` posts AckRequest (/api/ack), both
// authenticated with the host key. When the hub is unreachable the CLI sends
// straight to the Bot API and leaves a SpoolEntry, which the host forwards to
// the hub (EventSpool) once it is back, so replies to that message route.

// Agent is who a ping comes from.
type Agent struct {
	Slug     string `json:"slug"`
	Host     string `json:"host,omitempty"`      // set by the hub from the host key
	Session  string `json:"session,omitempty"`   // agterm session id
	BriefDir string `json:"brief_dir,omitempty"` // ~/agent-briefs/<folder>
	Link     string `json:"link,omitempty"`      // claude.ai session link
}

// SendRequest is one message to the owner. ID makes a retried POST idempotent.
type SendRequest struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Silent  bool     `json:"silent,omitempty"`
	Agent   *Agent   `json:"agent,omitempty"` // nil: a plain message (no header, no session)
	Buttons []string `json:"buttons,omitempty"`
	Ask     bool     `json:"ask,omitempty"` // add an "Answer…" button (the owner answers in text; no ForceReply: it would drop the 🎯 keyboard)
	// Plain: with Agent, the text goes out as is (no header, link or Open);
	// Agent only routes the owner's replies back to the sending session.
	Plain bool `json:"plain,omitempty"`
}

// SendResponse answers SendRequest.
type SendResponse struct {
	OK         bool   `json:"ok"`
	ChatID     int64  `json:"chat_id,omitempty"`
	MessageIDs []int  `json:"message_ids,omitempty"`
	Warning    string `json:"warning,omitempty"` // sent, but e.g. the buttons didn't attach
	Error      string `json:"error,omitempty"`
}

// AckRequest: the agent has handled inbox item Item (the owner's message MsgID) → 🫡.
type AckRequest struct {
	MsgID int    `json:"msg_id"`
	Item  string `json:"item"`
}

// EventSpool carries a SpoolEntry from a host (Event.JobID = the entry id; the
// hub acks with Ack{JobID, Kind: EventSpool}).
const EventSpool = "spool"

// Spool entry kinds.
const (
	SpoolSent = "sent" // a ping sent straight to the Bot API
	SpoolAck  = "ack"  // an inbox ack the hub didn't get
)

// SpoolEntry is what the hub missed while it was unreachable.
type SpoolEntry struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Hub     string    `json:"hub"` // HUB_URL of the config it was meant for
	Bot     int64     `json:"bot"` // the bot it was sent with (token prefix): a hub records only its own bot's
	ChatID  int64     `json:"chat_id"`
	Created time.Time `json:"created"`
	// sent
	MessageIDs []int    `json:"message_ids,omitempty"`
	Texts      []string `json:"texts,omitempty"` // each message's text, in order
	Agent      *Agent   `json:"agent,omitempty"`
	Buttons    []string `json:"buttons,omitempty"` // on the last message
	// ack
	Ack *AckRequest `json:"ack,omitempty"`
}
