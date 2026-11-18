package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/host"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

const sessA = "AAAA1111-0000-0000-0000-000000000000"

// fakeAgterm is an in-memory agterm with Claude Code sessions: a one-row
// composer that submits on "\r"; menu=true shows a permission dialog.
type fakeAgterm struct {
	mu       sync.Mutex
	sessions map[string]*fakeSess
	n        int
	failType bool // every keystroke write fails (agtermctl errors)
	newMenu  bool // sessions it creates stay at a menu (their composer never gets ready)
	// synced (set once the host runs) makes the hub see a session the test
	// added: a heartbeat now, waited for. Without it /target could find a
	// heartbeat taken before the session existed ("is gone").
	synced func(id string)
	// onType runs before each keystroke write (outside the lock).
	onType func(id, text string)
}

type fakeSess struct {
	agterm.Session
	body      string
	menu      bool
	submitted []string
}

// add puts an idle Claude session in (a test's own session: the hub sees it
// before add returns).
func (f *fakeAgterm) add(id string) { f.addSess(id, "e2e-claude", "hc-e2e", false, true) }

// addMenu: a session showing a permission dialog.
func (f *fakeAgterm) addMenu(id string) { f.addSess(id, "e2e-claude", "hc-e2e", true, true) }

func (f *fakeAgterm) addSess(id, name, workspace string, menu, sync bool) {
	f.mu.Lock()
	f.sessions[id] = &fakeSess{Session: agterm.Session{ID: id, Name: name, Workspace: workspace, Foreground: []string{"claude"},
		Title: "✳ x"}, menu: menu}
	synced := f.synced
	f.mu.Unlock()
	if sync && synced != nil {
		synced(id)
	}
}

func (f *fakeAgterm) get(id string) (*fakeSess, error) {
	s, ok := f.sessions[id]
	if !ok {
		return nil, fmt.Errorf("no session %s", id)
	}
	return s, nil
}

func (f *fakeAgterm) Sessions(context.Context) ([]agterm.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []agterm.Session
	for _, s := range f.sessions {
		out = append(out, s.Session)
	}
	return out, nil
}

func (f *fakeAgterm) Text(_ context.Context, s agterm.Session, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fs, err := f.get(s.ID)
	if err != nil {
		return "", err
	}
	if fs.menu {
		return " Do you want to proceed?\n ❯ 1. Yes\n   2. No\n\n Esc to cancel · Tab to amend\n", nil
	}
	rule := strings.Repeat("─", 40)
	return rule + "\n❯ " + fs.body + "\n" + rule + "\n  ~/x | Opus\n", nil
}

func (f *fakeAgterm) Cursor(_ context.Context, s agterm.Session) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fs, err := f.get(s.ID)
	if err != nil {
		return 0, err
	}
	return 2 + len([]rune(fs.body)), nil
}

func (f *fakeAgterm) Type(_ context.Context, s agterm.Session, text string) error {
	f.mu.Lock()
	hook := f.onType
	f.mu.Unlock()
	if hook != nil {
		hook(s.ID, text)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fs, err := f.get(s.ID)
	if err != nil || fs.menu {
		return err
	}
	if f.failType {
		return fmt.Errorf("agtermctl session type: exit status 1")
	}
	for _, r := range text {
		switch r {
		case '\r':
			fs.submitted = append(fs.submitted, fs.body)
			fs.body = ""
		case 0x7f:
			if rs := []rune(fs.body); len(rs) > 0 {
				fs.body = string(rs[:len(rs)-1])
			}
		default:
			fs.body += string(r)
		}
	}
	return nil
}

func (f *fakeAgterm) Close(_ context.Context, s agterm.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, s.ID)
	return nil
}

func (f *fakeAgterm) NewSession(_ context.Context, o agterm.NewSessionOpts) (string, error) {
	f.mu.Lock()
	f.n++
	id := fmt.Sprintf("BEEF%04d-0000-0000-0000-000000000000", f.n)
	menu := f.newMenu
	f.mu.Unlock()
	f.addSess(id, o.Name, o.Workspace, menu, false) // the host's own call: it kicks its heartbeat itself
	return id, nil
}

