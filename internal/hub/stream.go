package hub

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// draftStream shows one run_p's progress as a live Telegram draft: a status
// line above the growing text, one sendMessageDraft (full text) at most every
// DraftEvery. Drafts are a view only: the answer always goes out as messages.
type draftStream struct {
	jobID   string
	chatID  int64
	draftID int64

	mu    sync.Mutex
	seq   int
	view  string
	dirty bool // view changed since the last draft
	shown bool // a draft went out

	wake chan struct{}
	quit chan struct{}
	done chan struct{}
}

// newDraftID is a random non-zero id that JSON clients read exactly (< 2^53).
func newDraftID() int64 {
	var b [8]byte
	rand.Read(b[:])
	return int64(binary.BigEndian.Uint64(b[:])>>11) | 1
}

// draftLimit leaves room under sendMessageDraft's 4096 characters.
const draftLimit = 3500

// renderDraft is the status line, then the answer so far (without the
// trailing <tts> block, which is never shown as text), cut to its tail.
func renderDraft(p proto.Progress) string {
	text := p.Text
	if i := strings.Index(text, "<tts"); i >= 0 {
		text = text[:i]
	}
	text = strings.TrimSpace(text)
	status := strings.TrimSpace(p.Status)
	if status == "" {
		status = "…"
	}
	if text == "" {
		return status
	}
	r := []rune(text)
	if room := draftLimit - len([]rune(status)) - 3; len(r) > room {
		text = "…" + string(r[len(r)-room:])
	}
	return status + "\n\n" + text
}

// streamProgress feeds a run's progress snapshot to its draft stream,
// starting the stream on the first one.
func (h *Hub) streamProgress(j *JobRow, p proto.Progress) {
	h.streamsMu.Lock()
	if _, closed := h.streamClosed[j.ID]; closed {
		h.streamsMu.Unlock()
		return
	}
	s := h.streams[j.ID]
	if s == nil {
		s = &draftStream{jobID: j.ID, chatID: j.ChatID, draftID: j.DraftID,
			wake: make(chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{})}
		h.streams[j.ID] = s
		h.bg.Add(1)
		go func() { defer h.bg.Done(); defer close(s.done); h.runStream(h.rootCtx(), s) }()
	}
	h.streamsMu.Unlock()

	s.mu.Lock()
	if p.Seq <= s.seq {
		s.mu.Unlock()
		return // a replayed snapshot
	}
	s.seq = p.Seq
	if v := renderDraft(p); v != s.view {
		s.view, s.dirty = v, true
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// closedStream is an ended stream; done closes once its last draft call returned.
type closedStream struct {
	at   time.Time
	done <-chan struct{} // nil: there was no stream
}

// closeStream ends a job's draft stream for good and waits until no draft
// call is in flight, so no draft can land after what is sent next. Every
// caller waits, not just the first.
func (h *Hub) closeStream(jobID string) {
	if done := h.endStream(jobID); done != nil {
		<-done
	}
}

// endStream ends a job's draft stream for good without waiting; the result
// closes once no draft call is in flight.
func (h *Hub) endStream(jobID string) <-chan struct{} {
	h.streamsMu.Lock()
	defer h.streamsMu.Unlock()
	if c, ok := h.streamClosed[jobID]; ok {
		return c.done
	}
	now := time.Now()
	for id, c := range h.streamClosed {
		if now.Sub(c.at) > time.Hour {
			delete(h.streamClosed, id)
		}
	}
	c := closedStream{at: now}
	if s := h.streams[jobID]; s != nil {
		delete(h.streams, jobID)
		close(s.quit)
		c.done = s.done
	}
	h.streamClosed[jobID] = c
	return c.done
}

// drafting reports whether a live draft is showing in the chat ("typing…"
// would replace it on the client).
func (h *Hub) drafting(chatID int64) bool {
	h.streamsMu.Lock()
	defer h.streamsMu.Unlock()
	for _, s := range h.streams {
		s.mu.Lock()
		shown := s.shown && s.chatID == chatID
		s.mu.Unlock()
		if shown {
			return true
		}
	}
	return false
}

// runStream sends the newest view at most every DraftEvery, re-sends an
// unchanged one every DraftKeepalive (so the client keeps showing it), and
// backs off on 429 keeping only the newest text. A draft Telegram refuses for
// good ends the stream; the final answer doesn't depend on it.
func (h *Hub) runStream(ctx context.Context, s *draftStream) {
	every, keep := h.cfg.DraftEvery, h.cfg.DraftKeepalive
	if every <= 0 {
		every = time.Second
	}
	if keep <= 0 {
		keep = 4 * time.Second
	}
	var last, next time.Time
	for {
		s.mu.Lock()
		view, dirty, shown := s.view, s.dirty, s.shown
		s.mu.Unlock()
		now := time.Now()
		var due time.Time
		switch {
		case dirty:
			due = next
		case shown:
			due = maxTime(next, last.Add(keep))
		}
		if view != "" && (dirty || shown) && !now.Before(due) {
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := h.tg.Draft(cctx, s.chatID, s.draftID, view)
			cancel()
			now = time.Now()
			var tooMany *bot.TooManyRequestsError
			switch {
			case err == nil:
				s.mu.Lock()
				if s.view == view {
					s.dirty = false
				}
				s.shown = true
				s.mu.Unlock()
				last, next = now, now.Add(every)
			case errors.As(err, &tooMany):
				next = now.Add(max(time.Duration(tooMany.RetryAfter)*time.Second, every))
				log.Printf("draft %d (job %s): 429, retrying in %s", s.draftID, s.jobID, next.Sub(now))
			case permanent(err):
				log.Printf("draft %d (job %s) refused, streaming off for this run: %v", s.draftID, s.jobID, err)
				return
			default:
				if ctx.Err() != nil {
					return
				}
				log.Printf("draft %d (job %s): %v", s.draftID, s.jobID, err)
				next = now.Add(2 * every)
			}
			continue
		}
		var t *time.Timer
		var tc <-chan time.Time
		if (dirty || shown) && view != "" {
			t = time.NewTimer(due.Sub(now))
			tc = t.C
		}
		select {
		case <-s.quit:
		case <-ctx.Done():
		case <-s.wake:
		case <-tc:
		}
		if t != nil {
			t.Stop()
		}
		select {
		case <-s.quit:
			return
		case <-ctx.Done():
			return
		default:
		}
	}
}
