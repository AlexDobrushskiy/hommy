package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AlexDobrushskiy/hommy/internal/sanitize"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

type emitted struct {
	Kind string
	A    proto.HookAnswer
}

type relayRig struct {
	t    *testing.T
	r    *Relay
	mu   sync.Mutex
	out  []emitted
	fail error
	now  time.Time
	dir  string
}

func newRelayRig(t *testing.T) *relayRig {
	rr := &relayRig{t: t, dir: t.TempDir(), now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	rr.open()
	return rr
}

func (rr *relayRig) open() {
	r, err := NewRelay(filepath.Join(rr.dir, "state.json"), filepath.Join(rr.dir, "spool"), "mac2",
		func() time.Time { return rr.now },
		func(kind string, a proto.HookAnswer) error {
			rr.mu.Lock()
			defer rr.mu.Unlock()
			if rr.fail != nil {
				return rr.fail
			}
			rr.out = append(rr.out, emitted{kind, a})
			return nil
		})
	if err != nil {
		rr.t.Fatal(err)
	}
	rr.r = r
}

func (rr *relayRig) hook(agent, event, agtermID, bridge string, in map[string]any) error {
	rr.t.Helper()
	b, _ := json.Marshal(in)
	return rr.r.Handle(Request{Event: event, Agent: agent, Input: b, AgtermID: agtermID, BridgeID: bridge, At: rr.now})
}

func (rr *relayRig) must(agent, event, agtermID, bridge string, in map[string]any) {
	rr.t.Helper()
	if err := rr.hook(agent, event, agtermID, bridge, in); err != nil {
		rr.t.Fatal(err)
	}
}

func (rr *relayRig) emitted() []emitted {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return append([]emitted(nil), rr.out...)
}

func TestTelegramMsgID(t *testing.T) {
	for in, want := range map[string]int64{
		"[Telegram · tg-4242] run it":                     4242,
		`[Telegram · tg-17 · re "🤖 x: deposit?"] drop it`: 17,
		"  [Telegram · tg-9]":                             9,
		"run it":                                          0,
		"note [Telegram · tg-5] inside":                   0,
		"[Telegram · tg-12x] no":                          0,
		// the hub's ids for a pressed button and a reaction (internal/hub/agents.go): turns from Telegram too
		sanitize.PrefixID("tg-38-b5", "🤖 booking: 10:30?") + "Book": 38,
		sanitize.PrefixID("tg-41-r7", "") + "👍 yes / continue":      41,
		"[Telegram · tg-38-bx] no":                                  0,
	} {
		if got := TelegramMsgID(in); got != want {
			t.Errorf("TelegramMsgID(%q) = %d, want %d", in, got, want)
		}
	}
}

const agID = "4a17709c-1111-2222-3333-444455556666"

func TestRelayTelegramTurnAnswersOnceAndLocalTurnsStaySilent(t *testing.T) {
	rr := newRelayRig(t)
	sid := map[string]any{"session_id": "s1", "cwd": "/w"}
	with := func(kv ...any) map[string]any {
		m := map[string]any{"session_id": "s1", "cwd": "/w"}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	rr.must(AgentClaude, EventSessionStart, agID, "", sid)
	// A prompt typed at the Mac: its Stop sends nothing.
	rr.must(AgentClaude, EventUserPromptSubmit, agID, "session_b1", with("prompt", "local question", "prompt_id", "p0"))
	rr.must(AgentClaude, EventStop, agID, "session_b1", with("last_assistant_message", "local answer"))
	if got := rr.emitted(); len(got) != 0 {
		t.Fatalf("local turn emitted %+v", got)
	}
	// A Telegram turn: one ⏸ (idle notices ignored), then the answer.
	rr.must(AgentClaude, EventUserPromptSubmit, agID, "session_b1", with("prompt", "[Telegram · tg-77] reply pong", "prompt_id", "p1"))
	rr.must(AgentClaude, EventNotify, agID, "session_b1", with("notification_type", "idle_prompt", "message", "Claude is waiting for your input"))
	rr.must(AgentClaude, EventNotify, agID, "session_b1", with("notification_type", "permission_prompt", "message", "Claude needs your permission"))
	rr.must(AgentClaude, EventNotify, agID, "session_b1", with("notification_type", "permission_prompt", "message", "Claude needs your permission"))
	rr.must(AgentClaude, EventStop, agID, "session_b1", with("last_assistant_message", "pong"))
	rr.must(AgentClaude, EventStop, agID, "session_b1", with("last_assistant_message", "pong again")) // no turn any more
	got := rr.emitted()
	if len(got) != 3 || got[0].Kind != proto.EventHookWaiting || got[1].Kind != proto.EventHookResumed || got[2].Kind != proto.EventHookAnswer {
		t.Fatalf("emitted %+v", got)
	}
	w, a := got[0].A, got[2].A
	if got[1].A.Of != w.ID || got[1].A.ID != w.ID+proto.ResumedSuffix {
		t.Fatalf("resumed = %+v (waiting %s)", got[1].A, w.ID)
	}
	if w.Text != "Claude needs your permission" || w.TgMsgID != 77 {
		t.Fatalf("waiting = %+v", w)
	}
	if a.Text != "pong" || a.TgMsgID != 77 || a.AgtermID != strings.ToUpper(agID) || a.BridgeID != "session_b1" ||
		a.SessionID != "s1" || a.Agent != AgentClaude || a.Cwd != "/w" || a.ID == "" || a.ID == w.ID {
		t.Fatalf("answer = %+v", a)
	}
	sess := rr.r.Sessions()
	if len(sess) != 1 || sess[0].BridgeID != "session_b1" || sess[0].AgtermID != strings.ToUpper(agID) {
		t.Fatalf("sessions = %+v", sess)
	}
}

func TestRelayFailedEmitIsRetriedWithTheSameID(t *testing.T) {
	rr := newRelayRig(t)
	in := map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-5] hi", "prompt_id": "p"}
	rr.must(AgentClaude, EventUserPromptSubmit, agID, "", in)
	rr.fail = errors.New("disk full")
	stop := map[string]any{"session_id": "s1", "last_assistant_message": "hello"}
	if err := rr.hook(AgentClaude, EventStop, agID, "", stop); err == nil {
		t.Fatal("Stop with a failing outbox: want an error (the hook spools it)")
	}
	rr.fail = nil
	rr.must(AgentClaude, EventStop, agID, "", stop)
	// A reopened relay (host restart) keeps no turn: a replayed Stop is a no-op.
	rr.open()
	rr.must(AgentClaude, EventStop, agID, "", stop)
	if got := rr.emitted(); len(got) != 1 || got[0].A.Text != "hello" {
		t.Fatalf("emitted %+v", got)
	}
}

func TestRelayTurnSurvivesRestartAndExpires(t *testing.T) {
	rr := newRelayRig(t)
	rr.must(AgentClaude, EventUserPromptSubmit, agID, "", map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-5] hi"})
	rr.open() // host restarted mid-turn
	rr.must(AgentClaude, EventStop, agID, "", map[string]any{"session_id": "s1", "last_assistant_message": "done"})
	rr.must(AgentClaude, EventUserPromptSubmit, agID, "", map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-6] again"})
	rr.now = rr.now.Add(25 * time.Hour) // killed mid-turn; a much later Stop isn't its answer
	rr.must(AgentClaude, EventStop, agID, "", map[string]any{"session_id": "s1", "last_assistant_message": "late"})
	if got := rr.emitted(); len(got) != 1 || got[0].A.Text != "done" || got[0].A.TgMsgID != 5 {
		t.Fatalf("emitted %+v", got)
	}
}

func TestRelayTypedSessionBeatsHookEnv(t *testing.T) {
	// The hooks carry a stale AGTERM_SESSION_ID; the host knows where it typed tg-31.
	rr := newRelayRig(t)
	rr.r.NoteTyped(31, "c0dec0de-aaaa")
	rr.must(AgentClaude, EventUserPromptSubmit, "STALE-ID", "", map[string]any{"session_id": "t1", "prompt_id": "u1", "prompt": "[Telegram · tg-31] hi"})
	rr.must(AgentClaude, EventNotify, "STALE-ID", "", map[string]any{"session_id": "t1", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "touch x"}})
	rr.must(AgentClaude, EventStop, "STALE-ID", "", map[string]any{"session_id": "t1", "prompt_id": "u1", "last_assistant_message": "ok"})
	// The prompt hook ran before the host noted the line (it types first).
	rr.must(AgentClaude, EventUserPromptSubmit, "STALE-ID", "", map[string]any{"session_id": "t1", "prompt": "[Telegram · tg-32] more"})
	rr.r.NoteTyped(32, "c0dec0de-aaaa")
	rr.must(AgentClaude, EventStop, "STALE-ID", "", map[string]any{"session_id": "t1", "last_assistant_message": "ok2"})
	got := rr.emitted()
	if len(got) != 4 {
		t.Fatalf("emitted %+v", got)
	}
	if got[0].A.Text != "wants to run: touch x" {
		t.Fatalf("permission text = %q", got[0].A.Text)
	}
	for _, e := range got {
		if e.A.AgtermID != "C0DEC0DE-AAAA" || e.A.Agent != AgentClaude {
			t.Fatalf("emitted %+v, want agterm C0DEC0DE-AAAA", e.A)
		}
	}
	if s := rr.r.Sessions(); len(s) != 0 { // no bridge id: nothing for the hub's links
		t.Fatalf("sessions = %+v", s)
	}
}

