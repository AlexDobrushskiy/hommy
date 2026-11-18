package proto

import "time"

// Interactive-session answers (phase 06): the global Claude Code / Codex hooks
// report to the local host (`hommy hook …` → hookrelay), which sends these
// durable events to the hub through its outbox (re-sent until acked, so an
// answer survives a hub outage). Event.JobID is "hook:<HookAnswer.ID>".
const (
	EventHookAnswer  = "hook_answer"  // Data: HookAnswer, the final text of a Telegram-driven turn
	EventHookWaiting = "hook_waiting" // Data: HookAnswer, Text = what the session waits for
	// EventHookResumed: the session left the prompt a hook_waiting reported
	// (answered, declined, interrupted, ended). Data: HookAnswer, ID =
	// <the waiting report's ID> + ResumedSuffix, Of = that ID.
	EventHookResumed = "hook_resumed"
	ResumedSuffix    = ".resumed"
)

// HookAnswer is a Telegram-driven turn's answer, or its "waiting at the Mac" ping.
type HookAnswer struct {
	ID        string `json:"id"`    // unique per host (idempotency key on the hub)
	Agent     string `json:"agent"` // claude | codex
	AgtermID  string `json:"agterm_id,omitempty"`
	SessionID string `json:"session_id,omitempty"` // Claude Code session / Codex thread id
	BridgeID  string `json:"bridge_id,omitempty"`  // CLAUDE_CODE_BRIDGE_SESSION_ID → https://claude.ai/code/<id>
	TgMsgID   int64  `json:"tg_msg_id"`            // the owner's message (bot-side id) the turn came from
	Text      string `json:"text"`
	Cwd       string `json:"cwd,omitempty"`
	Of        string `json:"of,omitempty"` // hook_resumed: the hook_waiting report it ends
}

// HookSession is what the hooks told the host about one live session; the
// heartbeat carries them so the hub knows each agterm session's claude.ai link.
type HookSession struct {
	AgtermID  string    `json:"agterm_id"`
	Agent     string    `json:"agent"`
	SessionID string    `json:"session_id,omitempty"`
	BridgeID  string    `json:"bridge_id,omitempty"`
	Cwd       string    `json:"cwd,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ClaudeLink is the claude.ai page of a bridged interactive session ("" when
// the id is unknown or malformed).
func ClaudeLink(bridgeID string) string {
	if bridgeID == "" || len(bridgeID) > 128 {
		return ""
	}
	for _, r := range bridgeID {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
		if !ok {
			return ""
		}
	}
	return "https://claude.ai/code/" + bridgeID
}
