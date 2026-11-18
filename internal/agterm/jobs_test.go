package agterm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// jobClock advances only when slept on.
type jobClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *jobClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *jobClock) Sleep(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// jobSession is an in-memory Claude Code session: a composer that inserts at
// its caret, deletes on DEL, turns "\" + Return into a new row (as Claude Code
// does) and otherwise submits on "\r"; it can show a menu or run a turn.
type jobSession struct {
	Session
	body      string
	back      int // caret position, in runes before the end of body
	menu      bool
	trust     int // >0: a folder-trust dialog; 1 = "No" selected, 2 = "Yes"
	working   bool
	submitted []string
	typedAt   []time.Time
	keys      int // Type calls that reached the composer
	onType    func(text string)
}

// caretCol is the cursor column: the caret's offset in its row, after "❯ ".
func (s *jobSession) caretCol() int {
	before := []rune(s.body)
	before = before[:len(before)-s.back]
	row := string(before)
	if i := strings.LastIndex(row, "\n"); i >= 0 {
		row = row[i+1:]
	}
	return 2 + len([]rune(row))
}

func (s *jobSession) screen() string {
	if s.trust > 0 {
		no, yes := " ❯ No, exit", "   Yes, I trust this folder"
		if s.trust == 2 {
			no, yes = "   No, exit", " ❯ Yes, I trust this folder"
		}
		return " Quick safety check\n\n" + no + "\n" + yes + "\n\n Enter to confirm · Esc to cancel\n"
	}
	if s.menu {
		return "⏺ Bash(touch x)\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n\n Esc to cancel · Tab to amend\n"
	}
	box := "❯ " + strings.ReplaceAll(s.body, "\n", "\n  ")
	return "⏺ earlier\n\n" + rule + "\n" + box + "\n" + rule + "\n  ~/x | Opus 5.5\n  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"
}

type jobCtl struct {
	mu       sync.Mutex
	sessions map[string]*jobSession
	order    []string
	clock    *jobClock
	created  []NewSessionOpts
	closed   []string
	// newSession returns the session a NewSession call creates.
	newSession func(o NewSessionOpts) *jobSession
	// sessionsErr fails Sessions (agtermctl tree) while set.
	sessionsErr error
}

func newJobCtl(clock *jobClock) *jobCtl {
	return &jobCtl{sessions: map[string]*jobSession{}, clock: clock}
}

func (c *jobCtl) add(id string) *jobSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &jobSession{Session: Session{ID: id, Name: "s-" + id[:4], Foreground: []string{"claude"}, Title: "✳ test", Cwd: "/nowhere"}}
	c.sessions[id] = s
	c.order = append(c.order, id)
	return s
}

func (c *jobCtl) remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, id)
}

func (c *jobCtl) Sessions(context.Context) ([]Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionsErr != nil {
		return nil, c.sessionsErr
	}
	var out []Session
	for _, id := range c.order {
		if s, ok := c.sessions[id]; ok {
			sess := s.Session
			if s.working {
				sess.Title = "◐ test"
			}
			out = append(out, sess)
		}
	}
	return out, nil
}

func (c *jobCtl) get(id string) (*jobSession, error) {
	s, ok := c.sessions[id]
	if !ok {
		return nil, fmt.Errorf("no such session %s", id)
	}
	return s, nil
}

func (c *jobCtl) Text(_ context.Context, s Session, _ int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	js, err := c.get(s.ID)
	if err != nil {
		return "", err
	}
	return js.screen(), nil
}

func (c *jobCtl) Cursor(_ context.Context, s Session) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	js, err := c.get(s.ID)
	if err != nil {
		return 0, err
	}
	if js.menu || js.trust > 0 {
		return 1, nil
	}
	return js.caretCol(), nil
}