func TestRelayReadsTranscriptWithoutLastAssistantMessage(t *testing.T) {
	rr := newRelayRig(t)
	tr := filepath.Join(rr.dir, "t.jsonl")
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"[Telegram · tg-8] two steps"}}`,
		`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"first I look"}]}}`,
		`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"x","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]}}`,
		`{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"final part one"}]}}`,
		`{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"final part two"}]}}`,
	}
	os.WriteFile(tr, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	rr.must(AgentClaude, EventUserPromptSubmit, agID, "", map[string]any{"session_id": "s1", "transcript_path": tr, "prompt": "[Telegram · tg-8] two steps"})
	rr.must(AgentClaude, EventStop, agID, "", map[string]any{"session_id": "s1", "transcript_path": tr})
	if got := rr.emitted(); len(got) != 1 || got[0].A.Text != "final part one\n\nfinal part two" {
		t.Fatalf("emitted %+v", got)
	}
}

func TestRunSpoolsWhileTheHostIsDownAndTheRelayDrainsInOrder(t *testing.T) {
	rr := newRelayRig(t)
	cfg := Config{Socket: filepath.Join(rr.dir, "nobody.sock"), Spool: filepath.Join(rr.dir, "spool")}
	env := func(k string) string {
		return map[string]string{"AGTERM_SESSION_ID": agID, "CLAUDE_CODE_BRIDGE_SESSION_ID": "session_q"}[k]
	}
	at := rr.now
	clock := func() time.Time { at = at.Add(time.Millisecond); return at }
	for _, c := range []struct{ ev, in string }{
		{EventUserPromptSubmit, `{"session_id":"s9","prompt":"[Telegram · tg-90] q"}`},
		{EventStop, `{"session_id":"s9","last_assistant_message":"a"}`},
	} {
		what, err := Run(context.Background(), cfg, AgentClaude, c.ev, strings.NewReader(c.in), env, clock)
		if err != nil || !strings.HasPrefix(what, "spooled") {
			t.Fatalf("Run(%s) = %q, %v", c.ev, what, err)
		}
	}
	if _, err := Run(context.Background(), cfg, AgentClaude, EventStop, strings.NewReader("not json"), env, clock); err == nil {
		t.Fatal("garbage stdin: want an error (logged), nothing spooled")
	}
	rr.r.DrainSpool()
	got := rr.emitted()
	if len(got) != 1 || got[0].A.Text != "a" || got[0].A.TgMsgID != 90 || got[0].A.BridgeID != "session_q" {
		t.Fatalf("emitted %+v", got)
	}
	if left, _ := filepath.Glob(filepath.Join(cfg.Spool, "*")); len(left) != 0 {
		t.Fatalf("spool not drained: %v", left)
	}
}

