package hub

// Agent pings (phase 07): `hommy send` → /api/send (the hub sends and
// records message ↔ session), replies / reactions / buttons on those messages
// → the agent's session (router rules 1–3), `hommy inbox ack` → /api/ack
// (🫡), and the spool a host forwards for what the hub missed while it was down.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/ping"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// AnswerTTL bounds the "Answer…" context: an unused one expires, so it can't
// catch a message sent much later.
const AnswerTTL = 15 * time.Minute

// An agent that sent while the hub was down (direct + spool) can get the owner's
// press, reaction or reply before its host flushes the spool: such an update
// is parked (errParked → EventParked) and looks again when a spool entry
// lands, or after ParkTTL goes where it would have gone without one.
const (
	ParkTTL = 5 * time.Minute
	// a reply to an unknown agent message older than this isn't parked (an
	// old ping from before the hub: nothing will be spooled for it)
	parkReplyAge = 24 * time.Hour
)

var errParked = errors.New("parked: waiting for the agent message's spool entry")

// parkable: e is young enough to wait for a spool entry.
func (h *Hub) parkable(e *Event) bool {
	return h.now().Sub(time.Unix(0, int64(e.Created*1e9))) < ParkTTL
}

// unknownMessage: the hub has no record of msgID at all.
func (h *Hub) unknownMessage(chat int64, msgID int) bool {
	m, err := h.st.GetMessage(chat, msgID)
	return err == nil && m == nil
}

// wakeParked: agent messages were just recorded from a spool; parked events look again.
func (h *Hub) wakeParked() {
	h.spoolGen.Add(1)
	if err := h.st.UnparkEvents(h.now().Add(24 * time.Hour)); err != nil { // all of them
		log.Printf("unpark: %v", err)
	}
	h.wake()
}

// RuleAnswer: the one-shot context an "Answer…" button set (checked after rule
// 3, before the current target).
const RuleAnswer = 5

// ---- host API ----

const maxAPIBody = 1 << 20

