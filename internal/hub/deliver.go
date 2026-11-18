package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
	"github.com/AlexDobrushskiy/hommy/internal/speech"
	"github.com/AlexDobrushskiy/hommy/internal/textx"
)

// deliver runs the job type's result handler until it succeeds, then frees
// the lane (which stays held meanwhile, so answers keep their order).
func (h *Hub) deliver(ctx context.Context, j *JobRow) {
	var r proto.Result
	if err := json.Unmarshal([]byte(j.Result), &r); err != nil {
		log.Printf("job %s: bad stored result: %v", j.ID, err)
	}
	h.mu.Lock()
	fn := h.results[j.Type]
	h.mu.Unlock()
	if fn == nil {
		fn = h.deliverGeneric
	}
	for attempt := 0; ; attempt++ {
		err := fn(ctx, j, r)
		if ctx.Err() != nil {
			return // shutting down: delivered again after the restart
		}
		if err == nil {
			break
		}
		log.Printf("deliver job %s (attempt %d): %v; retrying", j.ID, attempt+1, err)
		if !h.sleepRetry(ctx, attempt) {
			return
		}
	}
	if err := h.st.MarkDone(j.ID, h.now()); err != nil {
		log.Printf("mark done %s: %v", j.ID, err)
	} else if j.Type == proto.JobSaveFile || j.Type == proto.JobNewSession { // file bytes
		if err := h.st.DropPayload(j.ID); err != nil {
			log.Printf("drop payload %s: %v", j.ID, err)
		}
	}
	h.removeTyping(j.ChatID)
	h.kickDispatch()
}

// part sends message n of a job's answer at most once: parts already sent
// (j.Delivered) are skipped, and a part Telegram refuses for good counts as
// sent so it can't hold the lane. A transient failure is returned, and the
// delivery retries from this part. Parts are numbered from 0, in send order.
func (h *Hub) part(j *JobRow, n int, send func() error) error {
	if n < j.Delivered {
		return nil
	}
	if err := send(); err != nil {
		if !permanent(err) {
			return err
		}
		log.Printf("job %s: message %d refused by Telegram, skipped: %v", j.ID, n, err)
	}
	if err := h.st.SetDelivered(j.ID, n+1); err != nil {
		return err
	}
	j.Delivered = n + 1
	return nil
}

// permanent reports a Bot API refusal that a retry won't change.
func permanent(err error) bool {
	return errors.Is(err, bot.ErrorBadRequest) || errors.Is(err, bot.ErrorForbidden) ||
		errors.Is(err, bot.ErrorUnauthorized) || errors.Is(err, bot.ErrorNotFound) || bot.IsMigrateError(err)
}

func (h *Hub) deliverGeneric(ctx context.Context, j *JobRow, r proto.Result) error {
	if r.Status == proto.StatusOK {
		return nil
	}
	return h.part(j, 0, func() error {
		return h.sendFor(ctx, j, "", fmt.Sprintf("⚠️ %s on %s: %s %s", j.Type, j.Host, r.Status, r.Error), j.MsgID)
	})
}

type lastRun struct {
	At    string `json:"at"`
	Error string `json:"error,omitempty"`
}

// deliverRunP posts the answer as the bot always has: text chunks, then a
// voice note when the reply carried a <tts> block.
func (h *Hub) deliverRunP(ctx context.Context, j *JobRow, r proto.Result) error {
	h.closeStream(j.ID) // no draft may land after the answer
	// The row may predate a stop accepted just before the result was stored;
	// once the result is stored no stop is accepted, so this read is final.
	cur, err := h.st.Job(j.ID)
	if err != nil {
		return err
	}
	if cur != nil {
		j.StopRequested = cur.StopRequested
	}
	var d proto.RunPResult
	json.Unmarshal(r.Data, &d)
	if d.Stopped || j.StopRequested {
		// Stopped from the chat: one line with what was written so far, no voice.
		// (A run that finished before the cancel reached it counts as stopped too.)
		partial, _, _ := speech.ExtractTTS(d.Text)
		if i := strings.Index(partial, "<tts"); i >= 0 {
			partial = partial[:i]
		}
		text := "⏹ stopped"
		if p := strings.TrimSpace(partial); p != "" {
			text += "\n\n" + p
		}
		for i, c := range textx.Chunk(text, 4000) {
			replyTo := 0
			if i == 0 {
				replyTo = j.MsgID
			}
			if err := h.part(j, i, func() error { return h.sendFor(ctx, j, d.SessionID, c, replyTo) }); err != nil {
				return err
			}
		}
		return nil
	}
	lr := lastRun{At: h.now().UTC().Format(time.RFC3339)}
	defer func() {
		if err := h.st.Set("last:"+j.Target, mustJSON(lr)); err != nil {
			log.Printf("last run %s: %v", j.Target, err)
		}
	}()
	hint, err := h.settleNewP(ctx, j, r, d)
	if err != nil {
		return err
	}
	if r.Status != proto.StatusOK {
		lr.Error = r.Error
		return h.part(j, 0, func() error { return h.sendFor(ctx, j, d.SessionID, "⚠️ "+r.Error+hint, j.MsgID) })
	}
	text, spoken, lang := speech.ExtractTTS(d.Text)
	if text == "" {
		text = "(empty reply)"
	}
	chunks := textx.Chunk(text, 4000)
	// The text goes out as soon as it exists; the voice note follows as its
	// own message once synthesis finishes.
	for i, c := range chunks {
		replyTo := 0
		if i == 0 {
			replyTo = j.MsgID
		}
		if err := h.part(j, i, func() error { return h.sendFor(ctx, j, d.SessionID, c, replyTo) }); err != nil {
			return err
		}
	}
	if spoken == "" || h.cfg.TTS.URL == "" {
		return nil
	}
	return h.voicePart(ctx, j, len(chunks), d.SessionID, spoken, lang)
}

