package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// Relay is the host side of the hooks: it remembers each session (agterm id,
// claude.ai bridge id) and whether its current turn came from Telegram, and
// turns the end of such a turn into a durable answer for the hub.
type Relay struct {
	ingest    sync.Mutex // one drain-or-request at a time, so the order holds
	mu        sync.Mutex
	statePath string
	spoolDir  string
	host      string
	now       func() time.Time
	emit      func(kind string, a proto.HookAnswer) error // durable: outbox write before it returns
	st        relayState
}

type relayState struct {
	Sessions map[string]*Session `json:"sessions"`
	// Typed: Telegram message id → the agterm session the host typed its line
	// into. A Codex TUI attached to Codex's shared daemon runs its hooks in the
	// daemon, whose AGTERM_SESSION_ID belongs to whatever terminal started it,
	// so for a Telegram turn this beats the hook's env.
	Typed map[string]typedLine `json:"typed,omitempty"`
}

type typedLine struct {
	AgtermID string    `json:"agterm_id"`
	At       time.Time `json:"at"`
}

const keepTyped = 48 * time.Hour

// NoteTyped records that the line of Telegram message msgID was typed (or
// queued to be typed) into agterm session agtermID.
func (r *Relay) NoteTyped(msgID int64, agtermID string) {
	if msgID <= 0 || agtermID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if r.st.Typed == nil {
		r.st.Typed = map[string]typedLine{}
	}
	for k, v := range r.st.Typed {
		if now.Sub(v.At) > keepTyped {
			delete(r.st.Typed, k)
		}
	}
	id := strings.ToUpper(agtermID)
	r.st.Typed[strconv.FormatInt(msgID, 10)] = typedLine{AgtermID: id, At: now}
	for _, s := range r.st.Sessions { // the prompt hook ran first (or a successor took the line)
		if s.Turn != nil && s.Turn.TgMsgID == msgID {
			s.Turn.AgtermID, s.AgtermID = id, id
		}
	}
	if err := r.saveLocked(); err != nil {
		log.Printf("hookrelay: save state: %v", err)
	}
}

// Session is one Claude Code session / Codex thread the hooks reported.
type Session struct {
	SessionID  string    `json:"session_id"`
	Agent      string    `json:"agent"`
	AgtermID   string    `json:"agterm_id,omitempty"`
	BridgeID   string    `json:"bridge_id,omitempty"`
	Cwd        string    `json:"cwd,omitempty"`
	Transcript string    `json:"transcript,omitempty"`
	Updated    time.Time `json:"updated"`
	Turn       *Turn     `json:"turn,omitempty"` // set while a Telegram-driven turn runs
}

// Turn is a turn whose prompt was a line typed from Telegram.
type Turn struct {
	TgMsgID  int64     `json:"tg_msg_id"`
	AgtermID string    `json:"agterm_id,omitempty"` // where the host typed the line (beats the hook env)
	PromptID string    `json:"prompt_id,omitempty"`
	Started  time.Time `json:"started"`            // when the prompt hook ran (the same on a replay)
	Notified bool      `json:"notified,omitempty"` // a ⏸ ping went out (one per prompt)
	// WaitingAt is when the latest ⏸ hook ran; Resumed: the session has left
	// that prompt since (hook_resumed sent), so a later prompt gets its own ⏸.
	// Waits counts the ⏸ pings (0 in older state: one).
	WaitingAt time.Time `json:"waiting_at,omitempty"`
	Resumed   bool      `json:"resumed,omitempty"`
	Waits     int       `json:"waits,omitempty"`
	// Answers is read only for state written by older hosts, which retained
	// answered turns. Such turns are closed when the state is loaded.
	Answers int `json:"answers,omitempty"`
}

const (
	keepSessions = 300
	keepFor      = 14 * 24 * time.Hour
	// turnMaxAge drops a Telegram turn that never saw its Stop (session killed
	// mid-turn), so a much later Stop isn't sent as its answer.
	turnMaxAge = 24 * time.Hour
)

// NewRelay loads the relay's state from statePath.
func NewRelay(statePath, spoolDir, host string, now func() time.Time, emit func(string, proto.HookAnswer) error) (*Relay, error) {
	r := &Relay{statePath: statePath, spoolDir: spoolDir, host: host, now: now, emit: emit,
		st: relayState{Sessions: map[string]*Session{}}}
	b, err := os.ReadFile(statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &r.st); err != nil {
			log.Printf("hookrelay: state %s unreadable, starting empty: %v", statePath, err)
			r.st = relayState{}
		}
		if r.st.Sessions == nil {
			r.st.Sessions = map[string]*Session{}
		}
	}
	migrated := false
	for _, s := range r.st.Sessions {
		if s.Turn != nil && s.Turn.Answers > 0 {
			if err := r.resumeLocked(s); err != nil {
				return nil, fmt.Errorf("close answered turn: %w", err)
			}
			s.Turn = nil
			migrated = true
		}
	}
	if migrated {
		if err := r.saveLocked(); err != nil {
			return nil, fmt.Errorf("save closed turns: %w", err)
		}
	}
	return r, nil
}