func TestClaudeLink(t *testing.T) {
	if got := proto.ClaudeLink("session_017SbYQmw6rbp1JtSDhrsYNA"); got != "https://claude.ai/code/session_017SbYQmw6rbp1JtSDhrsYNA" {
		t.Fatal(got)
	}
	for _, bad := range []string{"", "a/b", "x y", "session_<script>"} {
		if got := proto.ClaudeLink(bad); got != "" {
			t.Fatalf("ClaudeLink(%q) = %q", bad, got)
		}
	}
}

func (rr *relayRig) req(event, agtermID string, at time.Time, in map[string]any) Request {
	b, _ := json.Marshal(in)
	return Request{Event: event, Agent: AgentClaude, Input: b, AgtermID: agtermID, At: at}
}

func (rr *relayRig) spoolReq(q Request) {
	rr.t.Helper()
	b, _ := json.Marshal(q)
	if err := spool(rr.r.spoolDir, b, q.At); err != nil {
		rr.t.Fatal(err)
	}
}

// The host drains the spool from its ticker and from every live request at
// once: each spooled request is handled once, so each answer goes out once.
func TestRelayConcurrentDrainsHandleEachSpooledRequestOnce(t *testing.T) {
	rr := newRelayRig(t)
	const turns = 40
	at := rr.now
	for i := 1; i <= turns; i++ {
		sid := fmt.Sprintf("s%d", i)
		at = at.Add(time.Millisecond)
		rr.spoolReq(rr.req(EventUserPromptSubmit, agID, at, map[string]any{"session_id": sid, "prompt": fmt.Sprintf("[Telegram · tg-%d] q", i)}))
		at = at.Add(time.Millisecond)
		rr.spoolReq(rr.req(EventStop, agID, at, map[string]any{"session_id": sid, "last_assistant_message": "a"}))
	}
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() { defer wg.Done(); rr.r.DrainSpool() }()
	}
	wg.Wait()
	ids := map[string]int{}
	for _, e := range rr.emitted() {
		ids[e.A.ID]++
	}
	if n := len(rr.emitted()); n != turns || len(ids) != turns {
		t.Fatalf("%d answers (%d distinct) for %d turns", n, len(ids), turns)
	}
}