// apiHost authenticates a host API request (the WebSocket's key check).
func (h *Hub) apiHost(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.Header.Get("X-Hommy-Host")
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return "", false
	}
	if !h.cfg.checkKey(name, key) {
		log.Printf("api %s: rejected host %q from %s", r.URL.Path, name, r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	return name, true
}

func (h *Hub) serveSend(w http.ResponseWriter, r *http.Request) {
	host, ok := h.apiHost(w, r)
	if !ok {
		return
	}
	var req proto.SendRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxAPIBody)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, proto.SendResponse{Error: "bad request: " + err.Error()})
		return
	}
	if req.Agent != nil {
		req.Agent.Host = host
	}
	if err := validSend(req); err != nil {
		writeJSON(w, http.StatusBadRequest, proto.SendResponse{Error: err.Error()})
		return
	}
	res, err := h.SendPing(r.Context(), req)
	if err != nil {
		code := http.StatusBadGateway // Telegram refused or failed
		res.Error = err.Error()
		writeJSON(w, code, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func validSend(req proto.SendRequest) error {
	if strings.TrimSpace(req.Text) == "" {
		return fmt.Errorf("empty message")
	}
	if req.ID == "" || len(req.ID) > 64 {
		return fmt.Errorf("id is required (≤ 64 chars)")
	}
	if len(req.Buttons) > ping.MaxButtons {
		return fmt.Errorf("at most %d buttons", ping.MaxButtons)
	}
	for _, b := range req.Buttons {
		if strings.TrimSpace(b) == "" || len([]rune(b)) > ping.MaxLabelRunes {
			return fmt.Errorf("bad button label %q", b)
		}
	}
	if a := req.Agent; a != nil {
		if !ping.SlugRe.MatchString(a.Slug) {
			return fmt.Errorf("bad agent slug %q", a.Slug)
		}
		if a.Session != "" && !sessionIDRe.MatchString(a.Session) {
			return fmt.Errorf("bad session id %q", a.Session)
		}
		if a.Link != "" && !strings.HasPrefix(a.Link, ping.LinkPrefix) {
			return fmt.Errorf("bad link %q", a.Link)
		}
	}
	if (req.Agent == nil || req.Plain) && (len(req.Buttons) > 0 || req.Ask) {
		return fmt.Errorf("buttons and --ask need --agent")
	}
	return nil
}

// sendProgress is a request id's state (kv send:<id>), saved after every part
// so a retry resumes instead of sending a part twice.
type sendProgress struct {
	Done       bool     `json:"done"`
	MessageIDs []int    `json:"message_ids"`
	Buttons    []string `json:"buttons_pending,omitempty"` // labels that didn't attach to the last part
}

func (h *Hub) loadSend(key string) sendProgress {
	var p sendProgress
	if v, err := h.st.Get(key); err == nil && v != "" {
		_ = json.Unmarshal([]byte(v), &p)
	}
	return p
}

func (h *Hub) saveSend(key string, p sendProgress) {
	if err := h.st.Set(key, mustJSON(p)); err != nil {
		log.Printf("%s: %v", key, err)
	}
}

const buttonsWarning = "buttons not attached: "

// SendPing sends a message for an agent (or a plain one), records each part
// before returning, and answers a repeated request id with the first ids
// (resuming a send that failed part way, retrying buttons that didn't attach).
func (h *Hub) SendPing(ctx context.Context, req proto.SendRequest) (proto.SendResponse, error) {
	chat := h.ownerChat()
	if chat == 0 {
		return proto.SendResponse{}, errUnpaired
	}
	key := "send:" + req.ID
	h.sendMu.Lock() // one send per request id at a time (a retried POST waits for the first)
	defer h.sendMu.Unlock()
	prog := h.loadSend(key)
	res := proto.SendResponse{OK: true, ChatID: chat, MessageIDs: prog.MessageIDs}
	open := ""
	if !req.Plain {
		open = h.pingOpenURL(req.Agent)
	}
	if prog.Done {
		if len(prog.Buttons) > 0 && len(prog.MessageIDs) > 0 {
			last := prog.MessageIDs[len(prog.MessageIDs)-1]
			if err := h.setButtons(ctx, chat, last, prog.Buttons, open); err != nil {
				res.Warning = buttonsWarning + err.Error()
			} else {
				prog.Buttons = nil
				h.saveSend(key, prog)
			}
		}
		return res, nil
	}
	text, limit := strings.TrimSpace(req.Text), ping.PlainLimit
	buttons := req.Buttons
	if a := req.Agent; a != nil && !req.Plain {
		text, limit = ping.Text(a.Slug, a.Host, a.Link, text), ping.AgentLimit
		if req.Ask {
			buttons = ping.WithAnswer(buttons)
		}
	}
	parts := ping.Chunk(text, limit)
	for i, part := range parts {
		if i < len(prog.MessageIDs) {
			continue // sent by an earlier try of this id
		}
		last := i == len(parts)-1
		o := SendOpts{Silent: req.Silent}
		var partButtons []string
		switch {
		case last && len(buttons) > 0:
			o.Bare, partButtons = true, buttons
		case last && open != "":
			o.Inline = ping.OpenRow(open)
		}
		id, err := h.tg.SendPing(ctx, chat, part, o)
		if err != nil {
			res.OK = false
			return res, fmt.Errorf("send part %d/%d: %w", i+1, len(parts), err)
		}
		res.MessageIDs = append(res.MessageIDs, id)
		prog.MessageIDs = res.MessageIDs
		h.saveSend(key, prog)
		if err := h.recordPing(chat, id, part, req.Agent, partButtons); err != nil {
			log.Printf("record ping %d: %v", id, err)
		}
		if len(partButtons) > 0 {
			if err := h.setButtons(ctx, chat, id, partButtons, open); err != nil {
				log.Printf("buttons on %d: %v", id, err)
				res.Warning, prog.Buttons = buttonsWarning+err.Error(), partButtons
			}
		}
	}
	prog.Done = true
	h.saveSend(key, prog)
	log.Printf("send %s: %d message(s) %v agent=%v", req.ID, len(res.MessageIDs), res.MessageIDs, req.Agent != nil)
	return res, nil
}

// pingOpenURL is the console link an agent's message carries ("" when the
// session isn't known or there is no Mini App).
func (h *Hub) pingOpenURL(a *proto.Agent) string {
	if a == nil || a.Session == "" || a.Host == "" {
		return ""
	}
	return h.consoleURL(Target{Kind: KindSession, Host: a.Host, Dir: strings.ToUpper(a.Session)})
}

// consoleURL is the Mini App console of a session target.
func (h *Hub) consoleURL(t Target) string {
	if h.cfg.MiniAppURL == "" || t.Kind != KindSession || t.Dir == "" {
		return ""
	}
	return h.cfg.MiniAppURL + "/console?target=" + url.QueryEscape(t.String())
}

// setButtons attaches the inline keyboard (ag:<msgid>:<i>, then "Open" when
// open is set), retried briefly: the message id is only known after the send.
func (h *Hub) setButtons(ctx context.Context, chat int64, id int, labels []string, open string) error {
	var err error
	rows := append(ping.Keyboard(id, labels), ping.OpenRow(open)...)
	for attempt := 0; attempt < 3; attempt++ {
		if err = h.tg.SetButtons(ctx, chat, id, rows); err == nil || permanent(err) {
			return err
		}
		if !h.sleepRetry(ctx, attempt) {
			return ctx.Err()
		}
	}
	return err
}

// recordPing links a sent message to its agent (and, through messages, to
// its session target, which the router reads).
func (h *Hub) recordPing(chat int64, id int, text string, a *proto.Agent, buttons []string) error {
	m := Message{ChatID: chat, MsgID: id, Direction: "out", Kind: "ping"}
	if a == nil {
		m.Kind = KindPlain // an agent's message with no session: a reply has nowhere to go
		return h.st.AddMessage(m, h.now())
	}
	if a.Session != "" {
		m.Target = Target{Kind: KindSession, Host: a.Host, Dir: strings.ToUpper(a.Session)}.String()
	}
	if err := h.st.AddMessage(m, h.now()); err != nil {
		return err
	}
	body := text
	if _, rest, ok := strings.Cut(text, "\n"); ok && strings.HasPrefix(text, "🤖 ") {
		body = rest
	}
	if a.Link != "" {
		body = strings.TrimSuffix(strings.TrimSuffix(body, a.Link), "\n")
	}
	p := Ping{ChatID: chat, MsgID: id, Slug: a.Slug, Host: a.Host, Session: strings.ToUpper(a.Session),
		BriefDir: a.BriefDir, Text: text, Body: body, Buttons: buttons}
	return h.st.AddPing(p, h.now())
}

// serveAck is `hommy inbox ack`: 🫡 on the message the item came from.
func (h *Hub) serveAck(w http.ResponseWriter, r *http.Request) {
	host, ok := h.apiHost(w, r)
	if !ok {
		return
	}
	var req proto.AckRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxAPIBody)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad request: " + err.Error()})
		return
	}
	if err := h.agentAck(r.Context(), host, req); err != nil {
		code := http.StatusBadGateway // Telegram or storage, for now: the CLI spools it and the host retries
		if errors.Is(err, errFinalAck) {
			code = http.StatusBadRequest
		}
		writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// errFinalAck: an ack that can never succeed (unknown message, Telegram refused it).
var errFinalAck = errors.New("ack refused")

func (h *Hub) agentAck(ctx context.Context, host string, req proto.AckRequest) error {
	chat := h.ownerChat()
	m, err := h.st.GetMessage(chat, req.MsgID)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("%w: unknown message %d", errFinalAck, req.MsgID)
	}
	log.Printf("inbox ack %s from %s → 🫡 on %d", req.Item, host, req.MsgID)
	if err := h.tg.React(ctx, chat, req.MsgID, "🫡"); err != nil {
		if permanent(err) {
			return fmt.Errorf("%w: %v", errFinalAck, err)
		}
		return err
	}
	return nil
}

// botID is the hub's bot (its token's prefix).
func (h *Hub) botID() int64 {
	id, _ := strconv.ParseInt(strings.SplitN(h.cfg.TelegramToken, ":", 2)[0], 10, 64)
	return id
}

// handleSpool records what a host forwards from its spool; an error means
// "send it again".
func (h *Hub) handleSpool(ctx context.Context, host string, e proto.Event) error {
	var s proto.SpoolEntry
	if err := json.Unmarshal(e.Data, &s); err != nil {
		log.Printf("spool %s from %s: undecodable, dropped: %v", e.JobID, host, err)
		return nil
	}
	if chat := h.ownerChat(); chat == 0 || s.ChatID != chat {
		log.Printf("spool %s from %s: chat %d, dropped", s.ID, host, s.ChatID)
		return nil
	}
	// message ids belong to one bot: never cross dev and live. Bot 0 is a joined
	// host without a token: it never sends directly, only spools acks for this hub.
	if s.Bot != h.botID() && !(s.Bot == 0 && s.Kind == proto.SpoolAck) {
		log.Printf("spool %s from %s: sent with bot %d, not this hub's, dropped", s.ID, host, s.Bot)
		return nil
	}
	switch s.Kind {
	case proto.SpoolSent:
		if s.Agent != nil {
			s.Agent.Host = host
			if !ping.SlugRe.MatchString(s.Agent.Slug) || (s.Agent.Session != "" && !sessionIDRe.MatchString(s.Agent.Session)) {
				log.Printf("spool %s from %s: bad agent %+v, dropped", s.ID, host, s.Agent)
				return nil
			}
		}
		for i, id := range s.MessageIDs {
			var text string
			if i < len(s.Texts) {
				text = s.Texts[i]
			}
			var buttons []string
			if i == len(s.MessageIDs)-1 {
				buttons = s.Buttons
			}
			if err := h.recordPing(s.ChatID, id, text, s.Agent, buttons); err != nil {
				return fmt.Errorf("record %d: %w", id, err) // idempotent: the resend records the rest
			}
		}
		log.Printf("spool %s from %s: recorded %d message(s) %v", s.ID, host, len(s.MessageIDs), s.MessageIDs)
		h.wakeParked()
	case proto.SpoolAck:
		if s.Ack == nil {
			return nil
		}
		if err := h.agentAck(ctx, host, *s.Ack); err != nil {
			if !errors.Is(err, errFinalAck) {
				return err
			}
			log.Printf("spool ack %s from %s: %v, dropped", s.ID, host, err)
		}
	default:
		log.Printf("spool %s from %s: unknown kind %q, dropped", s.ID, host, s.Kind)
	}
	return nil
}

// ---- quotes and folders for lines typed into an agent ----

// pingQuote is the quoted head of a message for a typed line: `🤖 slug:
// body` for an agent's message, else its text.
func (h *Hub) pingQuote(chat int64, msgID int, fallback string) string {
	if msgID == 0 {
		return fallback
	}
	p, err := h.st.GetPing(chat, msgID)
	if err != nil || p == nil {
		return fallback
	}
	return ping.Quote(p.Slug, p.Body)
}

// pingBriefDir is the brief folder of the agent that wrote msgID ("" if none).
func (h *Hub) pingBriefDir(chat int64, msgID int) string {
	if msgID == 0 {
		return ""
	}
	p, err := h.st.GetPing(chat, msgID)
	if err != nil || p == nil {
		return ""
	}
	return p.BriefDir
}

// ---- reactions (rule 2) ----

func emojis(rs []models.ReactionType) []string {
	var out []string
	for _, r := range rs {
		if r.ReactionTypeEmoji != nil {
			out = append(out, r.ReactionTypeEmoji.Emoji)
		}
	}
	return out
}

// handleReaction types the owner's reaction (with its meaning) into the session
// that wrote the message; a vault / -p answer gets it as a prompt.
func (h *Hub) handleReaction(ctx context.Context, e *Event, r *models.MessageReactionUpdated) error {
	line := ping.ReactionLine(emojis(r.OldReaction), emojis(r.NewReaction))
	if line == "" {
		return nil
	}
	rt, err := h.route(r.Chat.ID, &models.Update{MessageReaction: r})
	if err != nil {
		return err
	}
	if rt.Ignore {
		if h.unknownMessage(r.Chat.ID, r.MessageID) && h.parkable(e) {
			return errParked // maybe an agent message sent while the hub was down
		}
		log.Printf("reaction %q on %d: no session wrote it, ignored", line, r.MessageID)
		return nil
	}
	h.markAnswered(r.Chat.ID, r.MessageID)
	msg := &models.Message{ID: r.MessageID, Chat: r.Chat}
	switch rt.Target.Kind {
	case KindSession:
		m := lineMeta{Kind: "reaction", Quote: h.pingQuote(r.Chat.ID, r.MessageID, ""), ReplyTo: r.MessageID,
			ID: fmt.Sprintf("tg-%d-r%d", r.MessageID, e.Seq)}
		return h.addTypeMeta(ctx, e, msg, rt.Target, line, 0, "", m)
	case KindVault, KindP:
		return h.enqueueRunP(ctx, e, msg, rt.Target, fmt.Sprintf("[The owner reacted to your message tg-%d] %s", r.MessageID, line))
	}
	return nil
}

// ---- inline buttons (rule 1) ----

// answerCallback stops the phone's spinner at once (from intake, before the
// event waits its turn); the press itself is handled in order with messages.
func (h *Hub) answerCallback(ctx context.Context, q *models.CallbackQuery) {
	text := "⚠️ unknown button"
	if msgID, i, ok := ping.ParseCallback(q.Data); ok {
		if p, err := h.st.GetPing(h.ownerChat(), msgID); err == nil && p != nil && i < len(p.Buttons) {
			text = "→ " + p.Buttons[i]
			if ping.IsAnswer(p.Buttons[i]) {
				text = "✍ your next message goes to " + p.Slug
			}
		} else if err == nil {
			text = "⚠️ the hub doesn't know this message (yet)"
		}
	}
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := h.tg.AnswerCallback(actx, q.ID, text); err != nil {
		log.Printf("answer callback %s: %v", q.ID, err)
	}
}

type answerCtx struct {
	MsgID int     `json:"msg_id"`
	At    float64 `json:"at"`
}

func answerKey(chat int64) string { return fmt.Sprintf("answer:%d", chat) }

// handleCallback is a press on an agent's button: the message shows "→ label ✓"
// (buttons gone) and the label is typed into the agent; "Answer…" instead
// makes the owner's next message go to the agent.
func (h *Hub) handleCallback(ctx context.Context, e *Event, q *models.CallbackQuery) error {
	chat := h.ownerChat()
	msgID, i, ok := ping.ParseCallback(q.Data)
	if !ok {
		return nil
	}
	p, err := h.st.GetPing(chat, msgID)
	if err != nil {
		return err
	}
	if p == nil && h.parkable(e) {
		return errParked // ag: buttons are agent messages': its spool entry may still come
	}
	if p == nil || i >= len(p.Buttons) {
		log.Printf("callback %q: not an agent button, ignored", q.Data)
		return nil
	}
	label := p.Buttons[i]
	h.markAnswered(chat, msgID)
	if ping.IsAnswer(label) {
		if err := h.st.Set(answerKey(chat), mustJSON(answerCtx{MsgID: msgID, At: ts(h.now())})); err != nil {
			return err
		}
		h.edit(ctx, chat, msgID, ping.Pressed(p.Text, label+" (your next message goes to "+p.Slug+")"))
		h.keepOpen(ctx, p)
		return nil
	}
	h.edit(ctx, chat, msgID, ping.Pressed(p.Text, label))
	h.keepOpen(ctx, p)
	rt, err := h.route(chat, &models.Update{CallbackQuery: q})
	if err != nil {
		return err
	}
	if rt.Ignore || rt.Target.Kind != KindSession {
		log.Printf("callback %q: %s has no session, not typed", q.Data, p.Slug)
		return nil
	}
	msg := &models.Message{ID: msgID, Chat: models.Chat{ID: chat}}
	m := lineMeta{Kind: "button", Quote: ping.Quote(p.Slug, p.Body), ReplyTo: msgID, ID: fmt.Sprintf("tg-%d-b%d", msgID, e.Seq)}
	return h.addTypeMeta(ctx, e, msg, rt.Target, label, 0, "", m)
}

// keepOpen puts the "Open" button back on a pressed agent message (the edit
// that shows the press removes the inline keyboard).
func (h *Hub) keepOpen(ctx context.Context, p *Ping) {
	open := h.consoleURL(Target{Kind: KindSession, Host: p.Host, Dir: p.Session})
	if open == "" {
		return
	}
	if err := h.tg.SetButtons(ctx, p.ChatID, p.MsgID, ping.OpenRow(open)); err != nil {
		log.Printf("open button on %d: %v", p.MsgID, err)
	}
}

// markAnswered: the owner answered this message (a ping's buttons stop counting as
// waiting on the board).
func (h *Hub) markAnswered(chat int64, msgID int) {
	if err := h.st.MarkPingAnswered(chat, msgID, h.now()); err != nil {
		log.Printf("mark answered %d: %v", msgID, err)
	}
}

func (h *Hub) edit(ctx context.Context, chat int64, msgID int, text string) {
	if err := h.tg.EditText(ctx, chat, msgID, text, nil); err != nil {
		log.Printf("edit %d: %v", msgID, err)
	}
}

// answerContext is the live "Answer…" context's message (0 = none).
func (h *Hub) answerContext(chat int64) (int, error) {
	v, err := h.st.Get(answerKey(chat))
	if err != nil || v == "" {
		return 0, err
	}
	var a answerCtx
	if json.Unmarshal([]byte(v), &a) != nil || a.MsgID == 0 {
		return 0, nil
	}
	if h.now().Sub(time.Unix(0, int64(a.At*1e9))) > AnswerTTL {
		return 0, nil
	}
	return a.MsgID, nil
}

func (h *Hub) clearAnswerContext(chat int64) {
	if err := h.st.Set(answerKey(chat), ""); err != nil {
		log.Printf("clear answer context: %v", err)
	}
}
