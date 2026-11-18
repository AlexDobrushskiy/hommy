package agterm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	RetryEvery  = 60 * time.Second
	RetryWindow = 30 * time.Minute
)

// Pending is one line waiting to be typed into a session.
type Pending struct {
	EventID  string `json:"event_id"`
	JobID    string `json:"job_id,omitempty"` // the hub job that queued it (for late outcomes)
	MsgID    int64  `json:"msg_id"`
	Session  string `json:"session"`
	BriefDir string `json:"brief_dir,omitempty"`
	// Inbox + ItemID: the line's inbox item; once the agent acks it (handled from
	// the inbox) the line is done and never typed.
	Inbox     string    `json:"inbox,omitempty"`
	ItemID    string    `json:"item_id,omitempty"`
	Line      string    `json:"line"`
	Created   time.Time `json:"created"`
	NextAt    time.Time `json:"next_at"`
	Attempts  int       `json:"attempts"`
	LastState State     `json:"last_state,omitempty"`
	// Submitting: Return is about to be (or was) sent. A host that stops here
	// finds it set on restart and reports the line unconfirmed, never retypes it.
	Submitting bool `json:"submitting,omitempty"`
	// Draft is composer text an earlier try found to be someone's draft (the
	// caret at its start reads like a suggestion); while it is unchanged the
	// line waits without typing.
	Draft string `json:"draft,omitempty"`
}

// Outcome of one typing attempt.
type Outcome string

const (
	Delivered   Outcome = "delivered"   // typed and submitted
	Unconfirmed Outcome = "unconfirmed" // Return sent, submission not confirmed: never retyped
	Blocked     Outcome = "blocked"     // not ready; nothing (left) typed: retry
	Gone        Outcome = "gone"        // no live session
	Failed      Outcome = "failed"      // a body write could not be cleaned up, or transport error
	Expired     Outcome = "expired"     // retry window over
	Handled     Outcome = "handled"     // the agent acked it in the inbox: not typed, nothing to report
)

// Attempt is what happened to one pending line.
type Attempt struct {
	Pending
	Outcome Outcome  `json:"outcome"`
	State   Composer `json:"state"`
	Detail  string   `json:"detail,omitempty"`
}

// lane is the unit lines keep their order in: the agent (its brief folder),
// else the session. Lines for an agent may name different sessions (a reply to
// an ended session's ping, one held in its successor); they are still typed
// in the order they came, into whichever session is live for the folder.
func lane(p Pending) string {
	if p.BriefDir != "" {
		return "dir:" + filepath.Clean(p.BriefDir)
	}
	return "id:" + strings.ToUpper(p.Session)
}

// Queue types lines in order per lane (agent folder or session), retrying
// blocked ones every RetryEvery for RetryWindow. It is persisted, so retries
// survive a host restart.
type Queue struct {
	Ctl   Ctl
	Clock Clock
	Path  string
	// Later is called for an outcome reached after the job that queued the line
	// returned (delivered on a retry, expired, failed, gone).
	Later func(Attempt)

	mu     sync.Mutex
	items  []Pending
	typing sync.Mutex
	loaded bool
	// finished holds lines' final attempts for a while, so Submit learns the
	// outcome when the retry loop reached its line first.
	finished map[string]Attempt

	afterPublish func() // tests: runs between Submit storing a line and its first attempt
}

func (q *Queue) now() time.Time { return q.Clock.Now() }

func (q *Queue) load() error {
	if q.loaded {
		return nil
	}
	q.loaded = true
	data, err := os.ReadFile(q.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &q.items)
}

