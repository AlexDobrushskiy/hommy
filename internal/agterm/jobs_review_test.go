package agterm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func lineBodies(lines []string) []string {
	var out []string
	for _, l := range lines {
		out = append(out, l[strings.Index(l, "] ")+2:])
	}
	return out
}

// A line queued for a session that then handed over is typed into the successor
// before a newer line, whether the newer one names the old session or the new one.
func TestHandoverKeepsTheOrderOfQueuedLines(t *testing.T) {
	for _, via := range []string{sidA, sidB} {
		t.Run(via[:4], func(t *testing.T) {
			f := newJobFixture(t)
			dir := f.briefDir("2026-10-31-ev-test", sidA)
			f.ctl.add(sidA).menu = true
			ctx := context.Background()
			if res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 50, Text: "first"}); res.Status != StatusInbox {
				t.Fatalf("first = %+v", res)
			}
			f.ctl.remove(sidA)
			succ := f.ctl.add(sidB)
			if err := UpdateState(dir, map[string]any{"active_agent": sidB, "handover_from": sidA}, []string{"active_agent", "handover_from"}); err != nil {
				t.Fatal(err)
			}
			if res := f.svc.Type(ctx, TypeRequest{Session: via, MsgID: 51, Text: "second"}); res.Status != StatusOK {
				t.Fatalf("second = %+v", res)
			}
			if got := strings.Join(lineBodies(succ.submitted), ","); got != "first,second" {
				t.Fatalf("successor got %q", got)
			}
		})
	}
}