// telegramRe is the prefix every line typed from Telegram starts with
// (internal/sanitize): `[Telegram · tg-<msgid> …]`.
var telegramRe = regexp.MustCompile(`^\s*\[Telegram · tg-(\d+)[\] ·]`)

// TelegramMsgID is the Telegram message id a prompt was typed from (0 = typed locally).
func TelegramMsgID(prompt string) int64 {
	m := telegramRe.FindStringSubmatch(prompt)
	if m == nil {
		return 0
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return id
}

// input is the union of the hook inputs the relay reads (Claude Code and Codex
// use the same snake_case names).
type input struct {
	SessionID        string  `json:"session_id"`
	TranscriptPath   string  `json:"transcript_path"`
	Cwd              string  `json:"cwd"`
	Prompt           string  `json:"prompt"`
	PromptID         string  `json:"prompt_id"`
	TurnID           string  `json:"turn_id"`
	LastAssistant    *string `json:"last_assistant_message"`
	Message          string  `json:"message"`
	NotificationType string  `json:"notification_type"`
	ToolName         string  `json:"tool_name"` // Codex PermissionRequest
	ToolInput        struct {
		Command     any    `json:"command"`
		Description string `json:"description"`
	} `json:"tool_input"`
}

func prefixed(p string, v any) string {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case []any:
		var parts []string
		for _, e := range x {
			parts = append(parts, fmt.Sprint(e))
		}
		s = strings.Join(parts, " ")
	}
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return p + s
}

// ErrBadRequest marks a request that can never be handled (it is dropped,
// not retried).
var ErrBadRequest = errors.New("bad hook request")