func (f *fakeAgterm) submitted(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sessions[id].submitted...)
}

func (f *fakeAgterm) setMenu(id string, on bool) {
	f.mu.Lock()
	f.sessions[id].menu = on
	f.mu.Unlock()
}

// stepClock advances only when told (the typing retry window).
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *stepClock) Sleep(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type agtermRig struct {
	*rig
	ag    *fakeAgterm
	svc   *agterm.Service
	clock *stepClock
	ctx   context.Context
	h     *host.Host
	stop  func() // the host goes offline (the agterm sessions stay)
}

// startAgtermHost runs mac1's host with the agent-session jobs over a fake agterm.
func startAgtermHost(t *testing.T, mods ...func(*host.Config)) *agtermRig {
	return newRig(t).runAgtermHost(&fakeAgterm{sessions: map[string]*fakeSess{}}, mods...)
}

// runAgtermHost starts mac1's agent-session host on this rig over ag.
func (r *rig) runAgtermHost(ag *fakeAgterm, mods ...func(*host.Config)) *agtermRig {
	t := r.t
	cfg := r.hostConfig()
	cfg.AgentBriefsDir = filepath.Join(r.dir, "agent-briefs")
	cfg.NewRoots = []string{filepath.Join(r.dir, "work")}
	for _, m := range mods {
		m(&cfg)
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	h, err := host.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	clk := &stepClock{t: time.Now()}
	svc := host.RegisterAgterm(ctx, h, ag, func(s *agterm.Service) { s.Clock, s.Queue.Clock = clk, clk })
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	r.waitConnected(true)
	r.waitHeartbeat(func(proto.Heartbeat) bool { return true })
	ag.mu.Lock()
	ag.synced = func(id string) {
		h.HeartbeatNow()
		r.waitHeartbeat(func(hb proto.Heartbeat) bool {
			var snap agtermSnap
			_ = json.Unmarshal(hb.Agterm, &snap)
			_, n := findSession(snap.Sessions, id)
			return n == 1
		})
	}
	ag.mu.Unlock()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done; _ = h.Close() }) }
	t.Cleanup(stop)
	return &agtermRig{rig: r, ag: ag, svc: svc, clock: clk, ctx: ctx, h: h, stop: stop}
}

func reactedOn(id int, emoji string) func([]call) bool {
	return func(cs []call) bool {
		for _, c := range cs {
			if c.Kind == "react" && c.MsgID == id && c.Text == emoji {
				return true
			}
		}
		return false
	}
}

func TestSessionTargetTypesTheLine(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.add(sessA)
	r.text("/target session@mac1:" + strings.ToLower(sessA))
	r.tg.waitFor(t, "switch", has("edit", "🎯 Talking to: "))
	id := r.send(func(m *models.Message) {
		m.Text = "reply with the word pong\nplease"
		m.ReplyToMessage = &models.Message{ID: 5, Text: "🤖 ev: dealer asks a deposit"}
	})
	r.tg.waitFor(t, "✍", reactedOn(id, "✍"))
	want := fmt.Sprintf(`[Telegram · tg-%d · re "🤖 ev: dealer asks a deposit"] reply with the word pong ⏎ please`, id)
	if got := r.ag.submitted(sessA); len(got) != 1 || got[0] != want {
		t.Fatalf("typed %q, want %q", got, want)
	}
	inbox := filepath.Join(r.dir, "inbox", sessA+".jsonl")
	if b, err := os.ReadFile(inbox); err != nil || !strings.Contains(string(b), `"text":"reply with the word pong\nplease"`) {
		t.Fatalf("inbox %s: %s %v", inbox, b, err)
	}
}

