package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/claude"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// eventLoop processes stored messages one at a time, in arrival order, so a
// text sent after a voice note is queued after it.
func (h *Hub) eventLoop(ctx context.Context) {
	h.loopEvents(ctx, h.wakeEvents, h.drainEvents)
}

// stopLoop processes stop buttons apart from messages: a stop must not wait
// for a voice note ahead of it to download and transcribe.
func (h *Hub) stopLoop(ctx context.Context) {
	h.loopEvents(ctx, h.wakeStops, h.drainStops)
}

func (h *Hub) loopEvents(ctx context.Context, wake <-chan struct{}, drain func(context.Context)) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-t.C:
		}
	}
}

func (h *Hub) drainEvents(ctx context.Context) {
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	// parked longer than ParkTTL: processed without waiting any more
	if err := h.st.UnparkEvents(h.now().Add(-ParkTTL)); err != nil {
		log.Printf("unpark: %v", err)
	}
	h.drain(ctx, h.st.NextEvent)
}

func (h *Hub) drainStops(ctx context.Context) {
	h.stopsMu.Lock()
	defer h.stopsMu.Unlock()
	h.drain(ctx, h.st.NextStop)
}

func (h *Hub) drain(ctx context.Context, next func() (*Event, error)) {
	for ctx.Err() == nil {
		e, err := next()
		if err != nil {
			log.Printf("next event: %v", err)
			return
		}
		if e == nil {
			return
		}
		status, errText := EventDone, ""
		gen := h.spoolGen.Load()
		err = h.processEvent(ctx, e)
		if ctx.Err() != nil {
			return // shutdown interrupted it: processed again after the restart
		}
		switch {
		case errors.Is(err, errParked):
			status = EventParked
			if h.spoolGen.Load() != gen {
				status = EventReceived // a spool entry landed meanwhile: look again now
			}
		case err != nil:
			status, errText = EventFailed, err.Error()
			log.Printf("event %s: %v", e.EventID, err)
		}
		if err := h.st.FinishEvent(e.Seq, status, errText, h.now()); err != nil {
			log.Printf("finish event %s: %v", e.EventID, err)
			return
		}
	}
}

func (h *Hub) processEvent(ctx context.Context, e *Event) error {
	var u models.Update
	if e.Kind == kindMiniApp {
		return h.handleMiniAppEvent(ctx, e)
	}
	if err := json.Unmarshal(e.Raw, &u); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if u.StoppedMessageGeneration != nil {
		return h.handleStop(e, int64(u.StoppedMessageGeneration.DraftID))
	}
	if u.MessageReaction != nil {
		return h.handleReaction(ctx, e, u.MessageReaction)
	}
	if u.CallbackQuery != nil {
		return h.handleCallback(ctx, e, u.CallbackQuery)
	}
	if u.Message == nil {
		return nil
	}
	if e.Kind == "pin_service" {
		h.deleteMsg(ctx, e.ChatID, e.MsgID)
		return nil
	}
	return h.handleMessage(ctx, e, u.Message)
}

// handleStop is Telegram's stop button on a draft: the draft is dropped at
// once and a cancel job (durable, own lane, idempotent per run) goes to the
// run's host. A stop for an unknown or finished run is ignored.
func (h *Hub) handleStop(e *Event, draftID int64) error {
	j, err := h.st.JobByDraft(draftID)
	if err != nil {
		return err
	}
	if j == nil || j.StopRequested || (j.Status != JobQueued && j.Status != JobPushed && j.Status != JobStarted) {
		log.Printf("stop for draft %d ignored (%s)", draftID, jobState(j))
		return nil
	}
	payload, _ := json.Marshal(proto.Cancel{EventID: j.EventID})
	c := &JobRow{ID: newID(), EventID: j.EventID + ":cancel", Seq: j.Seq, Sub: j.Sub, Host: j.Host,
		Lane: "cancel:" + j.ID, Target: j.Target, Type: proto.JobCancel, Payload: payload,
		ChatID: j.ChatID, MsgID: j.MsgID, SessionGen: j.SessionGen}
	if _, err := h.st.AddJob(c, h.now()); err != nil {
		return err
	}
	if _, err := h.st.RequestStop(j.ID); err != nil {
		return err
	}
	log.Printf("stop draft %d: cancel %s for job %s on %s (%s)", draftID, c.ID, j.ID, j.Host, e.EventID)
	h.kickDispatch()
	// No more drafts; a draft call in flight is waited for by the delivery,
	// not here, so the cancel never waits on the Bot API.
	h.endStream(j.ID)
	return nil
}