func (c *jobCtl) Type(_ context.Context, s Session, text string) error {
	c.mu.Lock()
	js, err := c.get(s.ID)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	hook := js.onType
	switch {
	case js.trust > 0:
		if text == "\x1b[B" {
			js.trust = 2
		} else if text == "\r" && js.trust == 2 {
			js.trust = 0
		}
	case js.menu:
	default:
		js.keys++
		for _, r := range text {
			rs := []rune(js.body)
			at := len(rs) - js.back
			switch {
			case r == '\r' && at > 0 && rs[at-1] == '\\':
				rs[at-1] = '\n'
				js.body = string(rs)
			case r == '\r':
				js.submitted = append(js.submitted, js.body)
				js.typedAt = append(js.typedAt, c.clock.Now())
				js.body, js.back = "", 0
			case r == 0x7f:
				if at > 0 {
					js.body = string(rs[:at-1]) + string(rs[at:])
				}
			default:
				js.body = string(rs[:at]) + string(r) + string(rs[at:])
			}
		}
	}
	c.mu.Unlock()
	if hook != nil {
		hook(text)
	}
	return nil
}

func (c *jobCtl) Close(_ context.Context, s Session) error {
	c.mu.Lock()
	c.closed = append(c.closed, s.ID)
	c.mu.Unlock()
	c.remove(s.ID)
	return nil
}

func (c *jobCtl) NewSession(_ context.Context, o NewSessionOpts) (string, error) {
	c.mu.Lock()
	c.created = append(c.created, o)
	c.mu.Unlock()
	id := fmt.Sprintf("NEW%05d-0000-0000-0000-000000000000", len(c.created))
	s := c.add(id)
	s.Name, s.Cwd = o.Name, o.Cwd
	if c.newSession != nil {
		c.mu.Lock()
		*s = *c.newSession(o)
		s.ID = id
		c.mu.Unlock()
	}
	return id, nil
}

type jobFixture struct {
	t      *testing.T
	clock  *jobClock
	ctl    *jobCtl
	svc    *Service
	env    Env
	later  []Attempt
	lateMu sync.Mutex
}

