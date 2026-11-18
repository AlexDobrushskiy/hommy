package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/host"
	"github.com/AlexDobrushskiy/hommy/internal/ping"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// agentBrief makes ~/agent-briefs/<name> owned by session id.
func (r *agtermRig) agentBrief(name, id string) string {
	dir := filepath.Join(r.dir, "agent-briefs", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.t.Fatal(err)
	}
	r.setActive(dir, id)
	real, _ := filepath.EvalSymlinks(dir)
	return real
}

func (r *agtermRig) setActive(dir, id string) {
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"active_agent":"`+id+`"}`), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// post calls the host API as mac1.
func (r *rig) post(path string, body any) (int, []byte) {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+hostKey)
	req.Header.Set("X-Hommy-Host", "mac1")
	w := httptest.NewRecorder()
	r.hub.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

func (r *rig) sendPing(req proto.SendRequest) []int {
	r.t.Helper()
	code, body := r.post("/api/send", req)
	var res proto.SendResponse
	if err := json.Unmarshal(body, &res); err != nil || code != http.StatusOK || !res.OK {
		r.t.Fatalf("send: %d %s", code, body)
	}
	return res.MessageIDs
}

func emojiReactions(es ...string) []models.ReactionType {
	var out []models.ReactionType
	for _, e := range es {
		out = append(out, models.ReactionType{Type: models.ReactionTypeTypeEmoji, ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: e}})
	}
	return out
}

func (r *rig) react(msgID int, old, new []string) {
	r.nextMs++
	r.hub.intake(&models.Update{ID: int64(r.nextMs), MessageReaction: &models.MessageReactionUpdated{
		Chat: models.Chat{ID: ownerUser, Type: models.ChatTypePrivate}, MessageID: msgID, User: &models.User{ID: ownerUser}, Date: 1,
		OldReaction: emojiReactions(old...), NewReaction: emojiReactions(new...)}})
}

func (r *rig) press(msgID, i int) string {
	r.nextMs++
	id := fmt.Sprintf("q%d", r.nextMs)
	r.hub.intake(&models.Update{ID: int64(r.nextMs), CallbackQuery: &models.CallbackQuery{ID: id, From: models.User{ID: ownerUser},
		Data: fmt.Sprintf("ag:%d:%d", msgID, i), Message: models.MaybeInaccessibleMessage{Type: models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{ID: msgID, Date: 1, Chat: models.Chat{ID: ownerUser, Type: models.ChatTypePrivate}}}}})
	return id
}

func (r *agtermRig) waitTyped(id string, n int) []string {
	r.t.Helper()
	r.tg.waitFor(r.t, fmt.Sprintf("%d lines typed", n), func([]call) bool { return len(r.ag.submitted(id)) >= n })
	return r.ag.submitted(id)
}

const pingAgent = "hc-e2e-agent"

func TestAgentPingReplyReactionButtons(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.add(sessA)
	brief := r.agentBrief("2026-11-01-hc-e2e-agent", sessA)
	agent := &proto.Agent{Slug: pingAgent, Host: "spoofed", Session: strings.ToLower(sessA), BriefDir: brief,
		Link: "https://claude.ai/code/session_x"}

	// a ping: header (the host from the key, not the request), body, link; an "Open" button (the session's console)
	ids := r.sendPing(proto.SendRequest{ID: "s1", Text: "dealer 3 asks a €500 deposit, ok?", Agent: agent})
	cs := r.tg.waitFor(t, "ping", has("send", "dealer 3"))
	ping := lastWith(cs, "send", "dealer 3")
	if want := "🤖 hc-e2e-agent · mac1\ndealer 3 asks a €500 deposit, ok?\nhttps://claude.ai/code/session_x"; ping.Text != want || ping.KB != nil || !isOpenRow(ping.Opts.Inline, sessA) || len(ids) != 1 || ids[0] != ping.MsgID {
		t.Fatalf("ping %+v ids %v", ping, ids)
	}

	// swipe-reply while the target is vault: typed into the agent with the short quote; target unchanged
	reply := r.send(func(m *models.Message) {
		m.Text = "drop it"
		m.ReplyToMessage = &models.Message{ID: ping.MsgID, Text: ping.Text}
	})
	r.tg.waitFor(t, "✍ on the reply", reactedOn(reply, "✍"))
	want := fmt.Sprintf(`[Telegram · tg-%d · re "🤖 hc-e2e-agent: dealer 3 asks a €500 deposit, ok?"] drop it`, reply)
	if got := r.waitTyped(sessA, 1); got[0] != want {
		t.Fatalf("typed %q\nwant  %q", got[0], want)
	}
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "" {
		t.Errorf("target changed to %q", tgt)
	}

	// 👍, then removed
	r.react(ping.MsgID, nil, []string{"👍"})
	got := r.waitTyped(sessA, 2)
	if !strings.HasSuffix(got[1], `"] 👍 yes / continue`) || !strings.Contains(got[1], fmt.Sprintf("tg-%d-r", ping.MsgID)) {
		t.Fatalf("reaction typed %q", got[1])
	}
	r.react(ping.MsgID, []string{"👍"}, nil)
	if got = r.waitTyped(sessA, 3); !strings.HasSuffix(got[2], "] retracted 👍") {
		t.Fatalf("retraction typed %q", got[2])
	}
	r.tg.waitFor(t, "✍ on the ping (the reactions' ack)", reactedOn(ping.MsgID, "✍"))
	inbox, _ := os.ReadFile(filepath.Join(brief, "inbox.jsonl"))
	if strings.Count(string(inbox), "\n") != 3 || !strings.Contains(string(inbox), `"kind":"reaction"`) {
		t.Errorf("inbox:\n%s", inbox)
	}

	// buttons: sent bare, then ag:<id>:<i> attached; pressing Drop answers, edits, types
	ids = r.sendPing(proto.SendRequest{ID: "s2", Text: "which?", Agent: agent, Buttons: []string{"Keep", "Drop", "Answer…"}})
	cs = r.tg.waitFor(t, "buttons", has("buttons", ""))
	bt := lastWith(cs, "buttons", "")
	if bt.MsgID != ids[0] || len(bt.Buttons) != 2 || bt.Buttons[0][1].Data != fmt.Sprintf("ag:%d:1", ids[0]) || bt.Buttons[0][2].Text != "Answer…" || !isOpenRow(bt.Buttons[1:], sessA) {
		t.Fatalf("buttons %+v", bt)
	}
	if s := lastWith(cs, "send", "which?"); !s.Opts.Bare || s.KB != nil {
		t.Errorf("a ping with buttons must go without the reply keyboard: %+v", s)
	}
	q := r.press(ids[0], 1)
	r.tg.waitFor(t, "callback answered", func(cs []call) bool {
		for _, c := range cs {
			if c.Kind == "answer" && c.Query == q && c.Text == "→ Drop" {
				return true
			}
		}
		return false
	})
	if got = r.waitTyped(sessA, 4); !strings.HasSuffix(got[3], `: which?"] Drop`) {
		t.Fatalf("button typed %q", got[3])
	}
	r.tg.waitFor(t, "edited", has("edit", "which?\nhttps://claude.ai/code/session_x\n→ Drop ✓"))
	r.tg.waitFor(t, "Open put back after the press", func(cs []call) bool {
		for _, c := range cs {
			if c.Kind == "buttons" && c.MsgID == ids[0] && isOpenRow(c.Buttons, sessA) {
				return true
			}
		}
		return false
	})

	// Answer…: the next text goes to the agent (not the target), once
	r.press(ids[0], 2)
	r.tg.waitFor(t, "answer context", has("edit", "→ Answer… (your next message goes to hc-e2e-agent) ✓"))
	a1 := r.text("take the blue one")
	r.tg.waitFor(t, "typed answer", reactedOn(a1, "✍"))
	if got = r.waitTyped(sessA, 5); got[4] != fmt.Sprintf(`[Telegram · tg-%d · re "🤖 hc-e2e-agent: which?"] take the blue one`, a1) {
		t.Fatalf("answer typed %q", got[4])
	}
	r.text("and this goes to vault")
	r.tg.waitFor(t, "vault", has("send", "answer to [and this goes to vault]"))
	if n := len(r.ag.submitted(sessA)); n != 5 {
		t.Errorf("the answer context must be one-shot: %d lines typed", n)
	}
}