func jobState(j *JobRow) string {
	if j == nil {
		return "unknown draft"
	}
	if j.StopRequested {
		return "job " + j.ID + " already stopped"
	}
	return "job " + j.ID + " " + j.Status
}

// chain is what a save_file job turns into once the file is on the host.
type chain struct {
	Prompt string    `json:"prompt,omitempty"` // contains {path}
	Meta   *lineMeta `json:"meta,omitempty"`
	// new sessions (newsession.go): Origin = the chat target a new_session job
	// was asked from; Spawn = the new_session job a queued line waits for.
	Origin string `json:"origin,omitempty"`
	Spawn  string `json:"spawn,omitempty"`
}

func (h *Hub) handleMessage(ctx context.Context, e *Event, msg *models.Message) error {
	chatID := msg.Chat.ID
	if err := h.st.AddMessage(Message{ChatID: chatID, MsgID: msg.ID, Direction: "in", Kind: messageKind(msg)}, h.now()); err != nil {
		return err
	}
	h.ensurePin(ctx, chatID)
	if done, err := h.targetUI(ctx, msg); done || err != nil {
		return err
	}
	if msg.Text != "" && strings.HasPrefix(msg.Text, "/") {
		if done, err := h.command(ctx, e, msg); done || err != nil {
			return err
		}
	}
	rt, err := h.route(chatID, &models.Update{Message: msg})
	if err != nil {
		return err
	}
	if rt.UnknownAgent && h.parkable(e) && h.now().Sub(time.Unix(int64(msg.ReplyToMessage.Date), 0)) < parkReplyAge {
		return errParked
	}
	if rt.Unrouted {
		log.Printf("reply %d to message %d: sender's session unknown, not delivered", msg.ID, msg.ReplyToMessage.ID)
		h.say(ctx, chatID, unroutedText, msg.ID)
		return nil
	}
	// done hands the message on; an "Answer…" context is used up only then (a
	// voice note that fails, or a restart mid-transcription, keeps it).
	done := func(err error) error { return err }
	if rt.Rule == RuleAnswer {
		// the "Answer…" context: this message answers that agent message, once
		msg.ReplyToMessage = &models.Message{ID: rt.AnswerTo, Chat: msg.Chat}
		done = func(err error) error {
			if err == nil {
				h.clearAnswerContext(chatID)
			}
			return err
		}
	}
	if msg.ReplyToMessage != nil {
		h.markAnswered(chatID, msg.ReplyToMessage.ID)
	}
	t := rt.Target
	if t.Kind == KindNew && t.Mode == ModeP {
		if t, err = h.startNewP(ctx, chatID, t); err != nil {
			return err
		}
	}
	if t.Kind == KindNew {
		if e.spawn, err = h.st.PendingSpawn(chatID, t.String()); err != nil {
			return err
		}
	}
	switch {
	case msg.Voice != nil && !h.cfg.Transcribe.Enabled():
		h.say(ctx, chatID, voiceOffText, msg.ID)
		return nil

	case msg.Voice != nil:
		audio, err := h.tg.Download(ctx, msg.Voice.FileID)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			h.say(ctx, chatID, "⚠️ voice download failed: "+err.Error(), msg.ID)
			return nil
		}
		text, source, err := h.sp.Transcribe(ctx, audio)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			h.say(ctx, chatID, "⚠️ transcription failed — please retype: "+err.Error(), msg.ID)
			return nil
		}
		h.say(ctx, chatID, fmt.Sprintf("🎤 (%s) %s", source, text), msg.ID)
		return done(h.enqueueTo(ctx, e, msg, t, "[voice message, transcribed]\n"+text))

	case (len(msg.Photo) > 0 || msg.Document != nil) && t.Kind == KindNew:
		return done(h.enqueueNewFile(ctx, e, msg, t))

	case len(msg.Photo) > 0:
		best := msg.Photo[len(msg.Photo)-1] // largest size last
		prompt := msg.Caption
		if prompt == "" {
			prompt = "I sent you an image."
		}
		return done(h.enqueueFile(ctx, e, msg, t, best.FileID, "", ".jpg",
			"[photo]\n"+prompt+"\n\n[Image attached: {path}]"))

	case msg.Document != nil:
		ext := filepath.Ext(msg.Document.FileName)
		if ext == "" {
			ext = ".bin"
		}
		prompt := msg.Caption
		if prompt == "" {
			prompt = "I sent you a file."
		}
		return done(h.enqueueFile(ctx, e, msg, t, msg.Document.FileID, msg.Document.FileName, ext,
			"[file]\n"+prompt+"\n\n[File attached ("+msg.Document.FileName+"): {path}]"))

	case msg.Location != nil:
		prompt := fmt.Sprintf("User shared location: %.6f, %.6f", msg.Location.Latitude, msg.Location.Longitude)
		if msg.Venue != nil {
			prompt += fmt.Sprintf(" (%s, %s)", msg.Venue.Title, msg.Venue.Address)
		}
		return done(h.enqueueTo(ctx, e, msg, t, "[location share] "+prompt))

	case msg.Text != "":
		return done(h.enqueueTo(ctx, e, msg, t, msg.Text))
	}
	return nil
}

