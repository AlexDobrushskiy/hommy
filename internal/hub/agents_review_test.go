package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

func (r *rig) postSend(req proto.SendRequest) (int, proto.SendResponse) {
	r.t.Helper()
	code, body := r.post("/api/send", req)
	var res proto.SendResponse
	if err := json.Unmarshal(body, &res); err != nil {
		r.t.Fatalf("send: %d %s", code, body)
	}
	return code, res
}

// A multipart send that failed half way: a retry of the same id (its answer
// was lost) sends only the rest.
func TestSendRetryResumesAfterAPartialSend(t *testing.T) {
	r := newRig(t)
	text := strings.Repeat("a", 3900) + "\n" + strings.Repeat("b", 3900) + "\n" + "tail"
	fails := 1
	r.tg.setFailSend(func(s string) error {
		if strings.HasPrefix(s, "b") && fails > 0 {
			fails--
			return errors.New("telegram: timeout")
		}
		return nil
	})
	req := proto.SendRequest{ID: "multi", Text: text, Agent: &proto.Agent{Slug: "a1"}}
	code, first := r.postSend(req)
	if code == http.StatusOK || len(first.MessageIDs) != 1 {
		t.Fatalf("first try: %d %+v", code, first)
	}
	code, again := r.postSend(req)
	if code != http.StatusOK || len(again.MessageIDs) != 2 || again.MessageIDs[0] != first.MessageIDs[0] {
		t.Fatalf("retry: %d %+v", code, again)
	}
	if n := count(r.tg.snapshot(), "send", strings.Repeat("a", 100)); n != 1 {
		t.Errorf("the first part went out %d times", n)
	}
}

func spoolEvent(e proto.SpoolEntry) proto.Event {
	b, _ := json.Marshal(e)
	return proto.Event{JobID: e.ID, Kind: proto.EventSpool, Data: b}
}

// testBot is the bot id of testConfig's token.
const testBot = 123456