func TestSendIdempotentAndValidated(t *testing.T) {
	r := newRig(t)
	req := proto.SendRequest{ID: "same", Text: "hello", Agent: &proto.Agent{Slug: "a1"}}
	first := r.sendPing(req)
	if again := r.sendPing(req); again[0] != first[0] || count(r.tg.snapshot(), "send", "hello") != 1 {
		t.Fatalf("a retried id must not send twice: %v %v", first, again)
	}
	for _, bad := range []proto.SendRequest{
		{ID: "b1", Text: "  "},
		{ID: "b2", Text: "x", Buttons: []string{"A"}},
		{ID: "b3", Text: "x", Agent: &proto.Agent{Slug: "../x"}},
		{ID: "b4", Text: "x", Agent: &proto.Agent{Slug: "a", Session: "rm -rf"}},
	} {
		if code, body := r.post("/api/send", bad); code != http.StatusBadRequest {
			t.Errorf("%+v: %d %s", bad, code, body)
		}
	}
	req2 := httptest.NewRequest(http.MethodPost, "/api/send", strings.NewReader(`{}`))
	req2.Header.Set("X-Hommy-Host", "mac1")
	req2.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	r.hub.Handler().ServeHTTP(w, req2)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bad key: %d", w.Code)
	}
	// plain: no header, a notice nobody's replies route to
	ids := r.sendPing(proto.SendRequest{ID: "p", Text: "plain one", Silent: true})
	if c := lastWith(r.tg.snapshot(), "send", "plain one"); c.Text != "plain one" || !c.Opts.Silent {
		t.Errorf("plain %+v", c)
	}
	if m, _ := r.st.GetMessage(ownerUser, ids[0]); m == nil || m.Target != "" {
		t.Errorf("plain message record %+v", m)
	}
}