// A request handled twice (the hook timed out and spooled it, the host had
// handled it anyway; a restart before the spool file was removed) yields the
// same answer id, so the hub keeps one.
func TestRelayReplayedTurnKeepsItsAnswerID(t *testing.T) {
	ids := []string{}
	for _, handledAt := range []time.Duration{time.Second, time.Hour} {
		rr := newRelayRig(t)
		at := rr.now
		rr.now = at.Add(handledAt)
		rr.must2(rr.req(EventUserPromptSubmit, agID, at, map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-5] q"}))
		rr.must2(rr.req(EventStop, agID, at.Add(time.Second), map[string]any{"session_id": "s1", "last_assistant_message": "a"}))
		ids = append(ids, rr.emitted()[0].A.ID)
	}
	if ids[0] != ids[1] {
		t.Fatalf("same turn handled at different times got ids %v", ids)
	}
}

func (rr *relayRig) must2(q Request) {
	rr.t.Helper()
	if err := rr.r.Handle(q); err != nil {
		rr.t.Fatal(err)
	}
}

// A live request never overtakes an older spooled one that could not be
// handled yet: turn A's late Stop must not answer turn B.
func TestRelayLiveRequestWaitsBehindAFailedSpooledOne(t *testing.T) {
	rr := newRelayRig(t)
	at := rr.now
	rr.must2(rr.req(EventUserPromptSubmit, agID, at, map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-1] A"}))
	rr.spoolReq(rr.req(EventStop, agID, at.Add(time.Second), map[string]any{"session_id": "s1", "last_assistant_message": "answer A"}))
	rr.fail = errors.New("outbox: disk full")
	if err := rr.r.Ingest(rr.req(EventUserPromptSubmit, agID, at.Add(2*time.Second), map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-2] B"})); err != nil {
		t.Logf("ingest B: %v", err)
	}
	rr.fail = nil
	rr.r.DrainSpool()
	rr.must2(rr.req(EventStop, agID, at.Add(3*time.Second), map[string]any{"session_id": "s1", "last_assistant_message": "answer B"}))
	rr.r.DrainSpool()
	got := map[int64]string{}
	for _, e := range rr.emitted() {
		got[e.A.TgMsgID] = e.A.Text
	}
	if got[1] != "answer A" || got[2] != "answer B" || len(rr.emitted()) != 2 {
		t.Fatalf("answers by tg id = %v (%d emitted)", got, len(rr.emitted()))
	}
}

// A Telegram prompt is acked only once its turn is on disk: a failed state
// write is an error (the request is kept), so a restart can't lose the turn.
func TestRelayStateSaveFailureIsNotAcked(t *testing.T) {
	rr := newRelayRig(t)
	os.Mkdir(rr.r.statePath+".tmp", 0o700) // writeSync of the state file fails
	q := rr.req(EventUserPromptSubmit, agID, rr.now, map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-5] q"})
	if err := rr.r.Handle(q); err == nil {
		t.Fatal("prompt acked although its turn was not saved")
	}
	os.Remove(rr.r.statePath + ".tmp")
	rr.open() // host restart; the hook's request was kept and comes again
	rr.must2(q)
	rr.must2(rr.req(EventStop, agID, rr.now, map[string]any{"session_id": "s1", "last_assistant_message": "a"}))
	if got := rr.emitted(); len(got) != 1 || got[0].A.TgMsgID != 5 {
		t.Fatalf("emitted %+v", got)
	}
}

// Another Stop hook can continue the session with stop_hook_active. The first
// relayed Stop still ends the Telegram turn; later Stops stay local.
func TestRelayFirstStopEndsTheTelegramTurn(t *testing.T) {
	rr := newRelayRig(t)
	rr.must(AgentClaude, EventUserPromptSubmit, agID, "", map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-5] q"})
	rr.must(AgentClaude, EventStop, agID, "", map[string]any{"session_id": "s1", "stop_hook_active": false, "last_assistant_message": "interim"})
	rr.must(AgentClaude, EventStop, agID, "", map[string]any{"session_id": "s1", "stop_hook_active": true, "last_assistant_message": "Commit + push these files too? (yes / no)"})
	rr.must(AgentClaude, EventStop, agID, "", map[string]any{"session_id": "s1", "stop_hook_active": false, "last_assistant_message": "background job done"})
	got := rr.emitted()
	if len(got) != 1 || got[0].A.Text != "interim" || got[0].A.TgMsgID != 5 {
		t.Fatalf("emitted %+v", got)
	}
}

// A new session in the same terminal without a claude.ai bridge must not keep
// advertising the previous session's link.
func TestRelaySessionsNewestPerTerminalWins(t *testing.T) {
	rr := newRelayRig(t)
	rr.must(AgentClaude, EventUserPromptSubmit, agID, "session_old", map[string]any{"session_id": "s1", "prompt": "x"})
	rr.now = rr.now.Add(time.Minute)
	rr.must(AgentClaude, EventSessionStart, agID, "", map[string]any{"session_id": "s2"})
	if s := rr.r.Sessions(); len(s) != 0 {
		t.Fatalf("sessions = %+v, want none (the newest session in that terminal has no link)", s)
	}
}

func (rr *relayRig) sysPrompt(sid string) {
	rr.t.Helper()
	b, _ := json.Marshal(map[string]any{"session_id": sid})
	if err := rr.r.Handle(Request{Event: EventUserPromptSubmit, Agent: AgentClaude, Input: b, AgtermID: agID, SystemPrompt: true, At: rr.now}); err != nil {
		rr.t.Fatal(err)
	}
}

// A background task's completion arrives as a system prompt (Claude Code's
// <task-notification>): only one absorbed before the first Stop stays Telegram's.
func TestRelaySystemPromptKeepsTheTelegramTurn(t *testing.T) {
	rr := newRelayRig(t)
	stop := func(text string) {
		rr.must(AgentClaude, EventStop, agID, "", map[string]any{"session_id": "s1", "last_assistant_message": text})
	}
	// Notification absorbed mid-turn: still one answer for the turn.
	rr.must(AgentClaude, EventUserPromptSubmit, agID, "", map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-5] wait for it"})
	rr.sysPrompt("s1")
	stop("OUTAGE_ONCE")
	// The turn ended, then the task's completion starts a new (system) turn.
	rr.must(AgentClaude, EventUserPromptSubmit, agID, "", map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-6] start it"})
	stop("STARTED")
	rr.sysPrompt("s1")
	stop("DONE")
	// Typed at the Mac: local again.
	rr.must(AgentClaude, EventUserPromptSubmit, agID, "", map[string]any{"session_id": "s1"})
	stop("local")
	rr.sysPrompt("s1") // a system prompt in a local turn stays local
	stop("local too")
	got := rr.emitted()
	var texts []string
	ids := map[string]bool{}
	for _, e := range got {
		texts = append(texts, fmt.Sprintf("%d:%s", e.A.TgMsgID, e.A.Text))
		ids[e.A.ID] = true
	}
	if strings.Join(texts, ",") != "5:OUTAGE_ONCE,6:STARTED" || len(ids) != 2 {
		t.Fatalf("answers = %v (%d ids)", texts, len(ids))
	}
}

// A ⏸ ends once the session is off its prompt: a composer reading (taken past
// the grace) that shows a live composer or no agent, or a prompt going in.
// A dialog or an unreadable screen keeps it.
func TestRelayWaitingEndsWhenTheSessionLeavesThePrompt(t *testing.T) {
	id := strings.ToUpper(agID)
	start := func(rr *relayRig, tg int) {
		rr.must(AgentClaude, EventUserPromptSubmit, agID, "", map[string]any{"session_id": "s1",
			"prompt": fmt.Sprintf("[Telegram · tg-%d] run it", tg), "prompt_id": fmt.Sprint(tg)})
		rr.must(AgentClaude, EventNotify, agID, "", map[string]any{"session_id": "s1", "notification_type": "permission_prompt"})
	}
	kinds := func(rr *relayRig) []string {
		var k []string
		for _, e := range rr.emitted() {
			k = append(k, e.Kind)
		}
		return k
	}
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	for _, c := range []struct {
		name   string
		states map[string]string
		ends   bool
	}{
		{"declined: ready", map[string]string{id: "ready"}, true},
		{"interrupted with a draft", map[string]string{strings.ToLower(id): "draft"}, true},
		{"approved: working", map[string]string{id: "working"}, true},
		{"session closed", map[string]string{"OTHER": "ready"}, true},
		{"still at the menu", map[string]string{id: "menu"}, false},
		{"screen unreadable", map[string]string{id: "unknown"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rr := newRelayRig(t)
			rr.now = t0
			start(rr, 50)
			rr.r.ObserveComposers(t0.Add(ResumeGrace-time.Millisecond), map[string]string{id: "ready"}) // may predate the dialog
			rr.r.ObserveComposers(t0.Add(ResumeGrace), c.states)
			rr.r.ObserveComposers(t0.Add(20*time.Second), c.states) // once
			want := []string{proto.EventHookWaiting}
			if c.ends {
				want = append(want, proto.EventHookResumed)
			}
			if got := kinds(rr); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("emitted %v, want %v", got, want)
			}
			if c.ends {
				got := rr.emitted()
				if r := got[1].A; r.Of != got[0].A.ID || r.ID != got[0].A.ID+proto.ResumedSuffix || r.TgMsgID != 50 || r.AgtermID != id {
					t.Fatalf("resumed %+v", r)
				}
			}
			// survives a restart: no second end
			rr.open()
			rr.r.ObserveComposers(t0.Add(time.Minute), map[string]string{id: "ready"})
			if got := kinds(rr); len(got) != 2 || got[1] != proto.EventHookResumed {
				t.Fatalf("after restart %v", got)
			}
		})
	}

	t.Run("a person types at the Mac", func(t *testing.T) {
		rr := newRelayRig(t)
		start(rr, 51)
		rr.must(AgentClaude, EventUserPromptSubmit, agID, "", map[string]any{"session_id": "s1", "prompt": "local"})
		if got := kinds(rr); fmt.Sprint(got) != fmt.Sprint([]string{proto.EventHookWaiting, proto.EventHookResumed}) {
			t.Fatalf("emitted %v", got)
		}
	})
	t.Run("no ⏸, nothing to end", func(t *testing.T) {
		rr := newRelayRig(t)
		rr.must(AgentClaude, EventUserPromptSubmit, agID, "", map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-52] x"})
		rr.r.ObserveComposers(rr.now.Add(time.Minute), map[string]string{})
		rr.must(AgentClaude, EventStop, agID, "", map[string]any{"session_id": "s1", "last_assistant_message": "done"})
		if got := kinds(rr); fmt.Sprint(got) != fmt.Sprint([]string{proto.EventHookAnswer}) {
			t.Fatalf("emitted %v", got)
		}
	})
}

// A turn that stops at a second prompt after its first ⏸ ended ("no longer
// waiting") reports that prompt too; a repeat while still waiting does not.
func TestRelaySecondPromptAfterAnEndedWaitIsReported(t *testing.T) {
	id := strings.ToUpper(agID)
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	rr := newRelayRig(t)
	rr.now = t0
	notify := func() {
		rr.must(AgentClaude, EventNotify, agID, "", map[string]any{"session_id": "s1", "notification_type": "permission_prompt"})
	}
	rr.must(AgentClaude, EventUserPromptSubmit, agID, "", map[string]any{"session_id": "s1",
		"prompt": "[Telegram · tg-60] run two tools", "prompt_id": "60"})
	notify()
	notify()                                                                     // the same prompt again: one ⏸
	rr.r.ObserveComposers(t0.Add(ResumeGrace), map[string]string{id: "working"}) // approved
	rr.now = t0.Add(30 * time.Second)
	notify() // the next tool asks
	rr.r.ObserveComposers(t0.Add(40*time.Second), map[string]string{id: "menu"})
	rr.r.ObserveComposers(t0.Add(50*time.Second), map[string]string{id: "working"})
	rr.must(AgentClaude, EventStop, agID, "", map[string]any{"session_id": "s1", "last_assistant_message": "done"})

	got := rr.emitted()
	var kinds []string
	for _, e := range got {
		kinds = append(kinds, e.Kind)
	}
	want := []string{proto.EventHookWaiting, proto.EventHookResumed, proto.EventHookWaiting, proto.EventHookResumed, proto.EventHookAnswer}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("emitted %v, want %v", kinds, want)
	}
	w1, r1, w2, r2 := got[0].A, got[1].A, got[2].A, got[3].A
	if w1.ID == w2.ID || r1.Of != w1.ID || r2.Of != w2.ID || r2.ID != w2.ID+proto.ResumedSuffix {
		t.Fatalf("ids: w1 %s r1 %s/%s w2 %s r2 %s/%s", w1.ID, r1.ID, r1.Of, w2.ID, r2.ID, r2.Of)
	}
}

