package hub

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

const mac2Sess = "AAAA1111-2222-3333-4444-555566667777"

// heartbeat stores an agterm snapshot for host, as its host would report it.
func (r *rig) heartbeat(host string, sessions ...agtermSession) {
	r.t.Helper()
	raw, _ := json.Marshal(agtermSnap{Sessions: sessions, Folders: []agtermFolder{{Dir: "/Users/tester/agent-briefs/2026-11-01-x", Name: "2026-11-01-x"}}})
	if err := r.st.HostSeen(host, &proto.Heartbeat{Host: host, Version: "v1", Agterm: raw}, r.clk.Now()); err != nil {
		r.t.Fatal(err)
	}
}

// fakeConnected marks a host connected without a socket (no jobs may go to it).
func (r *rig) fakeConnected(host string) {
	r.hub.mu.Lock()
	r.hub.conns[host] = &hostConn{name: host}
	r.hub.mu.Unlock()
	r.t.Cleanup(func() { // before the hub stops: it would close the missing socket
		r.hub.mu.Lock()
		delete(r.hub.conns, host)
		r.hub.mu.Unlock()
	})
}

func (r *rig) webAppData(payload string) int {
	return r.send(func(m *models.Message) {
		m.WebAppData = &models.WebAppData{Data: payload, ButtonText: "🎯 vault · mac1 ▾"}
	})
}

func lastWith(cs []call, kind, substr string) call {
	for i := len(cs) - 1; i >= 0; i-- {
		if cs[i].Kind == kind && strings.Contains(cs[i].Text, substr) {
			return cs[i]
		}
	}
	return call{}
}

func kbButtons(kb *models.ReplyKeyboardMarkup) []string {
	if kb == nil || len(kb.Keyboard) == 0 {
		return nil
	}
	var out []string
	for _, b := range kb.Keyboard[0] {
		out = append(out, b.Text)
	}
	return out
}

// codeCovers reports whether a code entity covers the UTF-16 range of sub in text.
func codeCovers(text string, ents []models.MessageEntity, sub string) bool {
	i := strings.Index(text, sub)
	if i < 0 {
		return false
	}
	off, n := u16len(text[:i]), u16len(sub)
	for _, e := range ents {
		if e.Type == models.MessageEntityTypeCode && e.Offset <= off && off+n <= e.Offset+e.Length {
			return true
		}
	}
	return false
}

func TestFreshChatGetsIndicators(t *testing.T) {
	r := newRig(t)
	r.startHost()
	r.text("hello")
	cs := r.tg.waitFor(t, "answer", has("send", "answer to [hello]"))

	pin := lastWith(cs, "send", "🎯 Talking to: vault · mac1")
	if pin.MsgID == 0 || count(cs, "pin", "") != 1 || !codeCovers(pin.Text, pin.Entities, "vault · mac1") {
		t.Fatalf("want the pinned line sent once and pinned: %+v", cs)
	}
	a := r.answers(cs)[0]
	if a.KB == nil || !a.KB.IsPersistent || !a.KB.ResizeKeyboard || a.KB.InputFieldPlaceholder != "→ vault · mac1" {
		t.Fatalf("answer keyboard: %+v", a.KB)
	}
	b := a.KB.Keyboard[0][0]
	if b.Text != "🎯 vault · mac1 ▾" || b.WebApp == nil || !strings.HasPrefix(b.WebApp.URL, "https://agents.test/picker?t=") {
		t.Errorf("🎯 button: %+v", b)
	}
	for _, c := range cs {
		if c.Kind == "send" && c.KB == nil && c.MsgID != pin.MsgID {
			t.Errorf("every bot message but the pinned line carries the keyboard: %+v", c)
		}
	}
	if pin.KB != nil {
		t.Errorf("the pinned line must not carry a keyboard (Telegram can't edit it then)")
	}

	// Telegram's "pinned a message" service line comes back from the bot itself and is deleted.
	r.hub.intake(&models.Update{ID: 900, Message: &models.Message{ID: pin.MsgID + 1, From: &models.User{ID: 900000002, IsBot: true},
		Chat: models.Chat{ID: ownerUser, Type: models.ChatTypePrivate}, PinnedMessage: &models.MaybeInaccessibleMessage{}}})
	r.tg.waitFor(t, "service deleted", func(cs []call) bool {
		for _, c := range cs {
			if c.Kind == "delete" && c.MsgID == pin.MsgID+1 {
				return true
			}
		}
		return false
	})
	// A second message doesn't pin again.
	r.text("again")
	cs = r.tg.waitFor(t, "second answer", has("send", "answer to [again]"))
	if count(cs, "pin", "") != 1 {
		t.Errorf("pinned twice: %+v", cs)
	}
}