// A spool entry the hub couldn't store is not acked (the host keeps it and
// sends it again); once storage works it is recorded.
func TestSpoolNotAckedWhenRecordingFails(t *testing.T) {
	r := newRig(t)
	if _, err := r.st.db.Exec(`CREATE TRIGGER no_pings BEFORE INSERT ON pings BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	e := spoolEvent(proto.SpoolEntry{ID: "sp1", Kind: proto.SpoolSent, Bot: testBot, ChatID: ownerUser, MessageIDs: []int{7001},
		Texts: []string{"🤖 a1 · mac1\nx"}, Agent: &proto.Agent{Slug: "a1", Session: "AAAA1111-0000"}})
	if err := r.hub.handleSpool(t.Context(), "mac1", e); err == nil {
		t.Fatal("acked although the ping wasn't stored")
	}
	r.st.db.Exec(`DROP TRIGGER no_pings`)
	if err := r.hub.handleSpool(t.Context(), "mac1", e); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.st.GetPing(ownerUser, 7001); p == nil || p.Slug != "a1" {
		t.Fatalf("ping %+v", p)
	}
}

// A spooled 🫡 that Telegram refused for now is retried; one for a message
// the hub doesn't know is dropped.
func TestSpoolAckRetriedOnTransientFailure(t *testing.T) {
	r := newRig(t)
	ids := r.sendPing(proto.SendRequest{ID: "s", Text: "hi", Agent: &proto.Agent{Slug: "a1"}})
	r.tg.setFailReact(errors.New("telegram: 429 Too Many Requests"))
	e := spoolEvent(proto.SpoolEntry{ID: "sp2", Kind: proto.SpoolAck, Bot: testBot, ChatID: ownerUser, Ack: &proto.AckRequest{MsgID: ids[0], Item: "tg-1"}})
	if err := r.hub.handleSpool(t.Context(), "mac1", e); err == nil {
		t.Fatal("acked although the 🫡 failed")
	}
	r.tg.setFailReact(nil)
	if err := r.hub.handleSpool(t.Context(), "mac1", e); err != nil {
		t.Fatal(err)
	}
	if !reactedOn(ids[0], "🫡")(r.tg.snapshot()) {
		t.Error("no 🫡")
	}
	unknown := spoolEvent(proto.SpoolEntry{ID: "sp3", Kind: proto.SpoolAck, Bot: testBot, ChatID: ownerUser, Ack: &proto.AckRequest{MsgID: 99999, Item: "tg-9"}})
	if err := r.hub.handleSpool(t.Context(), "mac1", unknown); err != nil {
		t.Errorf("an ack for an unknown message can never succeed: %v", err)
	}
}

// inbox ack: a Telegram hiccup is 5xx (the CLI spools and retries), an
// unknown message 4xx (final).
func TestAckTransientFailureIsRetryable(t *testing.T) {
	r := newRig(t)
	ids := r.sendPing(proto.SendRequest{ID: "s", Text: "hi", Agent: &proto.Agent{Slug: "a1"}})
	r.tg.setFailReact(errors.New("telegram: connection reset"))
	if code, body := r.post("/api/ack", proto.AckRequest{MsgID: ids[0], Item: "tg-1"}); code < 500 {
		t.Errorf("transient: %d %s", code, body)
	}
	r.tg.setFailReact(nil)
	if code, body := r.post("/api/ack", proto.AckRequest{MsgID: 99999, Item: "tg-9"}); code != http.StatusBadRequest {
		t.Errorf("unknown message: %d %s", code, body)
	}
}

// Spool entries carry the bot they were sent with: one from another bot (dev
// vs live) is never recorded, whatever hub URL it names.
func TestSpoolFromAnotherBotDropped(t *testing.T) {
	r := newRig(t)
	for _, bot := range []int64{0, 999} {
		e := spoolEvent(proto.SpoolEntry{ID: "x", Kind: proto.SpoolSent, Bot: bot, ChatID: ownerUser, MessageIDs: []int{7100},
			Texts: []string{"🤖 a1 · mac1\nx"}, Agent: &proto.Agent{Slug: "a1", Session: "AAAA1111-0000"}})
		if err := r.hub.handleSpool(t.Context(), "mac1", e); err != nil {
			t.Fatal(err)
		}
		if p, _ := r.st.GetPing(ownerUser, 7100); p != nil {
			t.Fatalf("bot %d: recorded %+v", bot, p)
		}
	}
}

func (r *rig) waitParked(n int) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got int
		r.st.db.QueryRow(`SELECT count(*) FROM events WHERE status = ?`, EventParked).Scan(&got)
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%d events parked, want %d", got, n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// The hub was down while an agent sent directly; the owner pressed, reacted and
// replied before the host flushed its spool. Those wait for the spool and
// then reach the agent, instead of being dropped or going to the target.
func TestUpdatesBeforeTheSpoolWaitForIt(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.add(sessA)
	brief := r.agentBrief("2026-11-01-hc-e2e-agent", sessA)
	text := func(body string) string { return "🤖 " + pingAgent + " · mac1\n" + body }

	r.press(7001, 0)
	r.react(7002, nil, []string{"👍"})
	reply := r.send(func(m *models.Message) {
		m.Text = "ok then"
		m.ReplyToMessage = &models.Message{ID: 7003, Date: int(r.clk.Now().Unix()) - 60, Text: text("hub was down")}
	})
	r.waitParked(3)
	if n := len(r.ag.submitted(sessA)); n != 0 || count(r.tg.snapshot(), "send", "answer to [ok then]") != 0 {
		t.Fatalf("routed before the spool: %d typed, calls %+v", n, r.tg.snapshot())
	}

	agent := &proto.Agent{Slug: pingAgent, Session: sessA, BriefDir: brief}
	for _, e := range []proto.SpoolEntry{
		{ID: "a", MessageIDs: []int{7001}, Texts: []string{text("which?")}, Buttons: []string{"Keep", "Drop"}},
		{ID: "b", MessageIDs: []int{7002}, Texts: []string{text("deposit?")}},
		{ID: "c", MessageIDs: []int{7003}, Texts: []string{text("hub was down")}},
	} {
		e.Kind, e.Bot, e.ChatID, e.Agent = proto.SpoolSent, testBot, ownerUser, agent
		if err := r.hub.handleSpool(t.Context(), "mac1", spoolEvent(e)); err != nil {
			t.Fatal(err)
		}
	}
	got := strings.Join(r.waitTyped(sessA, 3), "\n")
	for _, want := range []string{`: which?"] Keep`, `: deposit?"] 👍 yes / continue`, fmt.Sprintf(`tg-%d · re "🤖 hc-e2e-agent: hub was down"] ok then`, reply)} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	r.waitParked(0)
}

// No spool ever comes: after ParkTTL the reply is not delivered (the owner is
// told), the press and the reaction are ignored.
func TestParkedUpdatesExpire(t *testing.T) {
	r := startAgtermHost(t)
	r.press(7001, 0)
	r.send(func(m *models.Message) {
		m.Text = "ok then"
		m.ReplyToMessage = &models.Message{ID: 7003, Date: int(r.clk.Now().Unix()), Text: "🤖 ghost · mac1\nhello"}
	})
	r.waitParked(2)
	r.clk.Add(ParkTTL + time.Second)
	r.hub.wake()
	r.tg.waitFor(t, "the owner told", has("send", "Not delivered"))
	r.waitParked(0)
	if n := count(r.tg.snapshot(), "send", "answer to [ok then]"); n != 0 {
		t.Errorf("the reply reached the current target %d times", n)
	}
}

// A reply to an old agent message (before the hub's time) isn't held, and
// isn't guessed into the current target either.
func TestReplyToOldUnknownPingNotParked(t *testing.T) {
	r := startAgtermHost(t)
	r.send(func(m *models.Message) {
		m.Text = "late reply"
		m.ReplyToMessage = &models.Message{ID: 50, Date: int(r.clk.Now().Add(-48 * time.Hour).Unix()), Text: "🤖 ghost@mac1\nold"}
	})
	r.tg.waitFor(t, "the owner told", has("send", "Not delivered"))
	if n := count(r.tg.snapshot(), "send", "answer to [late reply]"); n != 0 {
		t.Errorf("the reply reached the current target %d times", n)
	}
}

// The 1838 incident: an agent pinged around the hub (no header, no record)
// while the chat's target was another session; the owner replied. The reply waits
// for a spool entry, then is refused instead of reaching the target.
func TestReplyToMessageSentAroundTheHub(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.add(sessA)
	if err := r.st.SetChatTarget(ownerUser, "session@mac1:"+sessA, r.clk.Now()); err != nil {
		t.Fatal(err)
	}
	r.send(func(m *models.Message) {
		m.Text = "Kill all its sessions"
		m.ReplyToMessage = &models.Message{ID: 7010, Date: int(r.clk.Now().Unix()) - 60,
			From: &models.User{ID: testBot, IsBot: true}, Text: "Claude needs you\nDev still frozen: kill or wait?"}
	})
	r.waitParked(1)
	r.clk.Add(ParkTTL + time.Second)
	r.hub.wake()
	r.tg.waitFor(t, "the owner told", has("send", "Not delivered"))
	r.waitParked(0)
	if got := r.ag.submitted(sessA); len(got) != 0 {
		t.Fatalf("typed into the current target: %q", got)
	}
}

// The same message, but its spool entry (a plain send that names its
// session) lands while the reply waits: the reply reaches that session.
func TestReplyToPlainMessageRoutesByItsSpooledSession(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.add(sessA)
	r.ag.add(sessB)
	if err := r.st.SetChatTarget(ownerUser, "session@mac1:"+sessB, r.clk.Now()); err != nil {
		t.Fatal(err)
	}
	text := "Claude needs you\nkill or wait?"
	r.send(func(m *models.Message) {
		m.Text = "kill"
		m.ReplyToMessage = &models.Message{ID: 7011, Date: int(r.clk.Now().Unix()) - 60,
			From: &models.User{ID: testBot, IsBot: true}, Text: text}
	})
	r.waitParked(1)
	e := proto.SpoolEntry{ID: "p", Kind: proto.SpoolSent, Bot: testBot, ChatID: ownerUser, MessageIDs: []int{7011}, Texts: []string{text},
		Agent: &proto.Agent{Slug: "acme-review", Session: sessA}}
	if err := r.hub.handleSpool(t.Context(), "mac1", spoolEvent(e)); err != nil {
		t.Fatal(err)
	}
	if got := r.waitTyped(sessA, 1); !strings.HasSuffix(got[0], "] kill") {
		t.Fatalf("typed %q", got)
	}
	if got := r.ag.submitted(sessB); len(got) != 0 {
		t.Fatalf("typed into the current target: %q", got)
	}
}

// "Answer…" then a voice note that can't be transcribed: the owner retypes, and
// the retyped text still answers the agent.
func TestAnswerContextSurvivesAFailedVoiceNote(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.add(sessA)
	brief := r.agentBrief("2026-11-01-hc-e2e-agent", sessA)
	ids := r.sendPing(proto.SendRequest{ID: "s", Text: "which?", Buttons: []string{"Answer…"},
		Agent: &proto.Agent{Slug: pingAgent, Session: sessA, BriefDir: brief}})
	r.press(ids[0], 0)
	r.tg.waitFor(t, "answer context", has("edit", "your next message goes to"))
	r.tg.mu.Lock()
	r.tg.files["v1"] = []byte("OggS")
	r.tg.mu.Unlock()
	r.sp.setFailSTT(errors.New("whisper down"))
	r.send(func(m *models.Message) { m.Voice = &models.Voice{FileID: "v1"} })
	r.tg.waitFor(t, "retype", has("send", "please retype"))
	r.sp.setFailSTT(nil)
	retyped := r.text("the blue one")
	r.tg.waitFor(t, "typed", reactedOn(retyped, "✍"))
	if got := r.ag.submitted(sessA); len(got) != 1 || !strings.HasSuffix(got[0], `: which?"] the blue one`) {
		t.Fatalf("typed %q", got)
	}
}