// Hosts before this fix persisted answered turns, including pending wakes.
// Loading that state must not reconnect a later notification to Telegram.
func TestRelayClosesPreviouslyAnsweredTurnsOnRestart(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			rr := newRelayRig(t)
			rr.must(AgentClaude, EventUserPromptSubmit, agID, "", map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-5] hi"})
			b, err := os.ReadFile(rr.r.statePath)
			if err != nil {
				t.Fatal(err)
			}
			var state map[string]any
			if err := json.Unmarshal(b, &state); err != nil {
				t.Fatal(err)
			}
			turn := state["sessions"].(map[string]any)["s1"].(map[string]any)["turn"].(map[string]any)
			turn["answers"], turn["pending"] = 1, pending
			b, _ = json.Marshal(state)
			if err := os.WriteFile(rr.r.statePath, b, 0o600); err != nil {
				t.Fatal(err)
			}
			rr.open()
			rr.sysPrompt("s1")
			rr.must(AgentClaude, EventStop, agID, "", map[string]any{"session_id": "s1", "stop_hook_active": true, "last_assistant_message": "LOCAL"})
			if got := rr.emitted(); len(got) != 0 {
				t.Fatalf("old answered turn emitted %+v", got)
			}
		})
	}
}

func TestRelayMigrationResumesAnOutstandingWait(t *testing.T) {
	rr := newRelayRig(t)
	rr.must(AgentClaude, EventUserPromptSubmit, agID, "", map[string]any{"session_id": "s1", "prompt": "[Telegram · tg-5] hi"})
	rr.must(AgentClaude, EventNotify, agID, "", map[string]any{"session_id": "s1", "notification_type": "permission_prompt"})
	waiting := rr.emitted()[0].A.ID
	rr.r.st.Sessions["s1"].Turn.Answers = 1 // old host: a wait from a later background wake
	if err := rr.r.saveLocked(); err != nil {
		t.Fatal(err)
	}
	// Failed resumed emission must leave the saved legacy turn retryable.
	rr.fail = errors.New("disk full")
	if _, err := NewRelay(rr.r.statePath, rr.r.spoolDir, "mac2", func() time.Time { return rr.now }, func(string, proto.HookAnswer) error { return rr.fail }); err == nil {
		t.Fatal("migration acked failed resume")
	}
	rr.fail = nil
	rr.open()
	got := rr.emitted()
	if len(got) != 2 || got[1].Kind != proto.EventHookResumed || got[1].A.Of != waiting {
		t.Fatalf("migration emitted %+v", got)
	}
	rr.open() // migrated state was saved: no second resumed event
	if len(rr.emitted()) != 2 {
		t.Fatalf("migration repeated: %+v", rr.emitted())
	}
}