func TestPickerSwitchToSessionAndRecentBack(t *testing.T) {
	r := newRig(t)
	r.heartbeat("mac2", agtermSession{ID: mac2Sess, Name: "ev-test", Agent: "claude", State: "ready", Cwd: "/Users/tester/x"})
	r.text("/status") // creates the pin on vault
	r.tg.waitFor(t, "status", has("send", "hosts:"))

	id := r.webAppData(`{"v":1,"target":"session@mac2:` + mac2Sess + `"}`)
	cs := r.tg.waitFor(t, "switch", has("edit", "🎯 Talking to: ev-test · mac2"))
	sw := lastWith(cs, "send", "Now talking to")
	if !codeCovers(sw.Text, sw.Entities, "ev-test · mac2") {
		t.Errorf("label not in a code entity: %+v", sw)
	}
	if got := kbButtons(sw.KB); len(got) != 2 || got[0] != "🎯 ev-test · mac2 ▾" || got[1] != "vault · mac1" {
		t.Errorf("keyboard after switch: %v", got)
	}
	if sw.KB.InputFieldPlaceholder != "→ ev-test · mac2" {
		t.Errorf("placeholder %q", sw.KB.InputFieldPlaceholder)
	}
	if e := lastWith(cs, "edit", ""); e.Text != "🎯 Talking to: ev-test · mac2" || !codeCovers(e.Text, e.Entities, "ev-test · mac2") {
		t.Errorf("pinned line not edited: %+v", cs)
	}
	if !hasDelete(cs, id) {
		t.Errorf("web_app_data service message %d not deleted: %+v", id, cs)
	}
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "session@mac2:"+mac2Sess {
		t.Errorf("target %q", tgt)
	}

	// The recent button switches back to vault; the owner's button message is deleted.
	id = r.text("vault · mac1")
	cs = r.tg.waitFor(t, "back to vault", has("edit", "🎯 Talking to: vault · mac1"))
	sw = lastWith(cs, "send", "Now talking to vault")
	if got := kbButtons(sw.KB); len(got) != 2 || got[0] != "🎯 vault · mac1 ▾" || got[1] != "ev-test · mac2" {
		t.Errorf("keyboard after recent: %v", got)
	}
	if !hasDelete(cs, id) || lastWith(cs, "edit", "").Text != "🎯 Talking to: vault · mac1" {
		t.Errorf("button message deleted + pin edited: %+v", cs)
	}
	if n, _ := r.st.QueueDepth("mac1"); n != 0 {
		t.Errorf("a recent-button press must not be sent anywhere (queue %d)", n)
	}
}

func hasDelete(cs []call, id int) bool {
	for _, c := range cs {
		if c.Kind == "delete" && c.MsgID == id {
			return true
		}
	}
	return false
}

func TestRecentButtonForClosedSession(t *testing.T) {
	r := newRig(t)
	r.heartbeat("mac2", agtermSession{ID: mac2Sess, Name: "ev-test", Agent: "claude", State: "ready"})
	r.fakeConnected("mac2")
	r.webAppData(`{"v":1,"target":"session@mac2:` + mac2Sess + `"}`)
	r.tg.waitFor(t, "to session", has("send", "Now talking to ev-test · mac2"))
	r.text("vault · mac1")
	r.tg.waitFor(t, "to vault", has("send", "Now talking to vault · mac1"))

	r.heartbeat("mac2") // the session closed
	id := r.text("ev-test · mac2")
	cs := r.tg.waitFor(t, "gone", has("send", "is gone"))
	g := lastWith(cs, "send", "is gone")
	if !codeCovers(g.Text, g.Entities, "ev-test · mac2") || !hasDelete(cs, id) {
		t.Errorf("gone notice: %+v", g)
	}
	if got := kbButtons(g.KB); len(got) != 1 {
		t.Errorf("the dead session leaves the keyboard: %v", got)
	}
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "vault@mac1" {
		t.Errorf("target changed to %q", tgt)
	}
	// The picker refuses it too.
	r.webAppData(`{"v":1,"target":"session@mac2:` + mac2Sess + `"}`)
	r.tg.waitFor(t, "gone twice", func(cs []call) bool { return count(cs, "send", "is gone") == 2 })
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "vault@mac1" {
		t.Errorf("target changed to %q", tgt)
	}
}