func (q *Queue) save() error {
	if err := os.MkdirAll(filepath.Dir(q.Path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(q.items, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(q.Path, data, 0o600)
}

// Items returns a copy of the pending lines.
func (q *Queue) Items() ([]Pending, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.load(); err != nil {
		return nil, err
	}
	return append([]Pending(nil), q.items...), nil
}

// Submit queues a line and immediately tries to type the session's queue in
// order. It returns the attempt for this line (Blocked when it stays queued).
func (q *Queue) Submit(ctx context.Context, p Pending) (Attempt, error) {
	q.mu.Lock()
	if err := q.load(); err != nil {
		q.mu.Unlock()
		return Attempt{}, err
	}
	for _, it := range q.items {
		if p.EventID != "" && it.EventID == p.EventID {
			q.mu.Unlock()
			return Attempt{Pending: it, Outcome: Blocked, Detail: "already queued"}, nil
		}
	}
	if a, ok := q.finished[p.EventID]; ok && p.EventID != "" {
		q.mu.Unlock()
		return a, nil
	}
	now := q.now()
	p.Created, p.NextAt = now, now
	q.items = append(q.items, p)
	err := q.save()
	q.mu.Unlock()
	if err != nil {
		return Attempt{}, err
	}
	mine := Attempt{Pending: p, Outcome: Blocked, Detail: "waiting behind earlier lines"}
	if q.afterPublish != nil {
		q.afterPublish()
	}
	seen := false
	for _, a := range q.flush(ctx, lane(p)) {
		if a.EventID == p.EventID {
			mine, seen = a, true
		} else {
			q.later(a)
		}
	}
	if !seen {
		q.mu.Lock()
		if a, ok := q.finished[p.EventID]; ok {
			mine = a // the retry loop typed it; it has already reported it
		}
		q.mu.Unlock()
	}
	return mine, nil
}

// Adopt moves the lines queued under sessions that are no longer live, for the
// same brief folder, to its live session, so they stay ahead of newer lines.
func (q *Queue) Adopt(briefDir, to string, live func(id string) bool) error {
	if briefDir == "" {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.load(); err != nil {
		return err
	}
	moved := false
	for i := range q.items {
		it := &q.items[i]
		if it.BriefDir == briefDir && !strings.EqualFold(it.Session, to) && !live(it.Session) {
			it.Session, moved = to, true
		}
	}
	if !moved {
		return nil
	}
	return q.save()
}

// markSubmitting durably flags a line as being submitted.
func (q *Queue) markSubmitting(eventID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].EventID == eventID {
			q.items[i].Submitting = true
			if err := q.save(); err != nil {
				q.items[i].Submitting = false
				return err
			}
			return nil
		}
	}
	return fmt.Errorf("line %s is no longer queued", eventID)
}

func (q *Queue) rememberFinished(a Attempt) {
	if q.finished == nil {
		q.finished = map[string]Attempt{}
	}
	for id, old := range q.finished {
		if q.now().Sub(old.Created) > 2*RetryWindow {
			delete(q.finished, id)
		}
	}
	q.finished[a.EventID] = a
}

// Tick retries every lane with a line due; Run calls it every 15 s.
func (q *Queue) Tick(ctx context.Context) {
	q.mu.Lock()
	if err := q.load(); err != nil {
		q.mu.Unlock()
		log.Printf("agterm queue: %v", err)
		return
	}
	now := q.now()
	due := map[string]bool{}
	var lanes []string
	for _, it := range q.items {
		if l := lane(it); !it.NextAt.After(now) && !due[l] {
			due[l] = true
			lanes = append(lanes, l)
		}
	}
	q.mu.Unlock()
	for _, l := range lanes {
		for _, a := range q.flush(ctx, l) {
			q.later(a)
		}
	}
}

// later reports outcomes that end a line's life to the Later callback.
func (q *Queue) later(a Attempt) {
	if q.Later != nil && a.Outcome != Blocked && a.Outcome != Gone && a.Outcome != Handled {
		q.Later(a)
	}
}

// Run retries until ctx ends.
func (q *Queue) Run(ctx context.Context) {
	t := time.NewTicker(RetryEvery / 4)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			q.Tick(ctx)
		}
	}
}

