package hub

// Answers back from interactive sessions (phase 06): a Telegram-driven turn's
// final text (hook_answer) and its "waiting at the Mac" ping (hook_waiting)
// arrive from a host's hookrelay as durable events. Each is stored as a job
// that already has its result, so the usual delivery brings it to the chat
// once, survives restarts, and retries Telegram failures.

import (
	"context"
	"encoding/json"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
	"github.com/AlexDobrushskiy/hommy/internal/speech"
	"github.com/AlexDobrushskiy/hommy/internal/textx"
)

const (
	jobHookAnswer  = proto.EventHookAnswer
	jobHookWaiting = proto.EventHookWaiting
	answerChunk    = 3700 // runes of answer text per message (tag + link fit beside it)
)

func (h *Hub) registerHookResults() {
	h.OnResult(jobHookAnswer, h.deliverHookAnswer)
	h.OnResult(jobHookWaiting, h.deliverHookWaiting)
	h.OnResult(proto.EventHookResumed, h.deliverHookResumed)
}

// handleHookEvent stores a hook report from host and starts its delivery. An
// error means it is not stored: the host keeps re-sending it until acked.
func (h *Hub) handleHookEvent(host string, e proto.Event) error {
	var a proto.HookAnswer
	if err := json.Unmarshal(e.Data, &a); err != nil || a.ID == "" || a.TgMsgID <= 0 {
		log.Printf("hook event from %s dropped (malformed): %s", host, e.Data)
		return nil
	}
	target := ""
	if a.AgtermID != "" {
		target = Target{Kind: KindSession, Host: host, Dir: strings.ToUpper(a.AgtermID)}.String()
	}
	id := newID()
	j := &JobRow{ID: id, EventID: "hook:" + host + ":" + a.ID, Host: host, Lane: "hook:" + id, Target: target,
		Type: e.Kind, Payload: e.Data, ChatID: h.ownerChat(), MsgID: int(a.TgMsgID)}
	added, err := h.st.AddDelivery(j, proto.Result{Status: proto.StatusOK, Data: e.Data}, h.now())
	if err != nil {
		return err
	}
	if !added {
		log.Printf("hook event %s from %s already stored", a.ID, host)
		return nil
	}
	log.Printf("%s %s from %s %s for tg %d (%d chars)", e.Kind, a.ID, host, a.AgtermID, a.TgMsgID, len(a.Text))
	h.addTyping(j.ChatID) // paired with deliver's removeTyping
	h.goDeliver(j)
	return nil
}

// hookLabel names the session a hook report came from: its target label
// ("e2e-claude · mac2"), else the short session id.
func (h *Hub) hookLabel(host string, a proto.HookAnswer) string {
	if a.AgtermID != "" {
		return h.label(Target{Kind: KindSession, Host: host, Dir: strings.ToUpper(a.AgtermID)})
	}
	return "session " + shortID(a.SessionID) + " · " + host
}

// hookLink is a report's claude.ai link: the bridge id the turn ran under,
// else the session's latest one from the heartbeat.
func (h *Hub) hookLink(host string, a proto.HookAnswer) string {
	if l := proto.ClaudeLink(a.BridgeID); l != "" {
		return l
	}
	return h.linkFor(host, a.AgtermID)
}

// sessionLink is a session target's claude.ai link ("" when unknown or not a
// session: -p targets have none).
func (h *Hub) sessionLink(t Target) string {
	if t.Kind != KindSession {
		return ""
	}
	return h.linkFor(t.Host, t.Dir)
}

// linkFor looks the agterm session up in the host's heartbeat (the hooks
// report each session's bridge id); an id prefix works like elsewhere.
func (h *Hub) linkFor(host, agtermID string) string {
	if agtermID == "" {
		return ""
	}
	return h.hookLinks(host)[strings.ToUpper(agtermID)]
}

// hookLinks maps a host's agterm session ids to their claude.ai links.
func (h *Hub) hookLinks(host string) map[string]string {
	out := map[string]string{}
	row, err := h.st.Host(host)
	if err != nil {
		return out
	}
	for _, s := range row.Heartbeat.Hooks {
		if l := proto.ClaudeLink(s.BridgeID); l != "" {
			if _, dup := out[strings.ToUpper(s.AgtermID)]; !dup { // newest first
				out[strings.ToUpper(s.AgtermID)] = l
			}
		}
	}
	return out
}

// jobLink is the link line for a message about a job's session target.
func (h *Hub) jobLink(j *JobRow) string {
	t, err := ParseTarget(j.Target)
	if err != nil {
		return ""
	}
	return linkLine(h.sessionLink(t))
}

// richText builds message text with text_link entities (UTF-16 offsets).
type richText struct {
	b    strings.Builder
	n    int
	ents []models.MessageEntity
}

func (r *richText) Write(p []byte) (int, error) {
	r.WriteString(string(p))
	return len(p), nil
}

func (r *richText) WriteString(s string) {
	r.b.WriteString(s)
	r.n += u16len(s)
}

// link writes s, linked to url when it is known.
func (r *richText) link(s, url string) {
	if url != "" && s != "" {
		r.ents = append(r.ents, models.MessageEntity{Type: models.MessageEntityTypeTextLink, Offset: r.n, Length: u16len(s), URL: url})
	}
	r.WriteString(s)
}

func (r *richText) String() string { return r.b.String() }