func TestSessionAtMenuQueuesThenTypesOnce(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.addMenu(sessA)
	r.text("/target session@mac1:" + sessA)
	id := r.text("go on")
	r.tg.waitFor(t, "👌", reactedOn(id, "👌"))
	if len(r.ag.submitted(sessA)) != 0 {
		t.Fatal("typed into a menu")
	}
	r.ag.setMenu(sessA, false)
	r.clock.Sleep(agterm.RetryEvery)
	r.svc.Queue.Tick(r.ctx)
	r.tg.waitFor(t, "late ✍", reactedOn(id, "✍"))
	r.svc.Queue.Tick(r.ctx)
	if got := r.ag.submitted(sessA); len(got) != 1 {
		t.Fatalf("typed %q, want once", got)
	}
}

func TestSessionGoneShrugs(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.add(sessA)
	r.text("/target session@mac1:" + sessA)
	r.tg.waitFor(t, "switched", has("send", "Now talking to"))
	_ = r.ag.Close(context.Background(), agterm.Session{ID: sessA}) // closed after the switch: the hub still lists it
	id := r.text("anyone?")
	r.tg.waitFor(t, "🤷", reactedOn(id, "🤷"))
}

func TestSpawnPeekFiles(t *testing.T) {
	r := startAgtermHost(t)
	id := r.text("/spawn mac1:~/work ev-new find me a car")
	cs := r.tg.waitFor(t, "spawned", has("send", "🆕 ev-new started on mac1"))
	if !reactedOn(id, "✍")(cs) {
		t.Errorf("first message should be typed: %+v", cs)
	}
	new := "BEEF0001-0000-0000-0000-000000000000"
	if got := r.ag.submitted(new); len(got) != 1 || !strings.HasSuffix(got[0], "] find me a car") {
		t.Fatalf("first message %q", got)
	}
	r.text("/status")
	r.tg.waitFor(t, "target is the new session", has("send", "target: session@mac1:"+new))
	r.text("/files")
	r.tg.waitFor(t, "listing", has("send", "brief.md"))
	r.text("/files brief.md")
	r.tg.waitFor(t, "brief", has("send", "find me a car"))
	r.text("/files ../../etc/passwd")
	r.tg.waitFor(t, "refused", has("send", "path refused"))
	r.text("/peek 5")
	r.tg.waitFor(t, "peek", has("send", "🖥 session@mac1:"+new+" · ready"))
	r.text("/pause")
	r.tg.waitFor(t, "paused", has("send", "⏸ paused"))
	r.text("/done")
	r.tg.waitFor(t, "done", has("send", "✅ marked done"))
}

func TestParseSessionTarget(t *testing.T) {
	got, err := ParseTarget("session@mac2:4a17709c-1c35")
	if err != nil || got != (Target{Kind: KindSession, Host: "mac2", Dir: "4A17709C-1C35"}) {
		t.Fatalf("%+v %v", got, err)
	}
	if again, _ := ParseTarget(got.String()); again != got {
		t.Errorf("round trip %q", got.String())
	}
	for _, bad := range []string{"session@mac2", "session@mac2:", "session@mac2:../x", "session@mac2:abc", "session@:4A17709C"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestStatusSessionLines(t *testing.T) {
	raw := mustJSONRaw(agterm.Snapshot{Sessions: []agterm.SessionInfo{
		{ID: sessA, Name: "ev-test", Agent: agterm.Claude, State: agterm.Ready},
		{ID: "BBBB2222-0", Name: "shell"},
	}})
	got := sessionLines(raw)
	if got != "   ◦ AAAA1111 ev-test · claude · ready\n" {
		t.Fatalf("lines = %q", got)
	}
}

func TestStatusSessionLinesCapped(t *testing.T) {
	var ss []agterm.SessionInfo
	for i := 0; i < 20; i++ {
		ss = append(ss, agterm.SessionInfo{ID: fmt.Sprintf("%08d", i), Name: strings.Repeat("n", 80), Agent: agterm.Claude, State: agterm.Ready})
	}
	got := sessionLines(mustJSONRaw(agterm.Snapshot{Sessions: ss}))
	if strings.Count(got, "\n") != maxStatusSessions+1 || !strings.HasSuffix(got, "… +5 more\n") || strings.Contains(got, strings.Repeat("n", 41)) {
		t.Fatalf("lines = %q", got)
	}
}