func TestAskAddsAnswerButton(t *testing.T) {
	r := newRig(t)
	ids := r.sendPing(proto.SendRequest{ID: "a", Text: "name?", Ask: true, Agent: &proto.Agent{Slug: "ev"}})
	b := lastWith(r.tg.waitFor(t, "buttons", has("buttons", "")), "buttons", "")
	if b.MsgID != ids[0] || len(b.Buttons[0]) != 1 || b.Buttons[0][0].Text != "Answer…" {
		t.Errorf("--ask alone: %+v", b)
	}
	r.sendPing(proto.SendRequest{ID: "b", Text: "pick", Ask: true, Buttons: []string{"A"}, Agent: &proto.Agent{Slug: "ev"}})
	if b := lastWith(r.tg.snapshot(), "buttons", ""); len(b.Buttons[0]) != 2 || b.Buttons[0][1].Text != "Answer…" {
		t.Errorf("--ask with buttons adds Answer…: %+v", b)
	}
}

func TestSpoolFlushRecordsAndRoutes(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "spool")
	r := startAgtermHost(t, func(c *host.Config) { c.SpoolDir, c.HeartbeatEvery = spool, 20*time.Millisecond })
	os.MkdirAll(spool, 0o700)
	r.ag.add(sessA)
	brief := r.agentBrief("2026-11-01-hc-e2e-agent", sessA)
	write := func(e proto.SpoolEntry) string {
		b, _ := json.Marshal(e)
		p := filepath.Join(spool, e.ID+".json")
		os.WriteFile(p, b, 0o600)
		return p
	}
	sent := write(proto.SpoolEntry{ID: "sp1", Kind: proto.SpoolSent, Hub: r.url, Bot: testBot, ChatID: ownerUser, MessageIDs: []int{7001},
		Texts: []string{"🤖 hc-e2e-agent · mac1\nhub was down"}, Agent: &proto.Agent{Slug: pingAgent, Session: sessA, BriefDir: brief}})
	other := write(proto.SpoolEntry{ID: "sp2", Kind: proto.SpoolSent, Hub: "ws://elsewhere/ws", ChatID: ownerUser, MessageIDs: []int{7002}})
	waitGone(t, sent)
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another hub's entry must stay: %v", err)
	}
	reply := r.send(func(m *models.Message) {
		m.Text = "ok then"
		m.ReplyToMessage = &models.Message{ID: 7001, Text: "🤖 hc-e2e-agent · mac1\nhub was down"}
	})
	r.tg.waitFor(t, "typed", reactedOn(reply, "✍"))
	if got := r.ag.submitted(sessA); len(got) != 1 || !strings.Contains(got[0], `re "🤖 hc-e2e-agent: hub was down"] ok then`) {
		t.Fatalf("typed %q", got)
	}
	// a spooled ack → 🫡
	write(proto.SpoolEntry{ID: "sp3", Kind: proto.SpoolAck, Hub: r.url, Bot: testBot, ChatID: ownerUser, Ack: &proto.AckRequest{MsgID: reply, Item: fmt.Sprintf("tg-%d", reply)}})
	r.tg.waitFor(t, "🫡", reactedOn(reply, "🫡"))
}

func TestInboxAckSalutes(t *testing.T) {
	r := newRig(t)
	id := r.text("/status") // a stored message of the owner's
	r.tg.waitFor(t, "status", has("send", "hub"))
	if code, body := r.post("/api/ack", proto.AckRequest{MsgID: id, Item: fmt.Sprintf("tg-%d", id)}); code != http.StatusOK {
		t.Fatalf("ack: %d %s", code, body)
	}
	r.tg.waitFor(t, "🫡", reactedOn(id, "🫡"))
	if code, _ := r.post("/api/ack", proto.AckRequest{MsgID: 999999, Item: "tg-999999"}); code != http.StatusBadRequest {
		t.Errorf("an unknown message must be refused: %d", code)
	}
}