func TestPickerVaultNewConversationAndBadChoices(t *testing.T) {
	r := newRig(t)
	r.startHost()
	r.text("remember me")
	r.tg.waitFor(t, "first", has("send", "answer to [remember me]"))
	r.webAppData(`{"v":1,"target":"vault@mac1","fresh":true}`)
	r.tg.waitFor(t, "fresh", has("send", "Now talking to vault · mac1 (new conversation)"))
	r.text("who am I?")
	cs := r.tg.waitFor(t, "fresh answer", has("send", "answer to [who am I?]"))
	if a := r.answers(cs)[1]; !strings.Contains(a.Text, "resume=none") {
		t.Errorf("new conversation must not resume: %s", a.Text)
	}
	r.webAppData(`{"v":1,"target":"vault@ghost"}`)
	r.webAppData(`not json`)
	r.tg.waitFor(t, "refusals", func(cs []call) bool {
		return count(cs, "send", `unknown host "ghost"`) == 1 && count(cs, "send", "unreadable choice") == 1
	})
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "vault@mac1" {
		t.Errorf("target %q", tgt)
	}
}

func TestLabelsNeverBecomeCommands(t *testing.T) {
	r := newRig(t)
	r.text("/status")
	r.tg.waitFor(t, "status", has("send", "hosts:"))
	r.text("/target p@mac1:~/x")
	cs := r.tg.waitFor(t, "switch", has("send", "Now talking to x · mac1"))
	sw := lastWith(cs, "send", "Now talking to")
	if !codeCovers(sw.Text, sw.Entities, "x · mac1") || !codeCovers(sw.Text, sw.Entities, "p@mac1:~/x") {
		t.Errorf("switch: %+v", sw)
	}
	// A session named like a path: the label is shown inside code everywhere.
	r.heartbeat("mac2", agtermSession{ID: mac2Sess, Name: "~/x", Agent: "claude"})
	r.webAppData(`{"v":1,"target":"session@mac2:` + mac2Sess + `"}`)
	cs = r.tg.waitFor(t, "path-like switch", has("edit", "Talking to: ~/x · mac2"))
	sw = lastWith(cs, "send", "Now talking to ~/x")
	pin := lastWith(cs, "edit", "~/x")
	if !codeCovers(sw.Text, sw.Entities, "~/x") || !codeCovers(pin.Text, pin.Entities, "~/x") {
		t.Errorf("~/x must sit in code entities: %+v / %+v", sw, pin)
	}
}

func TestPlaceholderTruncation(t *testing.T) {
	name := strings.Repeat("n", 90)
	p := placeholder(name + " · mac2")
	if u16len(p) > 64 || !strings.HasSuffix(p, "…") || !strings.HasPrefix(p, "→ nnn") {
		t.Errorf("placeholder %q (%d)", p, u16len(p))
	}
	if p := placeholder("vault · mac1"); p != "→ vault · mac1" {
		t.Errorf("short placeholder %q", p)
	}
	emoji := strings.Repeat("🚗", 40) // 2 UTF-16 units each
	if p := placeholder(emoji); u16len(p) > 64 {
		t.Errorf("emoji placeholder %d units", u16len(p))
	}
}

func TestReplyGoesToWriterTargetUnchanged(t *testing.T) {
	r := newRig(t)
	r.startHost()
	r.text("vault question")
	cs := r.tg.waitFor(t, "vault answer", has("send", "answer to [vault question]"))
	vaultAnswer := r.answers(cs)[0].MsgID

	briefs := filepath.Join(r.dir, "agent-briefs")
	os.MkdirAll(briefs, 0o755)
	r.text("/target p@mac1:" + briefs)
	r.tg.waitFor(t, "switch", has("edit", "Talking to: agent-briefs · mac1"))
	before := len(r.tg.snapshot())

	r.send(func(m *models.Message) {
		m.Text = "follow up"
		m.ReplyToMessage = &models.Message{ID: vaultAnswer, Chat: models.Chat{ID: ownerUser}}
	})
	cs = r.tg.waitFor(t, "reply answer", has("send", "answer to [follow up]"))
	if a := lastWith(cs, "send", "answer to [follow up]"); !strings.Contains(a.Text, "/vault resume=sess-") {
		t.Errorf("a reply to a vault answer goes to vault (same session): %s", a.Text)
	}
	for _, c := range cs[before:] {
		if strings.Contains(c.Text, "Now talking") || c.Kind == "edit" {
			t.Errorf("a reply must not switch the target: %+v", c)
		}
	}
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "p@mac1:"+briefs {
		t.Errorf("target changed to %q", tgt)
	}
}