// flush types a lane's lines in order until one is not delivered.
func (q *Queue) flush(ctx context.Context, ln string) []Attempt {
	q.typing.Lock()
	defer q.typing.Unlock()
	var results []Attempt
	for {
		q.mu.Lock()
		var p Pending
		found := false
		for _, it := range q.items {
			if lane(it) == ln {
				p, found = it, true
				break
			}
		}
		q.mu.Unlock()
		if !found {
			return results
		}

		var a Attempt
		switch {
		case p.Submitting:
			a = Attempt{Pending: p, Outcome: Unconfirmed, Detail: "the host stopped while submitting this line; not retyped"}
		case p.Inbox != "" && Acked(p.Inbox, p.ItemID):
			a = Attempt{Pending: p, Outcome: Handled, Detail: "acked in the inbox"}
		case q.now().Sub(p.Created) >= RetryWindow:
			a = Attempt{Pending: p, Outcome: Expired, Detail: fmt.Sprintf("not typed within %s (last: %s)", RetryWindow, p.LastState)}
		default:
			a = q.attempt(ctx, p)
		}
		done := a.Outcome != Blocked && a.Outcome != Gone
		q.mu.Lock()
		for i := range q.items {
			if q.items[i].EventID != p.EventID || q.items[i].Session != p.Session {
				continue
			}
			if done {
				q.items = append(q.items[:i], q.items[i+1:]...)
			} else {
				q.items[i].Attempts++
				q.items[i].NextAt = q.now().Add(RetryEvery)
				q.items[i].LastState = a.State.State
				q.items[i].Draft = a.Draft
			}
			break
		}
		if done {
			q.rememberFinished(a)
		}
		if a.Session != p.Session { // the brief folder names a successor (or the id was short)
			q.retarget(p.Session, a.Session)
			ln = lane(Pending{Session: a.Session, BriefDir: p.BriefDir})
		}
		if err := q.save(); err != nil {
			log.Printf("agterm queue: save: %v", err)
		}
		q.mu.Unlock()
		results = append(results, a)
		if !done {
			return results
		}
	}
}

// retarget moves the lines naming session from to session to; a draft seen in
// the old session's composer means nothing in the new one.
func (q *Queue) retarget(from, to string) {
	for i := range q.items {
		if q.items[i].Session == from {
			q.items[i].Session, q.items[i].Draft = to, ""
		}
	}
}

// attempt types one line if its session (or the successor named by the brief
// folder's state.json) is ready.
func (q *Queue) attempt(ctx context.Context, p Pending) Attempt {
	a := Attempt{Pending: p}
	sessions, err := q.Ctl.Sessions(ctx)
	if err != nil {
		a.Outcome, a.Detail = Blocked, "agterm unreachable: "+err.Error()
		return a
	}
	s, ok := ResolveLive(sessions, p.Session, p.BriefDir)
	if !ok {
		a.Outcome, a.Detail = Gone, "no live session"
		return a
	}
	a.Session = s.ID
	c, _, err := Observe(ctx, q.Ctl, s)
	if err != nil {
		a.Outcome, a.Detail = Blocked, err.Error()
		return a
	}
	if c.State == Ready && p.Draft != "" && c.Content == p.Draft {
		c.State, c.Reason = Draft, "the draft an earlier try found, unchanged"
	}
	a.State = c
	if c.State != Ready {
		a.Outcome, a.Detail = Blocked, fmt.Sprintf("composer %s: %s", c.State, c.Reason)
		return a
	}
	a.Draft = ""
	tx := &Transaction{Pane: &sessionPane{ctl: q.Ctl, s: s, agent: c.Agent}, Agent: c.Agent, Clock: q.Clock,
		BeforeSubmit: func() error { return q.markSubmitting(p.EventID) }}
	err = tx.Send(ctx, p.Line, ComposerReading{Content: c.Content, Cursor: c.Cursor})
	var dirty *ErrDirty
	switch {
	case err == nil:
		a.Outcome = Delivered
	case errors.As(err, &dirty) && dirty.Cleared:
		a.Outcome, a.Detail = Blocked, err.Error()
		if dirty.Restored { // the composer held someone's draft: leave it alone from now on
			a.Draft = c.Content
			a.State.State, a.State.Reason = Draft, "a draft with the caret at its start"
		}
	case errors.Is(err, ErrAmbiguous) || strings.Contains(err.Error(), "do not resend"):
		a.Outcome, a.Detail = Unconfirmed, err.Error()
	default:
		a.Outcome, a.Detail = Failed, err.Error()
	}
	return a
}

// ResolveLive finds the session to type into: the id itself while it is live,
// else the live session the brief folder's state.json names (a successor).
func ResolveLive(sessions []Session, id, briefDir string) (Session, bool) {
	if s, ok := FindSession(sessions, id); ok {
		return s, true
	}
	if briefDir == "" {
		return Session{}, false
	}
	if next := ActiveAgent(briefDir); next != "" && !strings.EqualFold(next, id) {
		return FindSession(sessions, next)
	}
	return Session{}, false
}