// linkLine is the visible link appended to a message about a session.
func linkLine(link string) string {
	if link == "" {
		return ""
	}
	return "\n🔗 " + link
}

// taggedHead is "<prefix><label>\n" with the label linked to the session.
func taggedHead(prefix, label, link string) (string, []models.MessageEntity) {
	head := prefix + label
	var ents []models.MessageEntity
	if link != "" {
		ents = []models.MessageEntity{{Type: models.MessageEntityTypeTextLink, Offset: u16len(prefix), Length: u16len(label), URL: link}}
	}
	return head, ents
}

// deliverHookAnswer posts the turn's final text as a reply to the owner's message:
// every chunk tagged `🚗 <label>` (linked), the last one ending in the
// claude.ai link; a voice note follows when the text carries a <tts> block.
func (h *Hub) deliverHookAnswer(ctx context.Context, j *JobRow, r proto.Result) error {
	var a proto.HookAnswer
	json.Unmarshal(r.Data, &a)
	if err := h.awaitStartNotice(ctx, j, a.AgtermID); err != nil { // "🆕 … started" first
		return err
	}
	lbl, link := h.hookLabel(j.Host, a), h.hookLink(j.Host, a)
	text, spoken, lang := speech.ExtractTTS(a.Text)
	if strings.TrimSpace(text) == "" {
		text = "(empty reply)"
	}
	head, ents := taggedHead("🚗 ", lbl, link)
	chunks := textx.Chunk(strings.TrimSpace(text), answerChunk)
	for i, c := range chunks {
		msg := head + "\n" + c
		if i == len(chunks)-1 && link != "" {
			msg += "\n" + linkLine(link)
		}
		replyTo := 0
		if i == 0 {
			replyTo = j.MsgID
		}
		if err := h.part(j, i, func() error {
			return h.sendOut(ctx, j, a.SessionID, "answer", Out{Text: msg, Entities: ents, ReplyTo: replyTo, NoPreview: true})
		}); err != nil {
			return err
		}
	}
	if spoken == "" || h.cfg.TTS.URL == "" {
		return nil
	}
	return h.voicePart(ctx, j, len(chunks), a.SessionID, spoken, lang)
}

// deliverHookWaiting is the ⏸ ping (one per turn): the session waits for a
// person at the Mac. "Open" opens the Mini App's view of that session.
func (h *Hub) deliverHookWaiting(ctx context.Context, j *JobRow, r proto.Result) error {
	var a proto.HookAnswer
	json.Unmarshal(r.Data, &a)
	if err := h.awaitStartNotice(ctx, j, a.AgtermID); err != nil {
		return err
	}
	h.pauseMu.Lock()
	defer h.pauseMu.Unlock()
	end, err := h.hookJob(j.Host, a.ID+proto.ResumedSuffix)
	if err != nil {
		return err
	}
	msg, ents := h.waitingText(j.Host, a, end != nil) // over before it went out (both came after an outage)
	o := Out{Text: msg, Entities: ents, ReplyTo: j.MsgID, NoPreview: true}
	if h.cfg.MiniAppURL != "" && j.Target != "" {
		o.Inline = &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{{
			Text: "Open", WebApp: &models.WebAppInfo{URL: h.cfg.MiniAppURL + "/console?target=" + url.QueryEscape(j.Target)}}}}}
	}
	kind := "waiting"
	if end != nil {
		kind = "waited" // nothing left to edit
	}
	return h.part(j, 0, func() error { return h.sendOut(ctx, j, a.SessionID, kind, o) })
}

// sendOut sends a message that belongs to a job's target/session and records it.
func (h *Hub) sendOut(ctx context.Context, j *JobRow, sessionID, kind string, o Out) error {
	id, err := h.tg.SendOut(ctx, j.ChatID, o)
	if err != nil {
		log.Printf("send: %v", err)
		return err
	}
	h.record(Message{ChatID: j.ChatID, MsgID: id, Direction: "out", Kind: kind,
		Target: j.Target, SessionID: sessionID, JobID: j.ID})
	return nil
}

// voicePart synthesizes and sends an answer's voice note as message n, after
// its text went out (text never waits for the voice). A failed synthesis is
// said in a line instead.
func (h *Hub) voicePart(ctx context.Context, j *JobRow, n int, sessionID, spoken, lang string) error {
	if n < j.Delivered {
		return nil
	}
	b := h.backendFor(lang)
	started := h.now()
	voice, err := h.sp.Synthesize(ctx, b, spoken)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		log.Printf("tts synthesis failed after %s for %d chars lang=%q via %s: %v",
			h.now().Sub(started).Round(time.Second), len([]rune(spoken)), lang, b.URL, err)
		return h.part(j, n, func() error { return h.sendFor(ctx, j, sessionID, "⚠️ voice generation failed", 0) })
	}
	return h.part(j, n, func() error {
		id, err := h.tg.SendVoice(ctx, j.ChatID, voice, j.MsgID)
		if err != nil {
			log.Printf("sendVoice: %v", err)
			if permanent(err) {
				h.sendFor(ctx, j, sessionID, "⚠️ voice send failed", 0)
			}
			return err
		}
		h.record(Message{ChatID: j.ChatID, MsgID: id, Direction: "out", Kind: "voice",
			Target: j.Target, SessionID: sessionID, JobID: j.ID})
		return nil
	})
}
