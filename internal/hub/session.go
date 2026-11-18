package hub

// Agent sessions (phase 04): messages to a session@<host>:<agterm id> target
// become `type` jobs; their results and late outcomes become acks on the owner's
// message (✍ typed · 👌 in the inbox, typing retried · 🤷 no live session).
// Dev commands /spawn, /peek, /files, /pause, /done drive the other jobs.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// KindSession targets a live interactive session: session@<host>:<agterm id>.
const KindSession = "session"

var sessionIDRe = regexp.MustCompile(`^[0-9A-Fa-f][0-9A-Fa-f-]{3,35}$`)

func parseSessionTarget(s, rest string) (Target, error) {
	host, id, ok := strings.Cut(rest, ":")
	if !ok || !hostRe.MatchString(host) || !sessionIDRe.MatchString(strings.TrimSpace(id)) {
		return Target{}, fmt.Errorf("target %q: want session@<host>:<agterm session id>", s)
	}
	return Target{Kind: KindSession, Host: host, Dir: strings.ToUpper(strings.TrimSpace(id))}, nil
}

func (h *Hub) registerSessionResults() {
	h.OnResult(proto.JobType, h.deliverType)
	h.OnResult(proto.JobNewSession, h.deliverNewSession)
	h.OnResult(proto.JobPeek, h.deliverPeek)
	h.OnResult(proto.JobFiles, h.deliverBrief)
	h.OnResult(proto.JobPause, h.deliverBrief)
	h.OnResult(proto.JobDone, h.deliverBrief)
}

// quoteOf is the head of the message the owner replied to, for the typed prefix.
func quoteOf(msg *models.Message) string {
	if msg == nil || msg.ReplyToMessage == nil {
		return ""
	}
	if q := msg.ReplyToMessage.Text; q != "" {
		return q
	}
	return msg.ReplyToMessage.Caption
}

// lineMeta is what the inbox keeps about the Telegram message behind a line.
type lineMeta struct {
	Kind    string `json:"kind,omitempty"`
	Quote   string `json:"quote,omitempty"`
	ReplyTo int    `json:"reply_to,omitempty"`
	ID      string `json:"id,omitempty"` // inbox item id (reactions, buttons); default tg-<msg>
}

// metaOf describes the message behind a line; a reply to an agent's message
// quotes it as `🤖 slug: body`.
func (h *Hub) metaOf(msg *models.Message) lineMeta {
	m := lineMeta{Kind: messageKind(msg), Quote: quoteOf(msg)}
	if msg.ReplyToMessage != nil {
		m.ReplyTo = msg.ReplyToMessage.ID
		m.Quote = h.pingQuote(msg.Chat.ID, m.ReplyTo, m.Quote)
	}
	return m
}

// laneFor is the lane of a job for target t. Lines (and files) for agent
// sessions share one lane per host, so the host gets them in the owner's order, one
// at a time, whichever session each names: an agent's lines may name an ended
// session and its successor, and the host's queue keeps the agent's order only
// for what it has received. A `type` job returns after one attempt, so the
// lane is held only briefly.
func laneFor(t Target) string {
	if t.Kind == KindSession {
		return "type:" + t.Host
	}
	return t.String()
}

// addType stores a `type` job for a session target (the run_p path's twin).
func (h *Hub) addType(ctx context.Context, e *Event, msg *models.Message, t Target, text string, sub int, suffix string) error {
	return h.addTypeMeta(ctx, e, msg, t, text, sub, suffix, h.metaOf(msg))
}

func (h *Hub) addTypeMeta(ctx context.Context, e *Event, msg *models.Message, t Target, text string, sub int, suffix string, m lineMeta) error {
	payload, _ := json.Marshal(agterm.TypeRequest{Session: t.Dir, MsgID: int64(msg.ID), Text: text,
		Quote: m.Quote, Kind: m.Kind, ReplyTo: int64(m.ReplyTo), ID: m.ID,
		BriefDir: h.pingBriefDir(msg.Chat.ID, m.ReplyTo)})
	j := &JobRow{ID: newID(), EventID: e.EventID + suffix, Seq: e.Seq, Sub: sub, Host: t.Host, Lane: laneFor(t),
		Target: t.String(), Type: proto.JobType, Payload: payload, ChatID: msg.Chat.ID, MsgID: msg.ID}
	return h.addJob(ctx, j)
}