// Buttons that didn't attach are reported, and a retry of the id attaches
// them without sending the text again.
func TestSendButtonFailureReportedAndRetried(t *testing.T) {
	r := newRig(t)
	r.tg.setFailButtons(errors.New("telegram: 502"))
	req := proto.SendRequest{ID: "btn", Text: "pick one", Buttons: []string{"A", "B"}, Agent: &proto.Agent{Slug: "a1"}}
	code, res := r.postSend(req)
	if code != http.StatusOK || !strings.Contains(res.Warning, "buttons") {
		t.Fatalf("first: %d %+v", code, res)
	}
	r.tg.setFailButtons(nil)
	code, again := r.postSend(req)
	if code != http.StatusOK || again.Warning != "" || again.MessageIDs[0] != res.MessageIDs[0] {
		t.Fatalf("retry: %d %+v", code, again)
	}
	if n := count(r.tg.snapshot(), "send", "pick one"); n != 1 {
		t.Errorf("text sent %d times", n)
	}
	if b := lastWith(r.tg.snapshot(), "buttons", ""); b.MsgID != res.MessageIDs[0] {
		t.Errorf("buttons not attached on retry: %+v", b)
	}
}

// A joined host has no bot token: its spooled 🫡 carries bot 0 and is still
// applied (an ack only reacts on a message this hub sent).
func TestSpoolAckFromTokenlessHost(t *testing.T) {
	r := newRig(t)
	ids := r.sendPing(proto.SendRequest{ID: "s", Text: "hi", Agent: &proto.Agent{Slug: "a1"}})
	e := spoolEvent(proto.SpoolEntry{ID: "sp9", Kind: proto.SpoolAck, Bot: 0, ChatID: ownerUser, Ack: &proto.AckRequest{MsgID: ids[0], Item: "tg-1"}})
	if err := r.hub.handleSpool(t.Context(), "h2", e); err != nil {
		t.Fatal(err)
	}
	if !reactedOn(ids[0], "🫡")(r.tg.snapshot()) {
		t.Error("no 🫡 for a tokenless host's ack")
	}
}