// unroutedText answers a reply to a message whose sending session is unknown.
const unroutedText = "⚠️ Not delivered: I don't know which session sent the message you replied to " +
	"(it came without hommy's sender record), so I didn't pass your reply to anyone. " +
	"Answer in that session's terminal, or send it again without replying to reach the current target."

func messageKind(m *models.Message) string {
	switch {
	case m.Voice != nil:
		return "voice"
	case len(m.Photo) > 0:
		return "photo"
	case m.Document != nil:
		return "document"
	case m.Location != nil:
		return "location"
	case m.Text != "":
		return "text"
	}
	return "other"
}

func (h *Hub) currentTarget(chatID int64) (Target, error) {
	s, err := h.st.ChatTarget(chatID)
	if err != nil {
		return Target{}, err
	}
	if s == "" {
		s = h.cfg.DefaultTarget
	}
	return ParseTarget(s)
}

// enqueueTo sends a message's prompt to its target.
func (h *Hub) enqueueTo(ctx context.Context, e *Event, msg *models.Message, t Target, prompt string) error {
	switch t.Kind {
	case KindVault, KindP, KindSession: // a session gets a `type` job (addRunP → addType)
		return h.enqueueRunP(ctx, e, msg, t, prompt)
	case KindNew:
		return h.enqueueNew(ctx, e, msg, t, prompt)
	}
	return fmt.Errorf("target %s: unknown kind", t)
}

func (h *Hub) systemPrompt() string {
	return claude.SystemPrompt(h.cfg.TTS.URL != "", h.cfg.TTS.MinChars, h.cfg.TTS.ExtraLangs())
}

// enqueueRunP stores a run_p job for t in its current session generation.
func (h *Hub) enqueueRunP(ctx context.Context, e *Event, msg *models.Message, t Target, prompt string) error {
	gen, err := h.st.CurrentGen(t.String())
	if err != nil {
		return err
	}
	return h.addRunP(ctx, e, msg, t, prompt, gen, 0, "")
}

// addRunP stores a run_p job of session generation gen; sub/suffix
// distinguish a job chained from an earlier one of the same message.
func (h *Hub) addRunP(ctx context.Context, e *Event, msg *models.Message, t Target, prompt string, gen, sub int, suffix string) error {
	if t.Kind == KindSession {
		return h.addType(ctx, e, msg, t, prompt, sub, suffix)
	}
	payload, _ := json.Marshal(proto.RunP{Prompt: prompt, Dir: t.Dir, AppendSystemPrompt: h.systemPrompt(),
		NewDir: h.newDirRun(t)})
	j := &JobRow{ID: newID(), EventID: e.EventID + suffix, Seq: e.Seq, Sub: sub, Host: t.Host, Lane: t.String(),
		Target: t.String(), Type: proto.JobRunP, Payload: payload, ChatID: msg.Chat.ID, MsgID: msg.ID, SessionGen: gen,
		DraftID: newDraftID()}
	return h.addJob(ctx, j)
}

func (h *Hub) enqueueFile(ctx context.Context, e *Event, msg *models.Message, t Target, fileID, name, ext, prompt string) error {
	data, err := h.tg.Download(ctx, fileID)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h.say(ctx, msg.Chat.ID, "⚠️ file download failed: "+err.Error(), msg.ID)
		return nil
	}
	// The chained run_p is created later; it takes this generation, not the one current then.
	gen, err := h.st.CurrentGen(t.String())
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(proto.SaveFile{Name: name, Ext: ext, Data: data})
	m := h.metaOf(msg)
	ch, _ := json.Marshal(chain{Prompt: prompt, Meta: &m})
	j := &JobRow{ID: newID(), EventID: e.EventID + ":save", Seq: e.Seq, Host: t.Host, Lane: laneFor(t),
		Target: t.String(), Type: proto.JobSaveFile, Payload: payload, ChatID: msg.Chat.ID, MsgID: msg.ID,
		SessionGen: gen, Chain: string(ch)}
	return h.addJob(ctx, j)
}