func TestRouter(t *testing.T) {
	r := newRig(t)
	r.heartbeat("mac2", agtermSession{ID: mac2Sess, Name: "ev-test", Agent: "claude"})
	now := r.clk.Now()
	if err := r.st.SetChatTarget(ownerUser, "p@mac1:~/x", now); err != nil {
		t.Fatal(err)
	}
	for _, m := range []Message{
		{ChatID: ownerUser, MsgID: 50, Direction: "out", Kind: "answer", Target: "session@mac2:" + mac2Sess},
		{ChatID: ownerUser, MsgID: 51, Direction: "out", Kind: "answer", Target: "vault@mac1"},
		{ChatID: ownerUser, MsgID: 52, Direction: "out", Kind: "notice"},
		{ChatID: ownerUser, MsgID: 53, Direction: "in", Kind: "text"},
		{ChatID: ownerUser, MsgID: 54, Direction: "out", Kind: KindPlain},
		{ChatID: ownerUser, MsgID: 55, Direction: "out", Kind: "ping"},
	} {
		if err := r.st.AddMessage(m, now); err != nil {
			t.Fatal(err)
		}
	}
	chat := models.Chat{ID: ownerUser, Type: models.ChatTypePrivate}
	msg := func(text string, replyTo *models.Message) *models.Update {
		return &models.Update{Message: &models.Message{ID: 99, Chat: chat, Text: text, ReplyToMessage: replyTo}}
	}
	cb := func(data string) *models.Update {
		return &models.Update{CallbackQuery: &models.CallbackQuery{Data: data}}
	}
	react := func(id int) *models.Update {
		return &models.Update{MessageReaction: &models.MessageReactionUpdated{Chat: chat, MessageID: id}}
	}
	bot := &models.User{ID: testBot, IsBot: true}
	const unrouted = "(not delivered)"
	for _, c := range []struct {
		name   string
		u      *models.Update
		rule   int
		target string // "" = Ignore, unrouted = not delivered
	}{
		{"callback to a session's message", cb("ag:50:1"), RuleCallback, "session@mac2:" + mac2Sess},
		{"callback to vault's message", cb("ag:51:0"), RuleCallback, "vault@mac1"},
		{"callback to an unknown message", cb("ag:4242:0"), RuleCallback, ""},
		{"callback with other data", cb("cb_B"), RuleCallback, ""},
		{"reaction on a session's message", react(50), RuleReaction, "session@mac2:" + mac2Sess},
		{"reaction on a notice", react(52), RuleReaction, ""},
		{"reaction on an unknown message", react(4242), RuleReaction, ""},
		{"reply to a session's answer", msg("x", &models.Message{ID: 50}), RuleReply, "session@mac2:" + mac2Sess},
		{"reply to vault's answer", msg("x", &models.Message{ID: 51}), RuleReply, "vault@mac1"},
		{"reply via 🤖 header", msg("x", &models.Message{ID: 777, Text: "🤖 ev-test@mac2: dealer 3 asks…"}), RuleReply, "session@mac2:" + mac2Sess},
		{"reply via 🤖 name · host header", msg("x", &models.Message{ID: 780, Text: "🤖 ev-test · mac2\ndealer 3 asks…"}), RuleReply, "session@mac2:" + mac2Sess},
		{"reply via 🤖 vault header", msg("x", &models.Message{ID: 778, Text: "🤖 vault@mac1: hi"}), RuleReply, "vault@mac1"},
		{"reply via header of an unknown session", msg("x", &models.Message{ID: 779, Text: "🤖 nobody@mac2: hi"}), RuleReply, unrouted},
		{"reply to a bot message the hub never sent (1838)", msg("x", &models.Message{ID: 4243, From: bot, Text: "Claude needs you\nkill or wait?"}), RuleReply, unrouted},
		{"reply to a plain agent message without a session", msg("x", &models.Message{ID: 54, From: bot}), RuleReply, unrouted},
		{"reply to an agent ping without a session", msg("x", &models.Message{ID: 55, From: bot}), RuleReply, unrouted},
		{"reply to a notice", msg("x", &models.Message{ID: 52}), RuleCurrent, "p@mac1:~/x"},
		{"reply to an old message of the owner's", msg("x", &models.Message{ID: 4244, From: &models.User{ID: ownerUser}}), RuleCurrent, "p@mac1:~/x"},
		{"reply to own message", msg("x", &models.Message{ID: 53}), RuleCurrent, "p@mac1:~/x"},
		{"plain message", msg("x", nil), RuleCurrent, "p@mac1:~/x"},
	} {
		got, err := r.hub.route(ownerUser, c.u)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.target == unrouted {
			if got.Rule != c.rule || !got.Unrouted || got.Ignore {
				t.Errorf("%s: got %+v, want rule %d not delivered", c.name, got, c.rule)
			}
			continue
		}
		if got.Unrouted || got.Rule != c.rule || (c.target == "") != got.Ignore || (c.target != "" && got.Target.String() != c.target) {
			t.Errorf("%s: got rule %d %s ignore=%v, want rule %d %q", c.name, got.Rule, got.Target, got.Ignore, c.rule, c.target)
		}
	}
}