func newJobFixture(t *testing.T) *jobFixture {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	for _, d := range []string{home, filepath.Join(home, "agent-briefs"), filepath.Join(home, "work")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	clock := &jobClock{now: time.Date(2026, 10, 31, 19, 0, 0, 0, time.UTC)}
	f := &jobFixture{t: t, clock: clock, ctl: newJobCtl(clock)}
	f.env = Env{
		Home: home, BriefsRoot: filepath.Join(home, "agent-briefs"),
		FallbackInbox: filepath.Join(home, ".local/state/hommy/inbox"),
		StateDir:      filepath.Join(root, "state"), Host: "mac2", Now: clock.Now,
		Roots: []string{filepath.Join(home, "work")},
	}
	f.svc = NewService(f.ctl, f.env, func(a Attempt) {
		f.lateMu.Lock()
		f.later = append(f.later, a)
		f.lateMu.Unlock()
	})
	f.svc.Clock, f.svc.Queue.Clock = clock, clock
	f.svc.ClaudeCommand = "claude-cmd"
	f.svc.ComposerWait = time.Minute
	return f
}

func (f *jobFixture) briefDir(name, active string) string {
	f.t.Helper()
	dir := filepath.Join(f.env.BriefsRoot, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	st := fmt.Sprintf(`{"active_agent": %q, "cars": [1, 2], "notes": "keep me"}`, active)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(st), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func readInbox(t *testing.T, path string) []InboxRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []InboxRecord
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r InboxRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

const sidA = "AAAA1111-0000-0000-0000-000000000000"
const sidB = "BBBB2222-0000-0000-0000-000000000000"

func TestTypeReadyWritesInboxFirstThenTypes(t *testing.T) {
	f := newJobFixture(t)
	dir := f.briefDir("2026-10-31-ev-test", sidA)
	s := f.ctl.add(sidA)
	inboxSeen := false
	s.onType = func(string) {
		if _, err := os.Stat(filepath.Join(dir, "inbox.jsonl")); err == nil {
			inboxSeen = true
		}
	}
	res := f.svc.Type(context.Background(), TypeRequest{Session: "aaaa1111", MsgID: 8812, Text: "drop it\nnow", Quote: "🤖 dealer 3 asks a deposit"})
	if res.Status != StatusOK || res.Session != sidA {
		t.Fatalf("result = %+v", res)
	}
	if !inboxSeen {
		t.Fatal("inbox.jsonl was not written before the first keystroke")
	}
	want := `[Telegram · tg-8812 · re "🤖 dealer 3 asks a deposit"] drop it ⏎ now`
	if len(s.submitted) != 1 || s.submitted[0] != want {
		t.Fatalf("submitted = %q", s.submitted)
	}
	recs := readInbox(t, filepath.Join(dir, "inbox.jsonl"))
	if len(recs) != 1 || recs[0].Text != "drop it\nnow" || recs[0].ID != "tg-8812" || recs[0].Host != "mac2" {
		t.Fatalf("inbox = %+v", recs)
	}
}

func TestTypeBlockedAtMenuIsTypedOnceAfterIt(t *testing.T) {
	f := newJobFixture(t)
	s := f.ctl.add(sidA)
	s.menu = true
	ctx := context.Background()
	res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 1, Text: "go on"})
	if res.Status != StatusInbox || !res.Queued || res.State != Menu {
		t.Fatalf("result = %+v", res)
	}
	if len(s.submitted) != 0 || s.body != "" {
		t.Fatal("typed into a menu")
	}
	f.clock.Sleep(30 * time.Second)
	f.svc.Queue.Tick(ctx) // not due yet
	s.menu = false
	f.svc.Queue.Tick(ctx)
	if len(s.submitted) != 0 {
		t.Fatal("retried before 60 s")
	}
	f.clock.Sleep(31 * time.Second)
	f.svc.Queue.Tick(ctx)
	f.svc.Queue.Tick(ctx)
	if len(s.submitted) != 1 {
		t.Fatalf("submitted = %q, want one line", s.submitted)
	}
	if len(f.later) != 1 || f.later[0].Outcome != Delivered || f.later[0].MsgID != 1 {
		t.Fatalf("later = %+v", f.later)
	}
	if items, _ := f.svc.Queue.Items(); len(items) != 0 {
		t.Fatalf("queue = %+v", items)
	}
}

func TestTypeNeverTypesOverADraft(t *testing.T) {
	f := newJobFixture(t)
	s := f.ctl.add(sidA)
	s.body = "my own words"
	ctx := context.Background()
	if res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 2, Text: "hi"}); res.Status != StatusInbox || res.State != Draft {
		t.Fatalf("result = %+v", res)
	}
	for i := 0; i < 5; i++ {
		f.clock.Sleep(RetryEvery)
		f.svc.Queue.Tick(ctx)
	}
	if s.body != "my own words" || len(s.submitted) != 0 {
		t.Fatalf("draft touched: body=%q submitted=%q", s.body, s.submitted)
	}
	s.body = ""
	f.clock.Sleep(RetryEvery)
	f.svc.Queue.Tick(ctx)
	if len(s.submitted) != 1 || !strings.HasSuffix(s.submitted[0], "] hi") {
		t.Fatalf("submitted = %q", s.submitted)
	}
}

func TestTypeWaitsForARunningTurn(t *testing.T) {
	f := newJobFixture(t)
	s := f.ctl.add(sidA)
	s.working = true
	ctx := context.Background()
	if res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 3, Text: "hi"}); res.State != Working || res.Status != StatusInbox {
		t.Fatalf("result = %+v", res)
	}
	s.working = false
	f.clock.Sleep(RetryEvery)
	f.svc.Queue.Tick(ctx)
	if len(s.submitted) != 1 {
		t.Fatalf("submitted = %q", s.submitted)
	}
}

