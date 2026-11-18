package agterm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A line queued before the host went down for longer than the retry window is
// not typed when the host comes back to a ready session.
func TestExpiredLineIsNotTypedAfterDowntime(t *testing.T) {
	f := newJobFixture(t)
	s := f.ctl.add(sidA)
	s.menu = true
	ctx := context.Background()
	f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 40, Text: "stale"})
	f.clock.Sleep(RetryWindow + time.Minute) // host down
	s.menu = false
	f.svc.Queue.Tick(ctx)
	if len(s.submitted) != 0 {
		t.Fatalf("typed after the window: %q", s.submitted)
	}
	if len(f.later) != 1 || f.later[0].Outcome != Expired {
		t.Fatalf("later = %+v", f.later)
	}
}

// The host dies right after Return: the restarted host must not type the line again.
func TestInterruptedSubmitIsNotRetypedAfterRestart(t *testing.T) {
	f := newJobFixture(t)
	s := f.ctl.add(sidA)
	s.menu = true
	ctx := context.Background()
	f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 41, Text: "once only", EventID: "ev-41"})
	crash := filepath.Join(t.TempDir(), "typing-queue.json")
	s.onType = func(text string) {
		if text == "\r" { // the disk as a crash right after Return leaves it
			if b, err := os.ReadFile(f.svc.Queue.Path); err == nil {
				_ = os.WriteFile(crash, b, 0o600)
			}
		}
	}
	s.menu = false
	f.clock.Sleep(RetryEvery)
	f.svc.Queue.Tick(ctx)
	if len(s.submitted) != 1 {
		t.Fatalf("submitted = %q", s.submitted)
	}
	s.onType = nil
	var later []Attempt
	q := &Queue{Ctl: f.ctl, Clock: f.clock, Path: crash, Later: func(a Attempt) { later = append(later, a) }}
	f.clock.Sleep(RetryEvery)
	q.Tick(ctx)
	if len(s.submitted) != 1 {
		t.Fatalf("retyped after restart: %q", s.submitted)
	}
	if len(later) != 1 || later[0].Outcome != Unconfirmed || later[0].EventID != "ev-41" {
		t.Fatalf("later = %+v", later)
	}
	if items, _ := q.Items(); len(items) != 0 {
		t.Fatalf("queue = %+v", items)
	}
}

// The retry loop delivering a just-submitted line first still gives Submit the
// real outcome.
func TestSubmitReportsAnOutcomeATickReachedFirst(t *testing.T) {
	f := newJobFixture(t)
	s := f.ctl.add(sidA)
	ctx := context.Background()
	f.svc.Queue.afterPublish = func() { f.svc.Queue.Tick(ctx) }
	res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 42, Text: "hi"})
	if res.Status != StatusOK || res.Queued {
		t.Fatalf("result = %+v", res)
	}
	if len(s.submitted) != 1 {
		t.Fatalf("submitted = %q", s.submitted)
	}
}
