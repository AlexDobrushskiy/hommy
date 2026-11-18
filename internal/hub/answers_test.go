package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/hook"
	"github.com/AlexDobrushskiy/hommy/internal/host"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

const (
	e2eAgterm = "4A17709C-1111-2222-3333-444455556666"
	e2eBridge = "session_01TestBridge"
	e2eLink   = "https://claude.ai/code/" + e2eBridge
)

// hookRig is a hub + a real host with its hookrelay; hooks run through the
// real client (hook.Run) over the unix socket, as Claude Code would call them.
type hookRig struct {
	*rig
	hr   *hostRig
	hcfg hook.Config
	env  map[string]string
}

func newHookRig(t *testing.T) *hookRig {
	r := newRig(t)
	// unix socket paths are capped at 104 bytes on macOS; t.TempDir() is too deep
	sockDir, err := os.MkdirTemp("/tmp", "hc06")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	hk := &hookRig{rig: r, env: map[string]string{"AGTERM_SESSION_ID": strings.ToLower(e2eAgterm), "CLAUDE_CODE_BRIDGE_SESSION_ID": e2eBridge}}
	hk.hr = hk.startHookHost(filepath.Join(sockDir, "hook.sock"))
	hk.hcfg = hook.Config{Socket: filepath.Join(sockDir, "hook.sock"), Spool: hook.SpoolDir(hk.hr.cfg.DataDir)}
	return hk
}