func TestQueuedLineExpiresAfter30Minutes(t *testing.T) {
	f := newJobFixture(t)
	s := f.ctl.add(sidA)
	s.menu = true
	ctx := context.Background()
	f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 4, Text: "hi"})
	for i := 0; i < 31; i++ {
		f.clock.Sleep(RetryEvery)
		f.svc.Queue.Tick(ctx)
	}
	if len(f.later) != 1 || f.later[0].Outcome != Expired {
		t.Fatalf("later = %+v", f.later)
	}
	if items, _ := f.svc.Queue.Items(); len(items) != 0 {
		t.Fatalf("queue = %+v", items)
	}
	s.menu = false
	f.clock.Sleep(RetryEvery)
	f.svc.Queue.Tick(ctx)
	if len(s.submitted) != 0 {
		t.Fatal("typed after expiry")
	}
}

// A line queued for a busy session that the agent handled from its inbox
// (`hommy inbox ack`) is done: never typed in again later, and no ⌛.
func TestQueuedLineAckedInTheInboxIsDropped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		freesUp  bool // the session becomes ready inside the retry window
		ticks    int
		wantType int
	}{
		{"session frees up after the ack", true, 1, 0},
		{"session stays busy past the window", false, 31, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newJobFixture(t)
			s := f.ctl.add(sidA)
			s.working = true
			ctx := context.Background()
			res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 7, Text: "I'd better pay by card"})
			if res.Status != StatusInbox || res.Inbox == "" {
				t.Fatalf("result = %+v", res)
			}
			// the agent reads it from the inbox and acks it, as `hommy inbox ack tg-7` does
			ack, err := os.OpenFile(AckedPath(res.Inbox), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = ack.WriteString(`{"id":"tg-7","ts":1}` + "\n")
			_ = ack.Close()
			s.working = !tc.freesUp
			for i := 0; i < tc.ticks; i++ {
				f.clock.Sleep(RetryEvery)
				f.svc.Queue.Tick(ctx)
			}
			if len(s.submitted) != tc.wantType {
				t.Errorf("typed again after the ack: %q", s.submitted)
			}
			if len(f.later) != 0 {
				t.Errorf("late outcome reported (the hub would post it): %+v", f.later)
			}
			if items, _ := f.svc.Queue.Items(); len(items) != 0 {
				t.Errorf("still queued: %+v", items)
			}
		})
	}
}

func TestQueuedLinesKeepTheirOrderAndSurviveARestart(t *testing.T) {
	f := newJobFixture(t)
	s := f.ctl.add(sidA)
	s.menu = true
	ctx := context.Background()
	f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 10, Text: "first"})
	f.clock.Sleep(time.Second)
	f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 11, Text: "second"})
	// host restart: a fresh queue on the same file
	q := &Queue{Ctl: f.ctl, Clock: f.clock, Path: f.svc.Queue.Path}
	s.menu = false
	// a third line while two wait: it must not jump the queue
	f.svc.Queue = q
	res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 12, Text: "third"})
	if res.Status != StatusOK {
		t.Fatalf("result = %+v", res)
	}
	var got []string
	for _, l := range s.submitted {
		got = append(got, l[strings.Index(l, "] ")+2:])
	}
	if strings.Join(got, ",") != "first,second,third" {
		t.Fatalf("order = %q", got)
	}
}

func TestTypeToClosedSessionKeepsTheInbox(t *testing.T) {
	f := newJobFixture(t)
	dir := f.briefDir("2026-10-31-gone", sidA)
	res := f.svc.Type(context.Background(), TypeRequest{Session: sidA, MsgID: 5, Text: "still there?"})
	if res.Status != StatusNoSession || res.Inbox != filepath.Join(dir, "inbox.jsonl") {
		t.Fatalf("result = %+v", res)
	}
	if recs := readInbox(t, res.Inbox); len(recs) != 1 || recs[0].Text != "still there?" {
		t.Fatalf("inbox = %+v", recs)
	}
	// no brief folder: the per-session fallback inbox
	res = f.svc.Type(context.Background(), TypeRequest{Session: sidB, MsgID: 6, Text: "x"})
	if res.Status != StatusNoSession || res.Inbox != filepath.Join(f.env.FallbackInbox, sidB+".jsonl") {
		t.Fatalf("result = %+v", res)
	}
}