// ---- Mini App ----

func (r *rig) pickerURLToken() string {
	r.t.Helper()
	kb, _ := r.hub.markup(ownerUser).(*models.ReplyKeyboardMarkup)
	u, err := url.Parse(kb.Keyboard[0][0].WebApp.URL)
	if err != nil {
		r.t.Fatal(err)
	}
	return u.Query().Get("t")
}

func (r *rig) api(path string, hdr map[string]string) (int, map[string]any) {
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.hub.MiniAppHandler().ServeHTTP(rec, req)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestPickerTokenAuth(t *testing.T) {
	r := newRig(t)
	r.heartbeat("mac2", agtermSession{ID: mac2Sess, Name: "ev-test", Agent: "claude", State: "menu", Cwd: "/Users/tester/x"},
		agtermSession{ID: "SHELL", Name: "zsh"})
	tok := r.pickerURLToken()
	code, body := r.api("/api/picker", map[string]string{"X-Picker-Token": tok})
	if code != 200 {
		t.Fatalf("valid token: %d %v", code, body)
	}
	raw, _ := json.Marshal(body)
	var p pickerResp
	json.Unmarshal(raw, &p)
	if p.Current.Target != "vault@mac1" || p.Vault.Label != "vault · mac1" || len(p.Hosts) != 2 {
		t.Errorf("picker data: %s", raw)
	}
	g := p.Hosts[1]
	if g.Name != "mac2" || !g.Agterm || len(g.Sessions) != 1 || g.Sessions[0].State != "menu" ||
		g.Sessions[0].Target != "session@mac2:"+mac2Sess || len(g.Folders) != 1 {
		t.Errorf("mac2: %+v", g)
	}

	// The same token rides on every message until the target changes.
	if again := r.pickerURLToken(); again != tok {
		t.Errorf("token changed without a switch")
	}
	tampered := tok[:len(tok)-2] + "xx"
	forged := strings.Replace(tok, fmt.Sprint(ownerUser), "42", 1)
	for name, bad := range map[string]string{"tampered": tampered, "other chat": forged, "garbage": "abc", "empty": ""} {
		if code, _ := r.api("/api/picker", map[string]string{"X-Picker-Token": bad}); code != 401 {
			t.Errorf("%s token: %d", name, code)
		}
	}
	// A switch supersedes it.
	r.text("/target vault@mac2")
	r.tg.waitFor(t, "switch", has("send", "Now talking to vault · mac2"))
	if code, body := r.api("/api/picker", map[string]string{"X-Picker-Token": tok}); code != 401 || body["reason"] != "superseded" {
		t.Errorf("superseded token: %d %v", code, body)
	}
	fresh := r.pickerURLToken()
	if code, _ := r.api("/api/picker", map[string]string{"X-Picker-Token": fresh}); code != 200 {
		t.Errorf("fresh token: %d", code)
	}
	// Reissued before it ages out; the hard cap is 7 days.
	r.clk.Add(6*24*time.Hour + time.Minute)
	reissued := r.pickerURLToken()
	if reissued == fresh {
		t.Errorf("token not reissued after 6 days")
	}
	r.clk.Add(24 * time.Hour)
	if code, _ := r.api("/api/picker", map[string]string{"X-Picker-Token": reissued}); code != 200 {
		t.Errorf("reissued token: %d", code)
	}
	k, _ := r.hub.keyboardState(ownerUser, false)
	old := r.hub.pickerToken(ownerUser, kbState{Serial: k.Serial, Issued: r.clk.Now().Add(-7 * 24 * time.Hour).Unix()})
	if code, body := r.api("/api/picker", map[string]string{"X-Picker-Token": old}); code != 401 || body["reason"] != "expired" {
		t.Errorf("7-day-old token: %d %v", code, body)
	}
	// The board needs initData; the picker token doesn't open it.
	if code, _ := r.api("/api/board", map[string]string{"X-Picker-Token": reissued}); code != 401 {
		t.Errorf("board with a picker token: %d", code)
	}
}

// signInitData builds initData the way Telegram signs it.
func signInitData(token string, vals url.Values) string {
	var keys []string
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		lines = append(lines, k+"="+vals.Get(k))
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(token))
	m := hmac.New(sha256.New, secret.Sum(nil))
	m.Write([]byte(strings.Join(lines, "\n")))
	vals.Set("hash", hex.EncodeToString(m.Sum(nil)))
	return vals.Encode()
}