// sendFor sends a message that belongs to a job's target/session.
func (h *Hub) sendFor(ctx context.Context, j *JobRow, sessionID, text string, replyTo int) error {
	id, err := h.tg.Send(ctx, j.ChatID, text, replyTo)
	if err != nil {
		log.Printf("send: %v", err)
		return err
	}
	h.record(Message{ChatID: j.ChatID, MsgID: id, Direction: "out", Kind: "answer",
		Target: j.Target, SessionID: sessionID, JobID: j.ID})
	return nil
}

// backendFor resolves the synthesis backend for a <tts> block's language.
// /voice only rebinds the default (English) backend.
func (h *Hub) backendFor(lang string) speech.TTSBackend {
	b := h.cfg.TTS.BackendFor(lang)
	if b.URL == h.cfg.TTS.URL {
		b.Voice = h.voiceName()
	}
	return b
}

// deliverSaveFile hands the saved path to the run that was waiting for it.
func (h *Hub) deliverSaveFile(ctx context.Context, j *JobRow, r proto.Result) error {
	if r.Status != proto.StatusOK {
		return h.part(j, 0, func() error {
			return h.sendFor(ctx, j, "", fmt.Sprintf("⚠️ saving the file on %s failed: %s", j.Host, r.Error), j.MsgID)
		})
	}
	var d proto.SaveFileResult
	if err := json.Unmarshal(r.Data, &d); err != nil || d.Path == "" {
		return h.part(j, 0, func() error {
			return h.sendFor(ctx, j, "", "⚠️ saving the file on "+j.Host+" returned no path", j.MsgID)
		})
	}
	if j.Chain == "" {
		return nil
	}
	var ch chain
	if err := json.Unmarshal([]byte(j.Chain), &ch); err != nil {
		return err
	}
	t, err := ParseTarget(j.Target)
	if err != nil {
		return err
	}
	e := &Event{Seq: j.Seq, EventID: strings.TrimSuffix(j.EventID, ":save")}
	msg := &models.Message{ID: j.MsgID, Chat: models.Chat{ID: j.ChatID}}
	prompt := strings.ReplaceAll(ch.Prompt, "{path}", d.Path)
	if ch.Spawn != "" { // sent while its new session was being created
		st, ok, why, err := h.spawnedSession(ch.Spawn)
		if err != nil {
			return err
		}
		if !ok {
			return h.part(j, 0, func() error {
				return h.sendFor(ctx, j, "", "⚠️ the new session wasn't created ("+why+"), so this wasn't sent; the file is at "+d.Path, j.MsgID)
			})
		}
		t = st
	}
	if t.Kind == KindSession && ch.Meta != nil { // the original message's kind and reply
		return h.addTypeMeta(ctx, e, msg, t, prompt, 1, ":run", *ch.Meta)
	}
	// The run keeps the session generation the message arrived in.
	return h.addRunP(ctx, e, msg, t, prompt, j.SessionGen, 1, ":run")
}

// record stores a message ↔ target/session link (used for reply routing from phase 07).
func (h *Hub) record(m Message) {
	if err := h.st.AddMessage(m, h.now()); err != nil {
		log.Printf("record message %d: %v", m.MsgID, err)
	}
}