func TestTypeFollowsAHandoverToTheSuccessor(t *testing.T) {
	f := newJobFixture(t)
	dir := f.briefDir("2026-10-31-ev-test", sidA)
	f.ctl.add(sidA)
	ctx := context.Background()
	// the host learns the session's brief folder while it is alive
	if res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 20, Text: "one"}); res.Status != StatusOK {
		t.Fatalf("result = %+v", res)
	}
	f.ctl.remove(sidA)
	succ := f.ctl.add(sidB)
	if err := UpdateState(dir, map[string]any{"active_agent": sidB, "handover_from": sidA}, []string{"active_agent", "handover_from"}); err != nil {
		t.Fatal(err)
	}
	res := f.svc.Type(ctx, TypeRequest{Session: sidA, MsgID: 21, Text: "two"})
	if res.Status != StatusOK || res.Session != sidB || len(succ.submitted) != 1 {
		t.Fatalf("result = %+v, successor got %q", res, succ.submitted)
	}
	if recs := readInbox(t, filepath.Join(dir, "inbox.jsonl")); len(recs) != 2 {
		t.Fatalf("inbox = %+v", recs)
	}
}

func TestTypeIsIdempotentPerEvent(t *testing.T) {
	f := newJobFixture(t)
	s := f.ctl.add(sidA)
	s.menu = true
	ctx := context.Background()
	r := TypeRequest{Session: sidA, MsgID: 7, Text: "once", EventID: "ev-7"}
	f.svc.Type(ctx, r)
	f.svc.Type(ctx, r)
	if items, _ := f.svc.Queue.Items(); len(items) != 1 {
		t.Fatalf("queue = %+v", items)
	}
	path := f.env.InboxPath("", sidA)
	if recs := readInbox(t, path); len(recs) != 1 {
		t.Fatalf("inbox = %+v", recs)
	}
}