// Handle applies one hook request. An error means its effect is not on disk
// (the state is as before) and the request must be handled again; an answer
// it already emitted then comes again with the same id.
func (r *Relay) Handle(req Request) error {
	var in input
	if err := json.Unmarshal(req.Input, &in); err != nil {
		return fmt.Errorf("%w: hook input: %v", ErrBadRequest, err)
	}
	if req.Event != EventSessionStart && req.Event != EventUserPromptSubmit && req.Event != EventStop && req.Event != EventNotify {
		return fmt.Errorf("%w: unknown hook event %q", ErrBadRequest, req.Event)
	}
	if req.Agent == "" {
		req.Agent = AgentClaude
	}
	key := in.SessionID
	if key == "" {
		key = req.Agent + ":" + req.AgtermID
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, err := json.Marshal(r.st)
	if err != nil {
		return err
	}
	if err := r.handleLocked(req, in, key); err != nil {
		r.restoreLocked(prev)
		return err
	}
	if err := r.saveLocked(); err != nil {
		r.restoreLocked(prev)
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

func (r *Relay) restoreLocked(prev []byte) {
	var st relayState
	if err := json.Unmarshal(prev, &st); err != nil {
		log.Printf("hookrelay: restore state: %v", err)
		return
	}
	if st.Sessions == nil {
		st.Sessions = map[string]*Session{}
	}
	r.st = st
}

func (r *Relay) handleLocked(req Request, in input, key string) error {
	now := r.now()
	started := req.At
	if started.IsZero() {
		started = now
	}
	s := r.st.Sessions[key]
	if s == nil {
		s = &Session{SessionID: in.SessionID, Agent: req.Agent}
		r.st.Sessions[key] = s
	}
	s.Updated = now
	if req.AgtermID != "" {
		s.AgtermID = strings.ToUpper(req.AgtermID)
	}
	if req.BridgeID != "" {
		s.BridgeID = req.BridgeID
	}
	if in.Cwd != "" {
		s.Cwd = in.Cwd
	}
	if in.TranscriptPath != "" {
		s.Transcript = in.TranscriptPath
	}
	if s.Turn != nil && now.Sub(s.Turn.Started) > turnMaxAge {
		s.Turn = nil
	}

	switch req.Event {
	case EventSessionStart:
	case EventUserPromptSubmit:
		if req.SystemPrompt && s.Turn != nil {
			// A notification absorbed before the first Stop continues the turn.
			break
		}
		if err := r.resumeLocked(s); err != nil { // a prompt went in: no dialog holds the input
			return err
		}
		s.Turn = nil
		if id := TelegramMsgID(in.Prompt); id != 0 {
			s.Turn = &Turn{TgMsgID: id, PromptID: firstNonEmpty(in.PromptID, in.TurnID), Started: started,
				AgtermID: r.st.Typed[strconv.FormatInt(id, 10)].AgtermID}
			if s.Turn.AgtermID != "" {
				s.AgtermID = s.Turn.AgtermID
			}
		}
	case EventStop:
		if s.Turn == nil {
			break // a turn typed at the Mac: nothing goes to the chat
		}
		text := ""
		if in.LastAssistant != nil {
			text = *in.LastAssistant
		} else {
			text = finalText(req.Agent, s.Transcript)
		}
		if err := r.resumeLocked(s); err != nil {
			return err
		}
		if err := r.emit(proto.EventHookAnswer, r.answer(s, "answer", text)); err != nil {
			return err
		}
		s.Turn = nil // the first durably emitted Stop ends the Telegram turn
	case EventNotify:
		if s.Turn == nil || (s.Turn.Notified && !s.Turn.Resumed) || !waitingNotice(in.NotificationType) {
			break // still at the prompt its ⏸ reported
		}
		msg := firstNonEmpty(in.Message, in.ToolInput.Description, prefixed("wants to run: ", in.ToolInput.Command),
			prefixed("wants to use ", in.ToolName), "needs your input")
		if s.Turn.Notified { // restored with the state if the emit fails
			s.Turn.Waits = max(s.Turn.Waits, 1) + 1
		} else {
			s.Turn.Waits = 1
		}
		if err := r.emit(proto.EventHookWaiting, r.answer(s, "waiting", msg)); err != nil {
			return err
		}
		s.Turn.Notified, s.Turn.Resumed, s.Turn.WaitingAt = true, false, started
	default:
		return fmt.Errorf("unknown hook event %q", req.Event)
	}
	r.pruneLocked(now)
	return nil
}

// waitingNotice: Claude Code notification types that mean "a person must act
// at the Mac" (idle_prompt fires after a finished turn; auth_success is info).
func waitingNotice(typ string) bool {
	switch typ {
	case "idle_prompt", "auth_success":
		return false
	}
	return true
}

// answer builds the event for the current turn; its id depends only on the
// requests (not on when the host handled them), so a replay gets the same id
// and the hub keeps one.
func (r *Relay) answer(s *Session, kind, text string) proto.HookAnswer {
	parts := []string{r.host, s.SessionID, kind, strconv.FormatInt(s.Turn.TgMsgID, 10), s.Turn.PromptID,
		s.Turn.Started.UTC().Format(time.RFC3339Nano)}
	if kind == "waiting" && s.Turn.Waits > 1 { // a later prompt of the turn (the first keeps its id)
		parts = append(parts, "w"+strconv.Itoa(s.Turn.Waits))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	agterm := firstNonEmpty(s.Turn.AgtermID, s.AgtermID)
	return proto.HookAnswer{ID: kind[:1] + hex.EncodeToString(sum[:10]), Agent: s.Agent, AgtermID: agterm,
		SessionID: s.SessionID, BridgeID: s.BridgeID, TgMsgID: s.Turn.TgMsgID, Text: text, Cwd: s.Cwd}
}

// resumeLocked ends the turn's ⏸ (once): the hub clears it on the board and
// edits the ⏸ message.
func (r *Relay) resumeLocked(s *Session) error {
	if s.Turn == nil || !s.Turn.Notified || s.Turn.Resumed {
		return nil
	}
	a := r.answer(s, "waiting", "")
	a.Of, a.ID = a.ID, a.ID+proto.ResumedSuffix
	if err := r.emit(proto.EventHookResumed, a); err != nil {
		return err
	}
	s.Turn.Resumed = true
	return nil
}

// ResumeGrace: a composer reading taken this soon after the ⏸ hook may predate
// the dialog on screen, so it can't clear the ⏸.
const ResumeGrace = 3 * time.Second

// ObserveComposers ends the ⏸ of every session that a composer reading taken at
// `at` shows off its prompt: a live composer (ready, draft, working) or no
// agent any more (exited, closed: absent from states). A dialog (menu) or an
// unreadable screen (unknown) keeps it. states maps agterm ids (any case) to
// composer states and must come from a complete reading of the host's sessions.
func (r *Relay) ObserveComposers(at time.Time, states map[string]string) {
	up := make(map[string]string, len(states))
	for id, st := range states {
		up[strings.ToUpper(id)] = st
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for _, s := range r.st.Sessions {
		t := s.Turn
		if t == nil || !t.Notified || t.Resumed || at.Sub(t.WaitingAt) < ResumeGrace {
			continue
		}
		id := firstNonEmpty(t.AgtermID, s.AgtermID)
		if id == "" {
			continue
		}
		st, live := up[id]
		if live && st != "ready" && st != "draft" && st != "working" {
			continue
		}
		if err := r.resumeLocked(s); err != nil {
			log.Printf("hookrelay: resume %s: %v", id, err)
			continue
		}
		log.Printf("hookrelay: %s left its prompt (%s): ⏸ for tg %d ends", id, firstNonEmpty(st, "gone"), t.TgMsgID)
		changed = true
	}
	if changed {
		if err := r.saveLocked(); err != nil {
			log.Printf("hookrelay: save state: %v", err) // the event is in the outbox; a repeat keeps its id
		}
	}
}

// Sessions lists the sessions with an agterm id and a claude.ai bridge id,
// newest first, one per agterm id (for the heartbeat: the hub's links).
func (r *Relay) Sessions() []proto.HookSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []*Session
	for _, s := range r.st.Sessions {
		if s.AgtermID != "" {
			all = append(all, s)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Updated.After(all[j].Updated) })
	seen := map[string]bool{}
	out := []proto.HookSession{}
	for _, s := range all {
		if seen[s.AgtermID] || len(out) == 100 {
			continue
		}
		seen[s.AgtermID] = true // the terminal's newest session decides, link or not
		if s.BridgeID == "" {
			continue
		}
		out = append(out, proto.HookSession{AgtermID: s.AgtermID, Agent: s.Agent, SessionID: s.SessionID,
			BridgeID: s.BridgeID, Cwd: s.Cwd, UpdatedAt: s.Updated})
	}
	return out
}

func (r *Relay) pruneLocked(now time.Time) {
	if len(r.st.Sessions) <= keepSessions/2 {
		return
	}
	var all []*Session
	for k, s := range r.st.Sessions {
		if now.Sub(s.Updated) > keepFor {
			delete(r.st.Sessions, k)
			continue
		}
		all = append(all, s)
	}
	if len(all) <= keepSessions {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Updated.After(all[j].Updated) })
	for _, s := range all[keepSessions:] {
		for k, v := range r.st.Sessions {
			if v == s {
				delete(r.st.Sessions, k)
			}
		}
	}
}

func (r *Relay) saveLocked() error {
	b, err := json.Marshal(r.st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.statePath), 0o700); err != nil {
		return err
	}
	tmp := r.statePath + ".tmp"
	if err := writeSync(tmp, b); err != nil {
		return err
	}
	return os.Rename(tmp, r.statePath)
}

// Ingest handles one live request after the spooled (older) ones. When one of
// those can't be handled yet, or this one fails, the request joins the spool
// behind them, so nothing overtakes an older request; an error means it could
// not even be spooled (the hook then spools it itself).
func (r *Relay) Ingest(req Request) error {
	r.ingest.Lock()
	defer r.ingest.Unlock()
	err := r.drainLocked()
	if err == nil {
		if err = r.Handle(req); err == nil || errors.Is(err, ErrBadRequest) {
			return err
		}
	}
	if r.spoolDir == "" {
		return err
	}
	b, merr := json.Marshal(req)
	if merr != nil {
		return merr
	}
	if serr := spool(r.spoolDir, b, req.At); serr != nil {
		return fmt.Errorf("%v; spool: %w", err, serr)
	}
	log.Printf("hookrelay: %s %s kept in the spool: %v", req.Agent, req.Event, err)
	return nil
}

// DrainSpool handles the requests hooks spooled while the host wasn't
// listening, oldest first; it stops at the first one that fails (kept for the
// next drain, so order holds).
func (r *Relay) DrainSpool() error {
	r.ingest.Lock()
	defer r.ingest.Unlock()
	return r.drainLocked()
}

func (r *Relay) drainLocked() error {
	if r.spoolDir == "" {
		return nil
	}
	names, err := filepath.Glob(filepath.Join(r.spoolDir, "*.json"))
	if err != nil || len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	for _, p := range names {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var req Request
		if err := json.Unmarshal(b, &req); err != nil {
			log.Printf("hookrelay: spooled %s unreadable: %v", filepath.Base(p), err)
			os.Rename(p, p+".bad")
			continue
		}
		if err := r.Handle(req); errors.Is(err, ErrBadRequest) {
			log.Printf("hookrelay: spooled %s: %v (set aside)", filepath.Base(p), err)
			os.Rename(p, p+".bad")
			continue
		} else if err != nil {
			log.Printf("hookrelay: spooled %s: %v (kept)", filepath.Base(p), err)
			return err
		}
		os.Remove(p)
		log.Printf("hookrelay: spooled %s %s handled", req.Event, filepath.Base(p))
	}
	return nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