// The successor starts between Type's session snapshot and the queue's flush:
// the line is typed into it at once, and Type says so (✍, not 🤷).
func TestTypeReportsADeliveryToASuccessorThatJustStarted(t *testing.T) {
	f := newJobFixture(t)
	dir := f.briefDir("2026-10-31-ev-test", sidA)
	f.ctl.add(sidA)
	ctx := context.Background()
	if res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 60, Text: "one"}); res.Status != StatusOK {
		t.Fatalf("one = %+v", res)
	}
	f.ctl.remove(sidA)
	var succ *jobSession
	f.svc.Queue.afterPublish = func() {
		if succ == nil {
			succ = f.ctl.add(sidB)
			if err := UpdateState(dir, map[string]any{"active_agent": sidB, "handover_from": sidA}, []string{"active_agent", "handover_from"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 61, Text: "two"})
	if succ == nil || len(succ.submitted) != 1 {
		t.Fatalf("successor got %v", succ)
	}
	if res.Status != StatusOK || res.Queued || res.Session != sidB {
		t.Fatalf("result = %+v (the line was typed)", res)
	}
}

// /status shows 8-character ids; a target picked that way still follows the
// session's handover after it closes.
func TestShortTargetFollowsAHandoverAfterClose(t *testing.T) {
	f := newJobFixture(t)
	dir := f.briefDir("2026-10-31-ev-test", sidA)
	f.ctl.add(sidA)
	ctx := context.Background()
	short := strings.ToLower(sidA[:8])
	if res := f.svc.Type(ctx, TypeRequest{Session: short, MsgID: 52, Text: "one"}); res.Status != StatusOK {
		t.Fatalf("one = %+v", res)
	}
	f.ctl.remove(sidA)
	succ := f.ctl.add(sidB)
	if err := UpdateState(dir, map[string]any{"active_agent": sidB, "handover_from": sidA}, []string{"active_agent", "handover_from"}); err != nil {
		t.Fatal(err)
	}
	res := f.svc.Type(ctx, TypeRequest{Session: short, MsgID: 53, Text: "two"})
	if res.Status != StatusOK || res.Session != sidB || len(succ.submitted) != 1 {
		t.Fatalf("result = %+v, successor got %q", res, succ.submitted)
	}
	if res.Inbox != filepath.Join(dir, "inbox.jsonl") {
		t.Fatalf("inbox = %s", res.Inbox)
	}
}

// Every owner key of state.json finds the folder, not only the first one set.
func TestBriefLookupChecksEveryOwnerKey(t *testing.T) {
	f := newJobFixture(t)
	dir := filepath.Join(f.env.BriefsRoot, "2026-10-31-watch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st := `{"active_agent": "` + sidA + `", "active_watcher": "` + sidB + `"}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(st), 0o644); err != nil {
		t.Fatal(err)
	}
	f.ctl.add(sidB)
	r := f.svc.Files(context.Background(), BriefRequest{Session: sidB})
	if real, _ := filepath.EvalSymlinks(dir); r.Status != StatusOK || r.BriefDir != real {
		t.Fatalf("files = %+v", r)
	}
}

// inbox_append and type with the same short id write one inbox file.
func TestInboxAppendWithAShortIDUsesTheSessionsInbox(t *testing.T) {
	f := newJobFixture(t)
	f.ctl.add(sidA)
	ctx := context.Background()
	short := strings.ToLower(sidA[:8])
	ia := f.svc.InboxAppend(ctx, InboxAppendRequest{Session: short, MsgID: 54, Text: "note"})
	tr := f.svc.Type(ctx, TypeRequest{Session: short, MsgID: 55, Text: "hi"})
	want := filepath.Join(f.env.FallbackInbox, sidA+".jsonl")
	if ia.Path != want || tr.Inbox != want {
		t.Fatalf("inbox_append %s, type %s, want %s", ia.Path, tr.Inbox, want)
	}
	if recs := readInbox(t, want); len(recs) != 2 || recs[0].Session != sidA {
		t.Fatalf("inbox = %+v", recs)
	}
}

// A new_session job re-pushed after a host restart resumes the session it
// created instead of starting another one.
func TestNewSessionReplayResumesTheSameSession(t *testing.T) {
	f := newJobFixture(t)
	ctx := context.Background()
	req := NewSessionRequest{Dir: "~/work", Slug: "ev-new", MsgID: 56, Text: "find me a car", EventID: "tg:1:56"}
	r1 := f.svc.NewSession(ctx, req)
	// host restart: fresh service state, same files
	f.svc = NewService(f.ctl, f.env, nil)
	f.svc.Clock, f.svc.Queue.Clock = f.clock, f.clock
	f.svc.ClaudeCommand = "claude-cmd"
	r2 := f.svc.NewSession(ctx, req)
	if len(f.ctl.created) != 1 {
		t.Fatalf("created %d sessions", len(f.ctl.created))
	}
	if r2.Session != r1.Session || r2.BriefDir != r1.BriefDir || r2.Status != StatusOK {
		t.Fatalf("replay = %+v, first = %+v", r2, r1)
	}
	if s := f.ctl.sessions[r1.Session]; len(s.submitted) != 1 {
		t.Fatalf("first message typed %d times", len(s.submitted))
	}
}

// A re-pushed new_session job that can't list agterm's sessions neither starts
// a second session nor calls the one it started closed.
func TestNewSessionReplayWithoutSessionListStartsNoSecond(t *testing.T) {
	for _, recorded := range []bool{true, false} {
		f := newJobFixture(t)
		ctx := context.Background()
		req := NewSessionRequest{Dir: "~/work", Slug: "ev-new", MsgID: 56, Text: "find me a car", EventID: "tg:1:56"}
		brief := filepath.Join(f.env.BriefsRoot, "2026-10-31-ev-new")
		if err := os.MkdirAll(brief, 0o755); err != nil {
			t.Fatal(err)
		}
		cwd, _ := filepath.EvalSymlinks(filepath.Join(f.env.Home, "work"))
		s := f.ctl.add(sidA)
		s.Name, s.Workspace, s.Cwd = "ev-new", DefaultWorkspace, cwd
		rec := spawnRecord{BriefDir: brief, Cwd: cwd, Slug: "ev-new"}
		if recorded {
			rec.Session = sidA
		}
		if err := f.svc.spawned.put(req.EventID, rec, f.clock.Now()); err != nil {
			t.Fatal(err)
		}
		f.ctl.sessionsErr = errors.New("agtermctl: connection refused")
		r := f.svc.NewSession(ctx, req)
		if len(f.ctl.created) != 0 {
			t.Errorf("recorded=%v: started %d more sessions", recorded, len(f.ctl.created))
		}
		if r.Status == StatusNoSession {
			t.Errorf("recorded=%v: a live session reported closed: %+v", recorded, r)
		}
		if r.Status == StatusOK {
			t.Errorf("recorded=%v: ok without seeing agterm: %+v", recorded, r)
		}
	}
}

// crashBeforeFinish drops a spawn's stored result, as if the host died after
// the run's last step but before recording it.
func crashBeforeFinish(t *testing.T, f *jobFixture, eventID string) {
	t.Helper()
	rec, ok := f.svc.spawned.get(eventID)
	if !ok {
		t.Fatal("no spawn record")
	}
	rec.Result = nil
	if err := f.svc.spawned.put(eventID, rec, f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	f.svc = NewService(f.ctl, f.env, nil)
	f.svc.Clock, f.svc.Queue.Clock = f.clock, f.clock
	f.svc.ClaudeCommand = "claude-cmd"
}

// A replay after the first message was typed never closes the session that is
// now working on it.
func TestNewSessionReplayAfterTypingKeepsTheWorkingSession(t *testing.T) {
	f := newJobFixture(t)
	ctx := context.Background()
	req := NewSessionRequest{Dir: "~/work", Slug: "ev-new", MsgID: 56, Text: "find me a car", EventID: "tg:1:56"}
	r1 := f.svc.NewSession(ctx, req)
	if r1.Status != StatusOK {
		t.Fatalf("first run %+v", r1)
	}
	crashBeforeFinish(t, f, req.EventID)
	f.ctl.sessions[r1.Session].working = true // the first prompt's turn runs for minutes
	r2 := f.svc.NewSession(ctx, req)
	if len(f.ctl.closed) != 0 {
		t.Fatalf("closed %q: the session was working on the first message", f.ctl.closed)
	}
	if !r2.Created || r2.Session != r1.Session || len(f.ctl.created) != 1 {
		t.Fatalf("replay %+v, created %d", r2, len(f.ctl.created))
	}
}

// A session recovered on replay (created, id not yet recorded) is the one
// closed when its composer never gets ready.
func TestNewSessionReplayRecoveredSessionIsClosedByID(t *testing.T) {
	f := newJobFixture(t)
	ctx := context.Background()
	req := NewSessionRequest{Dir: "~/work", Slug: "ev-new", MsgID: 56, Text: "find me a car", EventID: "tg:1:56"}
	brief := filepath.Join(f.env.BriefsRoot, "2026-10-31-ev-new")
	if err := os.MkdirAll(brief, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, _ := filepath.EvalSymlinks(filepath.Join(f.env.Home, "work"))
	s := f.ctl.add(sidA)
	s.Name, s.Workspace, s.Cwd, s.menu = "ev-new", DefaultWorkspace, cwd, true
	if err := f.svc.spawned.put(req.EventID, spawnRecord{BriefDir: brief, Cwd: cwd, Slug: "ev-new"}, f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	r := f.svc.NewSession(ctx, req)
	if len(f.ctl.closed) != 1 || f.ctl.closed[0] != sidA || r.Created {
		t.Fatalf("closed %q, result %+v", f.ctl.closed, r)
	}
}

// A replay keeps a brief.md that was changed after the first run wrote it.
func TestNewSessionReplayKeepsTheBrief(t *testing.T) {
	f := newJobFixture(t)
	ctx := context.Background()
	req := NewSessionRequest{Dir: "~/work", Slug: "ev-new", MsgID: 56, Text: "find me a car", EventID: "tg:1:56"}
	r1 := f.svc.NewSession(ctx, req)
	crashBeforeFinish(t, f, req.EventID)
	brief := filepath.Join(r1.BriefDir, "brief.md")
	if err := os.WriteFile(brief, []byte("find me a car\n\n## plan (edited by the agent)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.svc.NewSession(ctx, req)
	if b, _ := os.ReadFile(brief); !strings.Contains(string(b), "edited by the agent") {
		t.Fatalf("brief.md rewritten: %q", b)
	}
}

// A file with a 255-character name (valid in Telegram and on disk) is saved
// under a shortened name that keeps its extension.
func TestSaveAttachmentLongName(t *testing.T) {
	dir := t.TempDir()
	name := strings.Repeat("n", 251) + ".pdf"
	p, err := saveAttachment(dir, 1234567, &Attachment{Name: name, Ext: ".pdf", Data: []byte("PDF")})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "PDF" || filepath.Ext(p) != ".pdf" || !strings.HasPrefix(filepath.Base(p), "1234567-nnn") {
		t.Fatalf("saved %s: %q", p, b)
	}
}

// the owner's draft with the caret moved back to its start reads like Claude's ghost
// suggestion (text at column 2); the line must leave the draft exactly as it was.
func TestTypeLeavesADraftWithTheCaretAtItsStart(t *testing.T) {
	f := newJobFixture(t)
	s := f.ctl.add(sidA)
	s.body, s.back = "my own words", len("my own words")
	ctx := context.Background()
	res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 57, Text: "hi"})
	if res.Status != StatusInbox || !res.Queued {
		t.Fatalf("result = %+v", res)
	}
	if s.body != "my own words" || s.caretCol() != 2 || len(s.submitted) != 0 {
		t.Fatalf("draft touched: body=%q caret=%d submitted=%q", s.body, s.caretCol(), s.submitted)
	}
	keys := s.keys
	for i := 0; i < 3; i++ {
		f.clock.Sleep(RetryEvery)
		f.svc.Queue.Tick(ctx)
	}
	if s.keys != keys {
		t.Fatalf("retries typed into the same draft again (%d → %d keystroke batches)", keys, s.keys)
	}
	s.body, s.back = "", 0
	f.clock.Sleep(RetryEvery)
	f.svc.Queue.Tick(ctx)
	if got := lineBodies(s.submitted); len(got) != 1 || got[0] != "hi" {
		t.Fatalf("submitted = %q", s.submitted)
	}
}

// A draft whose caret sits on a fresh empty row (Shift+Enter, "\"+Return) is a
// draft: nothing is typed.
func TestTypeNeverTypesBelowADraftLine(t *testing.T) {
	f := newJobFixture(t)
	s := f.ctl.add(sidA)
	s.body = "first line of my draft\n"
	res := f.svc.Type(context.Background(), TypeRequest{Session: sidA, MsgID: 58, Text: "hi"})
	if res.Status != StatusInbox || res.State != Draft {
		t.Fatalf("result = %+v", res)
	}
	if s.keys != 0 || s.body != "first line of my draft\n" {
		t.Fatalf("typed into the draft: keys=%d body=%q", s.keys, s.body)
	}
}

// Claude Code reads "\" + Return as a new line; a message ending in a backslash
// is still submitted.
func TestTypeSubmitsAMessageEndingInABackslash(t *testing.T) {
	f := newJobFixture(t)
	s := f.ctl.add(sidA)
	res := f.svc.Type(context.Background(), TypeRequest{Session: sidA, MsgID: 59, Text: `copy it to C:\temp\`})
	if res.Status != StatusOK || res.Detail != "" {
		t.Fatalf("result = %+v", res)
	}
	if len(s.submitted) != 1 || !strings.HasPrefix(lineBodies(s.submitted)[0], `copy it to C:\temp\`) {
		t.Fatalf("submitted = %q (composer %q)", s.submitted, s.body)
	}
}

const sidC = "CCCC3333-0000-0000-0000-000000000000"

// 10 e2e row 3: replies to an ended agent's ping. Reply 1 resolves to the live
// successor S2 and waits behind a draft there; S2 dies; replies 2 and 3 find no
// live session; S3 takes over the folder. S3 gets 1, 2, 3, once each, in order,
// also when the host restarts before S3 starts.
func TestEverythingPendingForAnAgentReachesTheSuccessorInOrder(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			f := newJobFixture(t)
			dir := f.briefDir("2026-11-02-hc10-agent", sidA)
			f.ctl.add(sidA)
			ctx := context.Background()
			if res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 980, Text: "zero"}); res.Status != StatusOK {
				t.Fatalf("zero = %+v", res)
			}
			f.ctl.remove(sidA) // S1 closed (its ping is what the owner replies to)
			s2 := f.ctl.add(sidB)
			s2.body = "my local draft"
			if err := UpdateState(dir, map[string]any{"active_agent": sidB}, []string{"active_agent"}); err != nil {
				t.Fatal(err)
			}
			if res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 987, Text: "ORDER-1"}); res.Status != StatusInbox {
				t.Fatalf("reply 1 = %+v", res)
			}
			f.clock.Sleep(3 * time.Second)
			f.ctl.remove(sidB) // S2 killed
			for i, id := range []int64{988, 989} {
				res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: id, Text: fmt.Sprintf("ORDER-%d", i+2)})
				if res.Status != StatusNoSession || !res.Queued {
					t.Fatalf("reply %d = %+v", i+2, res)
				}
				f.clock.Sleep(2 * time.Second)
			}
			if restart {
				f.svc = NewService(f.ctl, f.env, nil)
				f.svc.Clock, f.svc.Queue.Clock = f.clock, f.clock
			}
			f.clock.Sleep(15 * time.Second)
			s3 := f.ctl.add(sidC)
			if err := UpdateState(dir, map[string]any{"active_agent": sidC}, []string{"active_agent"}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 8; i++ {
				f.svc.Queue.Tick(ctx)
				f.clock.Sleep(RetryEvery / 4)
			}
			if got := strings.Join(lineBodies(s3.submitted), ","); got != "ORDER-1,ORDER-2,ORDER-3" {
				t.Fatalf("successor got %q", got)
			}
			if items, _ := f.svc.Queue.Items(); len(items) != 0 {
				t.Fatalf("still queued: %+v", items)
			}
		})
	}
}