func TestBriefJobs(t *testing.T) {
	f := newJobFixture(t)
	dir := f.briefDir("2026-10-31-ev-test", sidA)
	f.ctl.add(sidA)
	if err := os.WriteFile(filepath.Join(dir, "log.md"), []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(f.env.Home, "secret.txt")
	if err := os.WriteFile(outside, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if r := f.svc.Files(ctx, BriefRequest{Session: sidA}); r.Status != StatusOK || len(r.Files) != 3 {
		t.Fatalf("list = %+v", r)
	}
	if r := f.svc.Files(ctx, BriefRequest{Session: sidA, Path: "log.md", Tail: 2}); r.Status != StatusOK || r.Content != "c\nd" {
		t.Fatalf("tail = %+v", r)
	}
	for _, bad := range []string{"../../etc/passwd", "/etc/passwd", "link", "..", "sub/../../x"} {
		if r := f.svc.Files(ctx, BriefRequest{Session: sidA, Path: bad}); r.Status != StatusError || r.Content != "" {
			t.Fatalf("%s: %+v", bad, r)
		}
	}
	if r := f.svc.Files(ctx, BriefRequest{BriefDir: f.env.Home, Path: "secret.txt"}); r.Status != StatusError {
		t.Fatalf("brief_dir outside the briefs root: %+v", r)
	}
	if r := f.svc.Pause(ctx, BriefRequest{Session: sidA}); r.Status != StatusOK {
		t.Fatalf("pause = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "PAUSE")); err != nil {
		t.Fatal(err)
	}
	if r := f.svc.Done(ctx, BriefRequest{Session: sidA}); r.Status != StatusOK {
		t.Fatalf("done = %+v", r)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	got := string(data)
	order := []string{`"active_agent"`, `"cars"`, `"notes": "keep me"`, `"status": "done"`, `"done_at": "2026-10-31T19:00:00Z"`}
	last := -1
	for _, k := range order {
		i := strings.Index(got, k)
		if i <= last {
			t.Fatalf("state.json order/content wrong at %s:\n%s", k, got)
		}
		last = i
	}
	if r := f.svc.Files(ctx, BriefRequest{Session: sidB}); r.Status != StatusError {
		t.Fatalf("session without a brief folder: %+v", r)
	}
}

func TestPeek(t *testing.T) {
	f := newJobFixture(t)
	f.ctl.add(sidA)
	r := f.svc.Peek(context.Background(), PeekRequest{Session: sidA, Lines: 3})
	if r.Status != StatusOK || r.Composer.State != Ready || r.Text != "⏺ earlier\n" {
		t.Fatalf("peek = %+v", r)
	}
	if r := f.svc.Peek(context.Background(), PeekRequest{Session: sidB}); r.Status != StatusNoSession {
		t.Fatalf("peek gone = %+v", r)
	}
}

func TestNewSession(t *testing.T) {
	f := newJobFixture(t)
	f.ctl.newSession = func(o NewSessionOpts) *jobSession {
		return &jobSession{Session: Session{Name: o.Name, Cwd: o.Cwd, Foreground: []string{"/bin/zsh", "-l", "-c", "env HOME=/Users/tester claude; exec /bin/zsh -l"}, Title: "✳ x"}, trust: 1}
	}
	r := f.svc.NewSession(context.Background(), NewSessionRequest{Dir: "~/work", Slug: "ev-new", MsgID: 30, Text: "find me a car\nunder 30k"})
	if r.Status != StatusOK || r.State != Ready {
		t.Fatalf("result = %+v", r)
	}
	if len(f.ctl.created) != 1 {
		t.Fatalf("created = %+v", f.ctl.created)
	}
	o := f.ctl.created[0]
	work, _ := filepath.EvalSymlinks(filepath.Join(f.env.Home, "work"))
	if o.Workspace != DefaultWorkspace || o.Name != "ev-new" || o.Command != "claude-cmd" || o.Cwd != work {
		t.Fatalf("opts = %+v", o)
	}
	brief := filepath.Join(f.env.BriefsRoot, "2026-10-31-ev-new")
	if r.BriefDir != brief || ActiveAgent(brief) != r.Session {
		t.Fatalf("brief = %s active = %s", r.BriefDir, ActiveAgent(brief))
	}
	if b, _ := os.ReadFile(filepath.Join(brief, "brief.md")); string(b) != "find me a car\nunder 30k\n" {
		t.Fatalf("brief.md = %q", b)
	}
	s := f.ctl.sessions[r.Session]
	if len(s.submitted) != 1 || s.submitted[0] != "[Telegram · tg-30] find me a car ⏎ under 30k" {
		t.Fatalf("submitted = %q", s.submitted)
	}
	// a second session with the same slug gets its own folder
	r2 := f.svc.NewSession(context.Background(), NewSessionRequest{Dir: "~/work", Slug: "ev-new", MsgID: 31, Text: "x"})
	if r2.BriefDir != brief+"-2" {
		t.Fatalf("second brief = %s", r2.BriefDir)
	}
}

func TestNewSessionRefusesBadInput(t *testing.T) {
	f := newJobFixture(t)
	for _, r := range []NewSessionRequest{
		{Dir: "~/work", Slug: "../x", Text: "x"},
		{Dir: "/etc", Slug: "ok-slug", Text: "x"},
		{Dir: "~/missing", Slug: "ok-slug", Text: "x"},
		{Dir: "work", Slug: "ok-slug", Text: "x"},
	} {
		if res := f.svc.NewSession(context.Background(), r); res.Status != StatusError {
			t.Fatalf("%+v accepted: %+v", r, res)
		}
	}
	if len(f.ctl.created) != 0 {
		t.Fatal("a session was created")
	}
}

func TestSnapshot(t *testing.T) {
	f := newJobFixture(t)
	dir := f.briefDir("2026-10-31-ev-test", sidA)
	f.briefDir("2026-10-30-old", "DEAD0000-0000-0000-0000-000000000000")
	f.ctl.add(sidA).menu = true
	shell := f.ctl.add(sidB)
	shell.Foreground = nil
	f.ctl.sessions[sidB] = shell
	if err := os.WriteFile(filepath.Join(dir, "PAUSE"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	snap := f.svc.Snapshot(context.Background())
	if len(snap.Sessions) != 2 {
		t.Fatalf("sessions = %+v", snap.Sessions)
	}
	a := snap.Sessions[0]
	if a.Agent != Claude || a.State != Menu || a.BriefDir != dir {
		t.Fatalf("agent session = %+v", a)
	}
	if b := snap.Sessions[1]; b.Agent != "" || b.State != "" {
		t.Fatalf("shell session = %+v", b)
	}
	byName := map[string]AgentFolder{}
	for _, fo := range snap.Folders {
		byName[fo.Name] = fo
	}
	if fo := byName["2026-10-31-ev-test"]; !fo.Live || !fo.Paused || fo.ActiveAgent != sidA {
		t.Fatalf("folder = %+v", fo)
	}
	if fo := byName["2026-10-30-old"]; fo.Live {
		t.Fatalf("dead folder live: %+v", fo)
	}
}

// The board's unacked counts: items in the inbox without an ack line, per
// session (its brief folder's inbox) and per folder.
func TestSnapshotUnacked(t *testing.T) {
	f := newJobFixture(t)
	dir := f.briefDir("2026-10-31-ev-test", sidA)
	f.ctl.add(sidA)
	inbox := filepath.Join(dir, "inbox.jsonl")
	for _, id := range []string{"tg-1", "tg-2", "tg-3", "tg-2"} { // a duplicate line counts once
		if _, err := AppendInbox(inbox, InboxRecord{ID: id, EventID: id + "-" + fmt.Sprint(time.Now().UnixNano()), Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(AckedPath(inbox), []byte(`{"id":"tg-1","ts":1}`+"\n"+`not json`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := f.svc.Snapshot(context.Background())
	if len(snap.Sessions) != 1 || snap.Sessions[0].Unacked != 2 {
		t.Fatalf("session unacked = %+v", snap.Sessions)
	}
	if len(snap.Folders) != 1 || snap.Folders[0].Unacked != 2 {
		t.Fatalf("folder unacked = %+v", snap.Folders)
	}
	if n := Unacked(filepath.Join(dir, "missing.jsonl")); n != 0 {
		t.Fatalf("missing inbox = %d", n)
	}
}

// The console's peek is the screen as agterm shows it: the composer stays,
// trailing blank rows go.
func TestPeekScreenKeepsComposer(t *testing.T) {
	f := newJobFixture(t)
	f.ctl.add(sidA)
	plain := f.svc.Peek(context.Background(), PeekRequest{Session: sidA, Lines: 40})
	r := f.svc.Peek(context.Background(), PeekRequest{Session: sidA, Lines: 40, Screen: true})
	if r.Status != StatusOK || !strings.Contains(r.Text, "❯") || strings.Contains(plain.Text, "❯") {
		t.Fatalf("screen peek = %q, plain = %q", r.Text, plain.Text)
	}
	if strings.HasSuffix(r.Text, "\n") || strings.Contains(r.Text, " \n") {
		t.Fatalf("trailing blanks kept: %q", r.Text)
	}
}

// The command line agterm runs hands the extra arguments (a first prompt with quotes, $ and newlines) to the agent
// unchanged, after the permission flag.
func TestAgentCommandLineArgsQuoting(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS: /bin/zsh")
	}
	dir := t.TempDir()
	agent := filepath.Join(dir, "agent")
	os.WriteFile(agent, []byte("#!/bin/sh\nfor a; do printf '[%s]\\n' \"$a\"; done\nexit 0\n"), 0o755)
	msg := "it's \"quoted\" $HOME `x`\nline two"
	line := AgentCommandLineArgs(agent, dir, "", true, msg)
	cmd := exec.Command("/bin/sh", "-c", line)
	cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := "[--dangerously-skip-permissions]\n[" + msg + "]\n"
	if !strings.Contains(string(out), want) {
		t.Fatalf("got %q, want %q", out, want)
	}
}