func (hk *hookRig) startHookHost(socket string) *hostRig {
	cfg := hk.hostConfig()
	cfg.HookSocket = socket
	h, err := host.New(cfg)
	if err != nil {
		hk.t.Fatal(err)
	}
	h.Register(proto.JobPeek, func(_ context.Context, job proto.Job, _ func(proto.Event)) (proto.Result, error) {
		var req agterm.PeekRequest
		json.Unmarshal(job.Payload, &req)
		data, _ := json.Marshal(agterm.PeekResult{Status: proto.StatusOK, Session: req.Session, Text: "❯ 1. Yes\n  2. No",
			Composer: agterm.Composer{State: agterm.Menu}})
		return proto.Result{Status: proto.StatusOK, Data: data}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := h.StartHookRelay(ctx); err != nil {
		hk.t.Fatal(err)
	}
	hr := &hostRig{h: h, stop: cancel, done: make(chan struct{}), cfg: cfg}
	go func() { h.Run(ctx); close(hr.done) }()
	hk.waitConnected(true)
	hk.t.Cleanup(hr.shutdown)
	return hr
}

// hook runs `hommy hook <event>` with this input, as Claude Code does.
func (hk *hookRig) hook(event string, in map[string]any) {
	hk.t.Helper()
	hk.hookWant(event, in, "sent")
}

func (hk *hookRig) hookWant(event string, in map[string]any, want string) {
	hk.t.Helper()
	b, _ := json.Marshal(in)
	what, err := hook.Run(context.Background(), hk.hcfg, hook.AgentClaude, event, strings.NewReader(string(b)),
		func(k string) string { return hk.env[k] }, time.Now)
	if err != nil {
		hk.t.Fatalf("hook %s: %v", event, err)
	}
	if what != want {
		hk.t.Fatalf("hook %s: %s, want %s", event, what, want)
	}
}

// turn is a Telegram-driven turn: the owner's message, the line's prompt, the Stop.
func (hk *hookRig) turn(answer string) int {
	id := hk.text("ignored: the hook test drives the session itself") // a real message id to reply to
	hk.hook(hook.EventUserPromptSubmit, map[string]any{"session_id": "s1", "prompt_id": "p" + itoa(id),
		"prompt": "[Telegram · tg-" + itoa(id) + "] reply"})
	hk.hook(hook.EventStop, map[string]any{"session_id": "s1", "prompt_id": "p" + itoa(id), "last_assistant_message": answer})
	return id
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func hookAnswers(cs []call) []call {
	var out []call
	for _, c := range cs {
		if c.Kind == "send" && (strings.HasPrefix(c.Text, "🚗 ") || strings.HasPrefix(c.Text, "⏸ ")) {
			out = append(out, c)
		}
	}
	return out
}

func TestHookAnswerRepliesTaggedWithLink(t *testing.T) {
	hk := newHookRig(t)
	hk.hook(hook.EventSessionStart, map[string]any{"session_id": "s1", "cwd": "/w", "source": "startup"})
	// Typed at the Mac: nothing reaches the chat.
	hk.hook(hook.EventUserPromptSubmit, map[string]any{"session_id": "s1", "prompt": "a local prompt"})
	hk.hookWant(hook.EventStop, map[string]any{"session_id": "s1", "last_assistant_message": "local answer"}, "local turn")
	id := hk.turn("pong")
	cs := hk.tg.waitFor(t, "answer", func(cs []call) bool { return len(hookAnswers(cs)) == 1 })
	a := hookAnswers(cs)[0]
	want := "🚗 session 4A17709C · mac1\npong\n\n🔗 " + e2eLink
	if a.Text != want || a.ReplyTo != id || !a.NoPreview || a.KB == nil {
		t.Fatalf("answer = %+v, want %q replying to %d with the keyboard", a, want, id)
	}
	if len(a.Entities) != 1 || a.Entities[0].Type != models.MessageEntityTypeTextLink || a.Entities[0].URL != e2eLink ||
		a.Entities[0].Offset != u16len("🚗 ") || a.Entities[0].Length != u16len("session 4A17709C · mac1") {
		t.Fatalf("entities = %+v", a.Entities)
	}
	for _, c := range cs {
		if strings.Contains(c.Text, "local answer") {
			t.Fatalf("a local turn reached the chat: %+v", c)
		}
	}
	// The answer is stored as written by the session (replies route back, 07).
	jobDone(t, hk.st, proto.EventHookAnswer) // recorded right after the send
	m, err := hk.st.GetMessage(ownerUser, a.MsgID)
	if err != nil || m == nil || m.Target != "session@mac1:"+e2eAgterm || m.Direction != "out" {
		t.Fatalf("stored message = %+v, %v", m, err)
	}
}

// jobDone waits (bounded) until every job of a type is delivered.
func jobDone(t *testing.T, st *Store, typ string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var open, all int
		st.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(status != ?), 0) FROM jobs WHERE type=?`, JobDone, typ).Scan(&all, &open)
		if all > 0 && open == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s jobs not delivered (%d of %d open)", typ, open, all)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestHookLongAnswerChunkedAllTagged(t *testing.T) {
	hk := newHookRig(t)
	var b strings.Builder
	for i := 0; b.Len() < 9500; i++ {
		b.WriteString("line " + itoa(i) + ": lorem ipsum dolor sit amet consectetur adipiscing elit\n")
	}
	id := hk.turn(b.String())
	cs := hk.tg.waitFor(t, "chunks", func(cs []call) bool { return len(hookAnswers(cs)) >= 3 })
	chunks := hookAnswers(cs)
	for i, c := range chunks {
		if !strings.HasPrefix(c.Text, "🚗 session 4A17709C · mac1\n") || u16len(c.Text) > 4096 {
			t.Fatalf("chunk %d untagged or too long (%d): %.60q", i, u16len(c.Text), c.Text)
		}
		if (i == 0) != (c.ReplyTo == id) {
			t.Fatalf("chunk %d replies to %d", i, c.ReplyTo)
		}
		if (i == len(chunks)-1) != strings.HasSuffix(c.Text, "🔗 "+e2eLink) {
			t.Fatalf("chunk %d link placement: %.80q", i, c.Text[len(c.Text)-80:])
		}
	}
}

func TestHookWaitingPingOncePerTurnWithOpenButton(t *testing.T) {
	hk := newHookRig(t)
	id := hk.text("run touch x")
	hk.hook(hook.EventUserPromptSubmit, map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-" + itoa(id) + "] run touch x"})
	for i := 0; i < 2; i++ {
		hk.hook(hook.EventNotify, map[string]any{"session_id": "s1", "notification_type": "permission_prompt", "message": "Claude needs your permission"})
	}
	hk.hook(hook.EventStop, map[string]any{"session_id": "s1", "last_assistant_message": "created x"})
	cs := hk.tg.waitFor(t, "ping + answer", func(cs []call) bool { return len(hookAnswers(cs)) == 2 })
	got := hookAnswers(cs)
	p := got[0]
	if !strings.HasPrefix(p.Text, "⏸ session 4A17709C · mac1 is waiting at the Mac\nClaude needs your permission\n🔗 "+e2eLink) || p.ReplyTo != id {
		t.Fatalf("ping = %+v", p)
	}
	if p.Inline == nil || len(p.Inline.InlineKeyboard) != 1 || p.Inline.InlineKeyboard[0][0].Text != "Open" ||
		p.Inline.InlineKeyboard[0][0].WebApp == nil ||
		p.Inline.InlineKeyboard[0][0].WebApp.URL != "https://agents.test/console?target=session%40mac1%3A"+e2eAgterm {
		t.Fatalf("ping keyboard = %+v", p.Inline)
	}
	if !strings.Contains(got[1].Text, "created x") {
		t.Fatalf("answer = %+v", got[1])
	}
	if n := len(hookAnswers(hk.tg.snapshot())); n != 2 {
		t.Fatalf("%d hook messages, want 2", n)
	}
}

func TestHookAnswerWaitsOutAHubOutageAndArrivesOnce(t *testing.T) {
	hk := newHookRig(t)
	id := hk.text("long job")
	hk.stopHub()
	hk.waitConnected(false)
	hk.hook(hook.EventUserPromptSubmit, map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-" + itoa(id) + "] long job"})
	hk.hook(hook.EventStop, map[string]any{"session_id": "s1", "last_assistant_message": "finished while the hub was down"})
	hk.startHub()
	cs := hk.tg.waitFor(t, "answer after the outage", func(cs []call) bool { return len(hookAnswers(cs)) == 1 })
	if a := hookAnswers(cs)[0]; !strings.Contains(a.Text, "finished while the hub was down") || a.ReplyTo != id {
		t.Fatalf("answer = %+v", a)
	}
	// The host re-sends until acked and the hub may see it again: still one message.
	var a proto.HookAnswer
	var ev proto.Event
	row := hk.st.db.QueryRow(`SELECT payload FROM jobs WHERE type=?`, proto.EventHookAnswer)
	var payload []byte
	if err := row.Scan(&payload); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(payload, &a)
	ev = proto.Event{JobID: "hook:" + a.ID, Kind: proto.EventHookAnswer, Data: payload}
	if err := hk.hub.handleHookEvent("mac1", ev); err != nil {
		t.Fatal(err)
	}
	jobDone(t, hk.st, proto.EventHookAnswer)
	if n := len(hookAnswers(hk.tg.snapshot())); n != 1 {
		t.Fatalf("%d answers, want 1", n)
	}
}

func TestHookLinksInStatusPickerAndConsole(t *testing.T) {
	hk := newHookRig(t)
	hk.hook(hook.EventUserPromptSubmit, map[string]any{"session_id": "s1", "prompt": "local"})
	// The host's own (first and, with HeartbeatEvery 1h, only) heartbeat must not overwrite the one set below.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		if row, _ := hk.st.Host("mac1"); row.Heartbeat.Host == "mac1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat from the host")
		}
	}
	snap, _ := json.Marshal(agterm.Snapshot{Sessions: []agterm.SessionInfo{{ID: e2eAgterm, Name: "e2e-claude", Agent: "claude", State: "ready"}}})
	hb := proto.Heartbeat{Host: "mac1", Version: "t", Agterm: snap, Hooks: []proto.HookSession{{AgtermID: e2eAgterm, Agent: "claude", BridgeID: e2eBridge}}}
	if err := hk.st.HostSeen("mac1", &hb, time.Now()); err != nil {
		t.Fatal(err)
	}
	text, ents := hk.hub.statusRich(ownerUser)
	var linked bool
	for _, e := range ents {
		if e.URL == e2eLink && strings.Contains(string([]rune(text)), "e2e-claude") {
			linked = true
		}
	}
	if !linked {
		t.Fatalf("/status has no link for the session: %q %+v", text, ents)
	}
	resp := hk.hub.pickerAPI(ownerUser).(pickerResp)
	if s := resp.Hosts[0].Sessions; len(s) != 1 || s[0].Link != e2eLink {
		t.Fatalf("picker sessions = %+v", s)
	}
	req := httptest.NewRequest("GET", "/api/console?target=session@mac1:"+e2eAgterm, nil)
	status, v := hk.hub.consoleAPI(ownerUser, req)
	c, _ := v.(consoleResp)
	if status != 200 || c.Link != e2eLink || c.Label != "e2e-claude · mac1" || c.State != "menu" || !strings.Contains(c.Text, "1. Yes") {
		t.Fatalf("console = %d %+v", status, v)
	}
	if status, _ := hk.hub.consoleAPI(ownerUser, httptest.NewRequest("GET", "/api/console?target=vault@mac1", nil)); status != 400 {
		t.Fatalf("console for a vault target = %d, want 400", status)
	}
	// The peek asked by the console posts nothing to the chat.
	for _, c := range hk.tg.snapshot() {
		if strings.Contains(c.Text, "1. Yes") {
			t.Fatalf("console peek reached the chat: %+v", c)
		}
	}
	// A switch to the session carries its link.
	if err := hk.hub.switchTarget(context.Background(), ownerUser, Target{Kind: KindSession, Host: "mac1", Dir: e2eAgterm}, ""); err != nil {
		t.Fatal(err)
	}
	hk.tg.waitFor(t, "switch notice with link", has("send", "🎯 Now talking to e2e-claude · mac1\n🔗 "+e2eLink))
}

func TestRunPTextDoesNotWaitForVoice(t *testing.T) {
	r := newRig(t)
	r.startHost()
	r.sp.mu.Lock()
	r.sp.gate = make(chan struct{})
	r.sp.mu.Unlock()
	r.text("LONG please")
	cs := r.tg.waitFor(t, "text answer", func(cs []call) bool { return len(r.answers(cs)) == 1 })
	if count(cs, "voice", "") != 0 {
		t.Fatal("voice before its synthesis finished")
	}
	close(r.sp.gate)
	cs = r.tg.waitFor(t, "voice", has("voice", "OggSspoken version"))
	ti, vi := -1, -1
	for i, c := range cs {
		if c.Kind == "send" && strings.HasPrefix(c.Text, "answer to [LONG please]") {
			ti = i
		}
		if c.Kind == "voice" {
			vi = i
		}
	}
	if ti < 0 || vi < ti {
		t.Fatalf("text at %d, voice at %d", ti, vi)
	}
}

func waitingPings(cs []call) []call {
	var out []call
	for _, c := range cs {
		if c.Kind == "send" && (strings.HasPrefix(c.Text, "⏸ ") || strings.HasPrefix(c.Text, "▶️ ")) {
			out = append(out, c)
		}
	}
	return out
}

func (hk *hookRig) boardSession(t *testing.T) boardSession {
	t.Helper()
	// the real host's own heartbeats carry no agterm data: rate the session directly
	hooks, err := hk.st.RecentHookReports(hk.clk.Now().Add(-boardWaitingWindow))
	if err != nil {
		t.Fatal(err)
	}
	return hk.hub.boardSession("mac1", agtermSession{ID: e2eAgterm, Name: "s1", Agent: "claude", State: "ready"},
		nil, nil, nil, hooks)
}

// A declined prompt or an interrupted turn has no Stop: the ⏸ still ends once
// the session leaves the prompt (here: a prompt typed at the Mac). The board
// stops showing it and the ⏸ message says so, keeping its Open button.
func TestHookWaitingEndsOnTheBoardAndInThePing(t *testing.T) {
	hk := newHookRig(t)
	id := hk.text("run touch x")
	hk.hook(hook.EventUserPromptSubmit, map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-" + itoa(id) + "] run touch x"})
	hk.hook(hook.EventNotify, map[string]any{"session_id": "s1", "notification_type": "permission_prompt", "message": "Claude needs your permission"})
	cs := hk.tg.waitFor(t, "⏸", func(cs []call) bool { return len(waitingPings(cs)) == 1 })
	p := waitingPings(cs)[0]
	jobDone(t, hk.st, proto.EventHookWaiting)
	if s := hk.boardSession(t); !strings.Contains(strings.Join(s.Reasons, "|"), "waiting at the Mac") {
		t.Fatalf("board before = %+v", s)
	}

	hk.hook(hook.EventUserPromptSubmit, map[string]any{"session_id": "s1", "prompt": "never mind, I did it"}) // the owner at the Mac
	cs = hk.tg.waitFor(t, "⏸ edited", func(cs []call) bool {
		for _, c := range cs {
			if c.Kind == "buttons" && c.MsgID == p.MsgID {
				return true
			}
		}
		return false
	})
	var edits []call
	for _, c := range cs {
		if c.Kind == "edit" && c.MsgID == p.MsgID {
			edits = append(edits, c)
		}
	}
	if len(edits) != 1 || !strings.HasPrefix(edits[0].Text, "▶️ ") ||
		!strings.Contains(edits[0].Text, " · mac1 is no longer waiting at the Mac\nClaude needs your permission\n🔗 "+e2eLink) {
		t.Fatalf("edits = %+v", edits)
	}
	jobDone(t, hk.st, proto.EventHookResumed)
	if s := hk.boardSession(t); s.Waiting || len(s.Reasons) != 0 || s.Group != "idle" {
		t.Fatalf("board after = %+v", s)
	}
	if n := len(waitingPings(hk.tg.snapshot())); n != 1 {
		t.Fatalf("%d ⏸ messages, want 1", n)
	}
}

// The edit that ends a ⏸ drops its Open button: a transient failure putting it
// back is retried (the text is edited once), not given up.
func TestHookResumedRetriesTheOpenButton(t *testing.T) {
	hk := newHookRig(t)
	id := hk.text("run touch x")
	hk.hook(hook.EventUserPromptSubmit, map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-" + itoa(id) + "] run touch x"})
	hk.hook(hook.EventNotify, map[string]any{"session_id": "s1", "notification_type": "permission_prompt", "message": "Claude needs your permission"})
	cs := hk.tg.waitFor(t, "⏸", func(cs []call) bool { return len(waitingPings(cs)) == 1 })
	p := waitingPings(cs)[0]
	jobDone(t, hk.st, proto.EventHookWaiting)

	hk.tg.setFailButtons(errors.New("telegram: 502"))
	hk.hook(hook.EventUserPromptSubmit, map[string]any{"session_id": "s1", "prompt": "never mind, I did it"})
	hk.tg.waitFor(t, "⏸ edited", func(cs []call) bool { return count(cs, "edit", "no longer waiting") == 1 })
	hk.tg.setFailButtons(nil)
	hk.tg.waitFor(t, "Open put back", func(cs []call) bool {
		for _, c := range cs {
			if c.Kind == "buttons" && c.MsgID == p.MsgID {
				return true
			}
		}
		return false
	})
	jobDone(t, hk.st, proto.EventHookResumed)
	if n := count(hk.tg.snapshot(), "edit", "no longer waiting"); n != 1 {
		t.Fatalf("⏸ edited %d times, want 1", n)
	}
}

// Both reports after a hub outage: one ⏸ message, which ends up "no longer
// waiting" (sent that way, or edited once when its end came in just after).
func TestHookWaitingEndedDuringAnOutageIsSentEnded(t *testing.T) {
	hk := newHookRig(t)
	id := hk.text("run touch x")
	hk.stopHub()
	hk.waitConnected(false)
	hk.hook(hook.EventUserPromptSubmit, map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-" + itoa(id) + "] run touch x"})
	hk.hook(hook.EventNotify, map[string]any{"session_id": "s1", "notification_type": "permission_prompt", "message": "Claude needs your permission"})
	hk.hook(hook.EventUserPromptSubmit, map[string]any{"session_id": "s1", "prompt": "local"})
	hk.startHub()
	jobDone(t, hk.st, proto.EventHookResumed)
	jobDone(t, hk.st, proto.EventHookWaiting)
	ps := waitingPings(hk.tg.snapshot())
	if len(ps) != 1 || ps[0].ReplyTo != id {
		t.Fatalf("pings = %+v", ps)
	}
	final, edits := ps[0].Text, 0
	for _, c := range hk.tg.snapshot() {
		if c.Kind == "edit" && c.MsgID == ps[0].MsgID {
			final, edits = c.Text, edits+1
		}
	}
	if !strings.HasPrefix(final, "▶️ ") || edits > 1 || (edits == 1) != strings.HasPrefix(ps[0].Text, "⏸ ") {
		t.Fatalf("sent %q, %d edits, final %q", ps[0].Text, edits, final)
	}
	if s := hk.boardSession(t); s.Waiting {
		t.Fatalf("board = %+v", s)
	}
}