func TestInitDataAuth(t *testing.T) {
	r := newRig(t)
	token := testConfig().TelegramToken
	now := r.clk.Now()
	initData := func(user int64, at time.Time) string {
		return signInitData(token, url.Values{"auth_date": {fmt.Sprint(at.Unix())}, "query_id": {"AAH"},
			"signature": {"sig"}, "user": {fmt.Sprintf(`{"id":%d,"first_name":"Owner"}`, user)}})
	}
	good := initData(ownerUser, now.Add(-10*time.Minute))
	if code, body := r.api("/api/board", map[string]string{"X-Telegram-Init-Data": good}); code != 200 || body["hosts"] == nil {
		t.Fatalf("valid initData: %d %v", code, body)
	}
	if code, _ := r.api("/api/picker", map[string]string{"X-Telegram-Init-Data": good}); code != 200 {
		t.Errorf("initData opens the picker API too: %d", code)
	}
	tampered := strings.Replace(good, "Owner", "Eve", 1)
	for name, bad := range map[string]string{
		"tampered":    tampered,
		"2 h old":     initData(ownerUser, now.Add(-2*time.Hour)),
		"other user":  initData(42, now),
		"other token": signInitData("999:other", url.Values{"auth_date": {fmt.Sprint(now.Unix())}, "user": {`{"id":1001}`}}),
		"no hash":     "auth_date=1&user=%7B%7D",
	} {
		if code, _ := r.api("/api/board", map[string]string{"X-Telegram-Init-Data": bad}); code != 401 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _ := r.api("/api/board", nil); code != 401 {
		t.Errorf("no credentials: %d", code)
	}
}

func TestMiniAppRateLimitAndSurface(t *testing.T) {
	r := newRig(t)
	tok := r.pickerURLToken()
	for i := 1; i <= 31; i++ {
		code, _ := r.api("/api/picker", map[string]string{"X-Picker-Token": tok})
		if i <= 30 && code != 200 || i == 31 && code != 429 {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	r.clk.Add(time.Minute)
	if code, _ := r.api("/api/picker", map[string]string{"X-Picker-Token": tok}); code != 200 {
		t.Errorf("after a minute: %d", code)
	}
	// Bad callers are limited per address, not on the owner's budget.
	for i := 0; i < 31; i++ {
		r.api("/api/picker", map[string]string{"X-Picker-Token": "bad", "CF-Connecting-IP": "203.0.113.9"})
	}
	if code, _ := r.api("/api/picker", map[string]string{"X-Picker-Token": "bad", "CF-Connecting-IP": "203.0.113.9"}); code != 429 {
		t.Errorf("bad caller: %d", code)
	}
	if code, _ := r.api("/api/picker", map[string]string{"X-Picker-Token": tok}); code != 200 {
		t.Errorf("the owner after bad caller: %d", code)
	}

	h := r.hub.MiniAppHandler()
	for path, want := range map[string]int{"/picker": 200, "/board": 200, "/static/picker.js": 200, "/static/app.css": 200, "/ws": 404, "/healthz": 404} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
		if rec.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: no Referrer-Policy", path)
		}
	}
}

func TestMenuButtonPointsAtBoard(t *testing.T) {
	r := newRig(t)
	r.hub.setOwner(r.hub.ownerRow()) // as after pairing: the bot is set up again
	r.hub.refreshTelegram(context.Background())
	cs := r.tg.snapshot()
	if count(cs, "commands", "") != 1 {
		t.Errorf("commands not set: %+v", cs)
	}
	if c := lastWith(cs, "menu", ""); c.Text != "Agents https://agents.test/board" {
		t.Errorf("menu button: %+v", c)
	}
}