func TestReplyToGoneAgentReachesSuccessor(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.add(sessA)
	brief := r.agentBrief("2026-11-01-hc-e2e-agent", sessA)
	ids := r.sendPing(proto.SendRequest{ID: "s", Text: "still there?", Agent: &proto.Agent{Slug: pingAgent, Session: sessA, BriefDir: brief}})
	r.ag.mu.Lock()
	delete(r.ag.sessions, sessA)
	r.ag.mu.Unlock()
	// the folder's state.json now names nobody live: 🤷, kept in the inbox
	reply := r.send(func(m *models.Message) { m.Text = "yes"; m.ReplyToMessage = &models.Message{ID: ids[0]} })
	r.tg.waitFor(t, "🤷", reactedOn(reply, "🤷"))
	if b, _ := os.ReadFile(filepath.Join(brief, "inbox.jsonl")); !strings.Contains(string(b), `"text":"yes"`) {
		t.Fatalf("inbox: %s", b)
	}
	// a successor takes the folder: the retry loop types it there
	const next = "CCCC3333-0000-0000-0000-000000000000"
	r.ag.add(next)
	r.setActive(brief, next)
	r.clock.Sleep(agterm.RetryEvery)
	r.svc.Queue.Tick(r.ctx)
	r.tg.waitFor(t, "✍ when typed into the successor", reactedOn(reply, "✍"))
	if got := r.ag.submitted(next); len(got) != 1 || !strings.HasSuffix(got[0], "] yes") {
		t.Fatalf("successor got %q", got)
	}
}

func TestReactionOnVaultAnswerAndUnknown(t *testing.T) {
	r := newRig(t)
	r.startHost()
	r.text("vault question")
	cs := r.tg.waitFor(t, "answer", has("send", "answer to [vault question]"))
	ans := r.answers(cs)[0].MsgID
	r.react(ans, nil, []string{"🔥"})
	r.tg.waitFor(t, "vault run for the reaction", has("send", fmt.Sprintf("answer to [[The owner reacted to your message tg-%d] 🔥 urgent]", ans)))
	before := len(r.tg.snapshot())
	r.react(424242, nil, []string{"👍"}) // unknown message: ignored
	r.press(424242, 0)                  // unknown button: answered, nothing else
	cs = r.tg.waitFor(t, "unknown button answered", has("answer", "doesn't know"))
	for _, c := range cs[before:] {
		if c.Kind != "answer" {
			t.Errorf("unexpected %+v", c)
		}
	}
}

func waitGone(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still there", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// isOpenRow: rows are exactly the console's "Open" button for session id on mac1.
func isOpenRow(rows [][]ping.Button, id string) bool {
	return len(rows) == 1 && len(rows[0]) == 1 && rows[0][0].Text == "Open" && rows[0][0].WebApp != nil &&
		rows[0][0].WebApp.URL == "https://agents.test/console?target=session%40mac1%3A"+id
}

// A plain send from an agent session goes out as is but names its session:
// the owner's reply reaches that session even while the target is another one.
func TestPlainSendWithSessionRoutesReplies(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.add(sessA)
	r.ag.add(sessB)
	if err := r.st.SetChatTarget(ownerUser, "session@mac1:"+sessB, r.clk.Now()); err != nil {
		t.Fatal(err)
	}
	ids := r.sendPing(proto.SendRequest{ID: "p1", Text: "Claude needs you\nkill or wait?", Plain: true,
		Agent: &proto.Agent{Slug: "acme-review", Session: sessA, Link: "https://claude.ai/code/session_x"}})
	cs := r.tg.waitFor(t, "plain send", has("send", "kill or wait?"))
	sent := lastWith(cs, "send", "kill or wait?")
	if sent.Text != "Claude needs you\nkill or wait?" || len(sent.Opts.Inline) != 0 || len(ids) != 1 {
		t.Fatalf("plain send %+v", sent)
	}
	reply := r.send(func(m *models.Message) {
		m.Text = "kill"
		m.ReplyToMessage = &models.Message{ID: ids[0], From: &models.User{ID: testBot, IsBot: true}, Text: sent.Text}
	})
	r.tg.waitFor(t, "✍ on the reply", reactedOn(reply, "✍"))
	if got := r.ag.submitted(sessA); len(got) != 1 || !strings.HasSuffix(got[0], "] kill") {
		t.Fatalf("typed %q", got)
	}
	if got := r.ag.submitted(sessB); len(got) != 0 {
		t.Fatalf("typed into the current target: %q", got)
	}
	// buttons stay an --agent feature
	if code, _ := r.post("/api/send", proto.SendRequest{ID: "p2", Text: "x", Plain: true, Buttons: []string{"A"},
		Agent: &proto.Agent{Slug: "acme-review", Session: sessA}}); code != http.StatusBadRequest {
		t.Errorf("plain + buttons: %d", code)
	}
}
