package faketg

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Waits mirror telegram-agent-cli's `e2e wait`, event-driven: every state change wakes them.

type waitSpec struct {
	kind       string
	since, msg int
	all        bool
	ignoreMine bool
	timeout    time.Duration
}

type waitResult struct {
	For      string       `json:"for"`
	Elapsed  float64      `json:"elapsed_s"`
	Message  *botMessage  `json:"message,omitempty"`
	Messages []botMessage `json:"messages,omitempty"`
	Before   any          `json:"before,omitempty"`
	Keyboard any          `json:"keyboard,omitempty"`
	Drafts   []draftView  `json:"drafts,omitempty"`
}

type watch struct {
	s          *Server
	c          *chat
	spec       waitSpec
	start      time.Time
	startLast  int
	startDraft int
	ids        []int // watched messages (edit, reaction); nil = the recent ones plus new bot messages
	base       map[int]string
	baseView   map[int]botMessage
	baseKey    string
	baseAny    any
}

// begin snapshots the start state; callers hold mu.
func (s *Server) begin(c *chat, spec waitSpec) *watch {
	w := &watch{s: s, c: c, spec: spec, start: s.now(), startLast: c.nextID, startDraft: s.draftSeq,
		base: map[int]string{}, baseView: map[int]botMessage{}}
	if w.spec.since == 0 {
		w.spec.since = c.nextID
	}
	s.watching++
	s.bump()
	switch spec.kind {
	case "edit", "reaction":
		if spec.msg != 0 {
			w.ids = []int{spec.msg}
		} else {
			for i := len(c.msgs) - 1; i >= 0 && len(w.ids) < 20; i-- {
				if m := c.msgs[i]; !m.Deleted && (spec.kind == "reaction" || m.FromBot) {
					w.ids = append(w.ids, m.ID)
				}
			}
		}
		for _, id := range w.ids {
			w.observe(id)
		}
	case "pin", "keyboard":
		w.baseKey, w.baseAny = w.stateKey()
	}
	return w
}

// only narrows the watch to one message (a send's reaction wait).
func (w *watch) only(id int) {
	w.ids = []int{id}
	w.observe(id)
}

func (w *watch) key(m *message) string {
	var v any
	if w.spec.kind == "edit" {
		fid := ""
		if m.Voice != nil {
			fid = m.Voice.ID
		}
		v = []any{m.Text, m.Entities, m.Inline, fid}
	} else {
		r := m.BotReaction
		if !w.spec.ignoreMine {
			v = []any{m.UserReaction, r}
		} else {
			v = r
		}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func (w *watch) observe(id int) {
	if _, seen := w.base[id]; seen {
		return
	}
	if m := w.c.find(id); m != nil {
		w.base[id] = w.key(m)
		w.baseView[id] = w.s.view(w.c, m)
	}
}

func (w *watch) stateKey() (string, any) {
	if w.spec.kind == "pin" {
		if m := w.c.find(w.c.pinned); m != nil {
			v := w.s.view(w.c, m)
			return fmt.Sprintf("%d|%s", m.ID, m.Text), v
		}
		return "", nil
	}
	kb := w.s.keyboardView(w.c)
	b, _ := json.Marshal(kb)
	return string(b), kb
}

func (w *watch) newBotMessages(after int) []*message {
	var out []*message
	for _, m := range w.c.msgs {
		if m.ID > after && m.FromBot && !m.Deleted && m.Service == "" {
			out = append(out, m)
		}
	}
	return out
}

// check reports whether the event happened; callers hold mu.
func (w *watch) check() (*waitResult, bool) {
	r := &waitResult{}
	c, s := w.c, w.s
	switch w.spec.kind {
	case "reply":
		ms := w.newBotMessages(w.spec.since)
		if len(ms) == 0 {
			return nil, false
		}
		for _, m := range ms {
			r.Messages = append(r.Messages, s.view(c, m))
		}
		r.Message = &r.Messages[0]
		return r, true
	case "edit", "reaction":
		ids := w.ids
		if w.spec.msg == 0 && w.spec.kind == "edit" {
			for _, m := range w.newBotMessages(w.startLast) {
				w.observe(m.ID)
				ids = append(ids, m.ID)
			}
		}
		for _, id := range ids {
			w.observe(id)
			m := c.find(id)
			if m == nil {
				continue
			}
			if base, ok := w.base[id]; ok && w.key(m) != base {
				v := s.view(c, m)
				r.Message, r.Before = &v, w.baseView[id]
				return r, true
			}
		}
		return nil, false
	case "pin", "keyboard":
		k, v := w.stateKey()
		if k == w.baseKey {
			return nil, false
		}
		if w.spec.kind == "pin" {
			if bm, ok := v.(botMessage); ok {
				r.Message = &bm
			}
		} else {
			r.Keyboard = v
		}
		r.Before = w.baseAny
		return r, true
	case "draft":
		ds := draftViews(c.drafts, w.startDraft)
		if len(ds) == 0 {
			return nil, false
		}
		if !w.spec.all {
			r.Drafts = ds
			return r, true
		}
		final := w.newBotMessages(w.startLast)
		if len(final) == 0 {
			return nil, false
		}
		v := s.view(c, final[0])
		r.Drafts, r.Message = ds, &v
		return r, true
	}
	return nil, false
}

func (w *watch) wait(ctx context.Context) (*waitResult, *cliError) {
	defer func() {
		w.s.mu.Lock()
		w.s.watching--
		w.s.mu.Unlock()
	}()
	timer := w.s.after(w.spec.timeout)
	for {
		w.s.mu.Lock()
		r, done := w.check()
		if done && w.spec.kind == "reply" && r.Message != nil && r.Message.ID > w.c.seen {
			w.c.seen = r.Message.ID
		}
		ch := w.s.changed
		now := w.s.now()
		w.s.mu.Unlock()
		if done {
			r.For = w.spec.kind
			r.Elapsed = float64(now.Sub(w.start).Milliseconds()) / 1000
			return r, nil
		}
		select {
		case <-ch:
		case <-timer:
			return nil, &cliError{Code: "timeout", Message: fmt.Sprintf("no %s within %gs", w.spec.kind, w.spec.timeout.Seconds())}
		case <-ctx.Done():
			return nil, &cliError{Code: "timeout", Message: "the request ended before the " + w.spec.kind}
		}
	}
}