func (h *Hub) deliverType(ctx context.Context, j *JobRow, r proto.Result) error {
	var d agterm.TypeResult
	_ = json.Unmarshal(r.Data, &d)
	switch r.Status {
	case proto.StatusOK:
		h.react(ctx, j.ChatID, j.MsgID, "✍")
	case proto.StatusInbox:
		h.react(ctx, j.ChatID, j.MsgID, "👌")
	case proto.StatusNoSession:
		h.react(ctx, j.ChatID, j.MsgID, "🤷")
	default:
		return h.part(j, 0, func() error {
			return h.sendFor(ctx, j, "", fmt.Sprintf("⚠️ typing into %s failed: %s", j.Target, firstNonEmpty(r.Error, d.Detail))+h.jobLink(j), j.MsgID)
		})
	}
	return nil
}

// outcomePart is the message index of a late outcome: after part 0 (a failed
// result), so a repeated outcome is said once.
const outcomePart = 1

// handleTypeOutcome turns a late outcome of a queued line into an ack. The host
// repeats the event until the hub acks it, so a repeat must change nothing; an
// error means "not handled, send it again".
func (h *Hub) handleTypeOutcome(ctx context.Context, e proto.Event) error {
	j, err := h.st.Job(e.JobID)
	if err != nil {
		return err
	}
	if j == nil {
		log.Printf("type outcome for unknown job %s", e.JobID)
		return nil
	}
	var o proto.TypeOutcome
	_ = json.Unmarshal(e.Data, &o)
	switch o.Outcome {
	case "delivered", "unconfirmed":
		h.react(ctx, j.ChatID, j.MsgID, "✍")
		return nil
	case "expired":
		if o.NoSession { // 🤷 already said it; the successor finds it in the inbox
			log.Printf("job %s: no successor took the line within the retry window; left in the inbox", j.ID)
			return nil
		}
		return h.part(j, outcomePart, func() error {
			return h.sendFor(ctx, j, "", fmt.Sprintf("⌛ %s stayed busy for 30 min; not typed. It is in the session's inbox.", j.Target)+h.jobLink(j), j.MsgID)
		})
	default:
		return h.part(j, outcomePart, func() error {
			return h.sendFor(ctx, j, "", fmt.Sprintf("⚠️ typing into %s failed: %s", j.Target, o.Detail)+h.jobLink(j), j.MsgID)
		})
	}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---- dev commands ----

const sessionHelp = "\n/target session@<host>:<agterm id> — talk to an interactive session" +
	"\n/spawn <host>:<dir> <slug> <first message> — new Claude session (workspace telegram)" +
	"\n/peek [session@<host>:<id>] [lines] · /files [session@<host>:<id>] [path] [tail]" +
	"\n/pause · /done [session@<host>:<id>]"

// sessionCommand handles the agent-session commands; false = not one of them.
func (h *Hub) sessionCommand(ctx context.Context, e *Event, msg *models.Message, name, args string) (bool, error) {
	switch name {
	case "spawn":
		return true, h.spawnCommand(ctx, e, msg, args)
	case "peek", "files", "pause", "done":
	default:
		return false, nil
	}
	fields := strings.Fields(args)
	var t Target
	if len(fields) > 0 && strings.HasPrefix(fields[0], KindSession+"@") {
		var err error
		if t, err = ParseTarget(fields[0]); err != nil {
			h.say(ctx, msg.Chat.ID, "⚠️ "+err.Error(), msg.ID)
			return true, nil
		}
		fields = fields[1:]
	} else {
		cur, err := h.currentTarget(msg.Chat.ID)
		if err != nil {
			return true, err
		}
		if cur.Kind != KindSession {
			h.say(ctx, msg.Chat.ID, "⚠️ /"+name+" needs a session: /"+name+" session@<host>:<id>, or /target one first", msg.ID)
			return true, nil
		}
		t = cur
	}
	if _, ok := h.cfg.HostKeys[t.Host]; !ok {
		h.say(ctx, msg.Chat.ID, fmt.Sprintf("⚠️ unknown host %q", t.Host), msg.ID)
		return true, nil
	}
	var typ string
	var payload []byte
	switch name {
	case "peek":
		n := 30
		if len(fields) > 0 {
			if v, err := strconv.Atoi(fields[0]); err == nil {
				n = v
			}
		}
		typ = proto.JobPeek
		payload, _ = json.Marshal(agterm.PeekRequest{Session: t.Dir, Lines: n})
	case "files":
		req := agterm.BriefRequest{Session: t.Dir}
		if len(fields) > 0 {
			req.Path = fields[0]
		}
		if len(fields) > 1 {
			req.Tail, _ = strconv.Atoi(fields[1])
		}
		typ = proto.JobFiles
		payload, _ = json.Marshal(req)
	case "pause":
		typ = proto.JobPause
		payload, _ = json.Marshal(agterm.BriefRequest{Session: t.Dir})
	case "done":
		typ = proto.JobDone
		payload, _ = json.Marshal(agterm.BriefRequest{Session: t.Dir})
	}
	return true, h.addControl(ctx, e, msg, t, typ, payload)
}

// addControl stores a one-off job in its own lane, so it never waits behind a
// session's typing.
func (h *Hub) addControl(ctx context.Context, e *Event, msg *models.Message, t Target, typ string, payload []byte) error {
	id := newID()
	j := &JobRow{ID: id, EventID: e.EventID, Seq: e.Seq, Host: t.Host, Lane: "ctl:" + id,
		Target: t.String(), Type: typ, Payload: payload, ChatID: msg.Chat.ID, MsgID: msg.ID}
	return h.addJob(ctx, j)
}

const maxChatText = 3800

func clip(s string) string {
	if len(s) <= maxChatText {
		return s
	}
	cut := s[len(s)-maxChatText:]
	for !utf8.ValidString(cut) {
		cut = cut[1:]
	}
	return "…" + cut
}

func (h *Hub) deliverPeek(ctx context.Context, j *JobRow, r proto.Result) error {
	if h.handOver(j, r) {
		return nil // the console view asked, not the chat
	}
	var d agterm.PeekResult
	_ = json.Unmarshal(r.Data, &d)
	text := fmt.Sprintf("🖥 %s · %s\n%s", j.Target, d.Composer.State, d.Text)
	if r.Status != proto.StatusOK {
		text = fmt.Sprintf("⚠️ peek %s: %s %s", j.Target, r.Status, firstNonEmpty(r.Error, d.Error))
	}
	return h.part(j, 0, func() error { return h.sendFor(ctx, j, "", clip(text), j.MsgID) })
}

func (h *Hub) deliverBrief(ctx context.Context, j *JobRow, r proto.Result) error {
	if h.handOver(j, r) {
		return nil // the console asked, not the chat
	}
	var d agterm.BriefResult
	_ = json.Unmarshal(r.Data, &d)
	var text string
	switch {
	case r.Status != proto.StatusOK:
		text = fmt.Sprintf("⚠️ %s %s: %s", j.Type, j.Target, firstNonEmpty(d.Error, r.Error))
	case j.Type == proto.JobFiles && d.Path == "":
		var b strings.Builder
		fmt.Fprintf(&b, "📁 %s\n", d.BriefDir)
		for _, f := range d.Files {
			fmt.Fprintf(&b, "• %s (%d B)\n", f.Name, f.Size)
		}
		text = b.String()
	case j.Type == proto.JobFiles:
		text = fmt.Sprintf("📄 %s\n%s", d.Path, d.Content)
	case j.Type == proto.JobPause:
		text = "⏸ paused: " + d.Path
	default:
		text = "✅ marked done: " + d.Path
	}
	return h.part(j, 0, func() error { return h.sendFor(ctx, j, "", clip(text), j.MsgID) })
}

// ---- /status ----

// maxStatusSessions keeps /status under Telegram's 4096-char message limit with three busy hosts.
const maxStatusSessions = 15

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// sessionLines lists a host's Claude Code / Codex sessions from its last heartbeat.
func sessionLines(raw json.RawMessage) string {
	var b richText
	writeSessionLines(&b, raw, nil)
	return b.String()
}

// writeSessionLines writes the session lines; a session whose claude.ai link
// is known (links: agterm id → link) gets its name linked.
func writeSessionLines(b *richText, raw json.RawMessage, links map[string]string) {
	if len(raw) == 0 {
		return
	}
	var snap agterm.Snapshot
	if json.Unmarshal(raw, &snap) != nil {
		return
	}
	shown, more := 0, 0
	for _, s := range snap.Sessions {
		if s.Agent == "" {
			continue
		}
		if shown == maxStatusSessions {
			more++
			continue
		}
		shown++
		name := s.Name
		if name == "" {
			name = s.Workspace
		}
		fmt.Fprintf(b, "   ◦ %s ", s.ID[:min(8, len(s.ID))])
		b.link(clipRunes(name, 40), links[strings.ToUpper(s.ID)])
		fmt.Fprintf(b, " · %s · %s\n", s.Agent, s.State)
	}
	if more > 0 {
		fmt.Fprintf(b, "   ◦ … +%d more\n", more)
	}
}