// addJob stores the job, acks 👀 when its host is offline, and wakes the dispatcher.
func (h *Hub) addJob(ctx context.Context, j *JobRow) error {
	added, err := h.st.AddJob(j, h.now())
	if err != nil {
		return err
	}
	if !added {
		return nil // this event already has its job (reprocessed after a restart)
	}
	if j.Sub == 0 && !h.connected(j.Host) {
		h.react(ctx, j.ChatID, j.MsgID, "👀")
	}
	h.kickDispatch()
	return nil
}

func (h *Hub) react(ctx context.Context, chatID int64, msgID int, emoji string) {
	if err := h.tg.React(ctx, chatID, msgID, emoji); err != nil {
		log.Printf("react %s on %d: %v", emoji, msgID, err)
	}
}

// voiceOffText answers a voice note when transcription is off (SPEECH=off).
const voiceOffText = "🔇 voice is off — please send text"

// say sends a bot message and records it.
func (h *Hub) say(ctx context.Context, chatID int64, text string, replyTo int) int {
	id, err := h.tg.Send(ctx, chatID, text, replyTo)
	if err != nil {
		log.Printf("send: %v", err)
		return 0
	}
	h.record(Message{ChatID: chatID, MsgID: id, Direction: "out", Kind: "notice"})
	return id
}

const helpText = "hommy: Telegram → Claude Code.\n" +
	"Send text, voice, images, or a location. They go to the current target (🎯); a reply goes to whoever wrote the message.\n" +
	"/new_session — start fresh\n/voice — show or set TTS voice\n/status\n" +
	"/target — show or set where messages go (" + targetUsage + ")" + sessionHelp

// command handles a slash command; false means "not a command, treat as text".
func (h *Hub) command(ctx context.Context, e *Event, msg *models.Message) (bool, error) {
	chatID := msg.Chat.ID
	fields := strings.Fields(msg.Text)
	name := strings.TrimPrefix(fields[0], "/")
	name, _, _ = strings.Cut(name, "@")
	args := strings.TrimSpace(strings.TrimPrefix(msg.Text, fields[0]))
	switch name {
	case "start":
		h.say(ctx, chatID, helpText, 0)
	case "new_session":
		t, err := h.currentTarget(chatID)
		if err != nil {
			return true, err
		}
		if t.Kind == KindSession {
			// A live session can't be reset from here: go back to the default
			// target, where the next message starts a new one.
			d, err := ParseTarget(h.cfg.DefaultTarget)
			if err != nil {
				return true, err
			}
			if d.Kind != KindNew {
				if _, err := h.st.NewGen(d.String(), h.now()); err != nil {
					return true, err
				}
			}
			return true, h.switchTarget(ctx, chatID, d, " — next message starts a new session")
		}
		if _, err := h.st.NewGen(t.String(), h.now()); err != nil {
			return true, err
		}
		h.say(ctx, chatID, "session reset — next message starts fresh", 0)
	case "status":
		text, ents := h.statusRich(chatID)
		h.sayRich(ctx, chatID, text, ents, 0)
	case "voice":
		h.voiceCommand(ctx, chatID, fields)
	case "target":
		return true, h.targetCommand(ctx, chatID, args)
	default:
		return h.sessionCommand(ctx, e, msg, name, args)
	}
	return true, nil
}

func (h *Hub) voiceCommand(ctx context.Context, chatID int64, fields []string) {
	if h.cfg.TTS.URL == "" {
		h.say(ctx, chatID, "voice replies are disabled (TTS_URL=off)", 0)
		return
	}
	voices, err := h.sp.ListVoices(ctx)
	if err != nil {
		h.say(ctx, chatID, "⚠️ can't reach TTS server: "+err.Error(), 0)
		return
	}
	if len(fields) < 2 {
		h.say(ctx, chatID, "current voice: "+h.voiceName()+"\n\nusage: /voice <name>\n\navailable:\n"+strings.Join(voices, "\n"), 0)
		return
	}
	name := fields[1]
	if !slices.Contains(voices, name) {
		h.say(ctx, chatID, "unknown voice: "+name+" — send /voice for the list", 0)
		return
	}
	if err := h.st.Set("tts_voice", name); err != nil {
		h.say(ctx, chatID, "⚠️ "+err.Error(), 0)
		return
	}
	h.say(ctx, chatID, "voice set to "+name, 0)
}

func (h *Hub) voiceName() string {
	if v, _ := h.st.Get("tts_voice"); v != "" {
		return v
	}
	return h.cfg.TTS.DefaultVoice
}

func (h *Hub) hostNames() []string {
	var out []string
	for n := range h.cfg.HostKeys {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}
