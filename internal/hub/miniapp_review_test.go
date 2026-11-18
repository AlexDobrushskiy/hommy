package hub

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

// /status shows 8-char session ids and the host resolves unique prefixes, so
// a short id names a live session (not a gone one) and switches by its name.
func TestSessionTargetByIDPrefix(t *testing.T) {
	r := newRig(t)
	r.heartbeat("mac2", agtermSession{ID: mac2Sess, Name: "ev-test", Agent: "claude", State: "ready"})
	r.fakeConnected("mac2")
	r.text("/target session@mac2:" + mac2Sess[:8])
	cs := r.tg.waitFor(t, "switch or gone", func(cs []call) bool {
		return count(cs, "send", "Now talking to") == 1 || count(cs, "send", "is gone") == 1
	})
	if count(cs, "send", "is gone") != 0 {
		t.Fatalf("a live session's short id counted as gone: %+v", lastWith(cs, "send", "is gone"))
	}
	if sw := lastWith(cs, "send", "Now talking to"); !strings.Contains(sw.Text, "ev-test · mac2") {
		t.Errorf("switch notice: %q", sw.Text)
	}
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "session@mac2:"+mac2Sess {
		t.Errorf("target %q, want the full id", tgt)
	}
}

// A reply to the /spawn confirmation goes to the spawned session, not to
// whatever the target is by then.
func TestReplyToSpawnConfirmationGoesToNewSession(t *testing.T) {
	r := startAgtermHost(t)
	r.text("/spawn mac1:~/work ev-new find me a car")
	cs := r.tg.waitFor(t, "spawned", has("send", "🆕 ev-new started on mac1"))
	confirm := lastWith(cs, "send", "🆕 ev-new started on mac1").MsgID
	spawned := "BEEF0001-0000-0000-0000-000000000000"

	r.text("/target vault@mac1")
	r.tg.waitFor(t, "switch away", has("edit", "🎯 Talking to: vault · mac1"))
	r.send(func(m *models.Message) {
		m.Text = "and a blue one"
		m.ReplyToMessage = &models.Message{ID: confirm, Chat: models.Chat{ID: ownerUser}, Text: "🆕 ev-new started on mac1"}
	})
	eventually(t, "the reply typed into the spawned session", func() bool { return len(r.ag.submitted(spawned)) == 2 })
	if got := r.ag.submitted(spawned); !strings.HasSuffix(got[1], "] and a blue one") {
		t.Errorf("spawned session got %q", got)
	}
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "vault@mac1" {
		t.Errorf("a reply must not switch the target: %q", tgt)
	}
}

// Confirmations stored before the fix carry "session@<host>:" + the session id.
func TestRouteReplyToLegacySpawnConfirmation(t *testing.T) {
	r := newRig(t)
	if err := r.st.AddMessage(Message{ChatID: ownerUser, MsgID: 77, Direction: "out", Kind: "answer",
		Target: "session@mac2:", SessionID: mac2Sess}, r.clk.Now()); err != nil {
		t.Fatal(err)
	}
	rt, err := r.hub.route(ownerUser, &models.Update{Message: &models.Message{Text: "x", ReplyToMessage: &models.Message{ID: 77}}})
	if err != nil || rt.Rule != RuleReply || rt.Target.String() != "session@mac2:"+mac2Sess {
		t.Fatalf("route %+v %v", rt, err)
	}
}

// A pin that failed once is retried on the same line (not re-sent).
func TestFailedPinIsRetried(t *testing.T) {
	r := newRig(t)
	r.tg.mu.Lock()
	r.tg.failPin = errors.New("Too Many Requests: retry after 1")
	r.tg.mu.Unlock()
	r.text("/status")
	cs := r.tg.waitFor(t, "status", has("send", "hosts:"))
	line := lastWith(cs, "send", "🎯 Talking to:").MsgID
	if line == 0 || count(cs, "pin", "") != 0 {
		t.Fatalf("pinned line not sent or pinned despite the failure: %+v", cs)
	}
	r.tg.mu.Lock()
	r.tg.failPin = nil
	r.tg.mu.Unlock()
	r.text("/status")
	cs = r.tg.waitFor(t, "second status", func(cs []call) bool { return count(cs, "send", "hosts:") == 2 })
	if count(cs, "send", "🎯 Talking to:") != 1 {
		t.Errorf("the line was re-sent instead of re-pinned")
	}
	pinned := false
	for _, c := range cs {
		pinned = pinned || c.Kind == "pin" && c.MsgID == line
	}
	if !pinned {
		t.Errorf("line %d never pinned after the failure: %+v", line, cs)
	}
	r.text("/status") // pinned once is enough
	cs = r.tg.waitFor(t, "third status", func(cs []call) bool { return count(cs, "send", "hosts:") == 3 })
	if n := count(cs, "pin", ""); n != 1 {
		t.Errorf("%d pin calls, want 1", n)
	}
}

// A keyboard token stays valid for its whole 7-day cap even when no bot
// message reissued it: checking a token must not bump the keyboard serial.
func TestPickerTokenValidUntilCapWithoutReissue(t *testing.T) {
	r := newRig(t)
	tok := r.pickerURLToken()
	r.clk.Add(6*24*time.Hour + time.Hour) // past the reissue age, no bot message since
	if code, body := r.api("/api/picker", map[string]string{"X-Picker-Token": tok}); code != 200 {
		t.Fatalf("6-day-old keyboard token: %d %v", code, body)
	}
	if code, body := r.api("/api/picker", map[string]string{"X-Picker-Token": tok}); code != 200 {
		t.Fatalf("same token again: %d %v", code, body)
	}
	r.clk.Add(24 * time.Hour)
	if code, body := r.api("/api/picker", map[string]string{"X-Picker-Token": tok}); code != 401 || body["reason"] != "expired" {
		t.Errorf("7-day-old token: %d %v", code, body)
	}
}
