package faketg

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

const testToken = "1000001:fake-test-token"

// The shapes the source hub's Bot API client (go-telegram/bot models) parses.
type tUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type tChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type tMessage struct {
	ID       int       `json:"message_id"`
	From     *tUser    `json:"from"`
	Chat     tChat     `json:"chat"`
	Date     int       `json:"date"`
	EditDate int       `json:"edit_date"`
	Text     string    `json:"text"`
	Entities []any     `json:"entities"`
	Caption  string    `json:"caption"`
	Reply    *tMessage `json:"reply_to_message"`
	Pinned   *tMessage `json:"pinned_message"`
	Voice    *struct {
		FileID   string `json:"file_id"`
		UniqueID string `json:"file_unique_id"`
		Duration int    `json:"duration"`
		MimeType string `json:"mime_type"`
		FileSize int64  `json:"file_size"`
	} `json:"voice"`
	Markup *struct {
		Inline [][]struct {
			Text string `json:"text"`
			Data string `json:"callback_data"`
		} `json:"inline_keyboard"`
	} `json:"reply_markup"`
}

type tUpdate struct {
	ID       int64     `json:"update_id"`
	Message  *tMessage `json:"message"`
	Reaction *struct {
		Chat      tChat  `json:"chat"`
		MessageID int    `json:"message_id"`
		User      *tUser `json:"user"`
		New       []struct {
			Type  string `json:"type"`
			Emoji string `json:"emoji"`
		} `json:"new_reaction"`
	} `json:"message_reaction"`
	Callback *struct {
		ID      string    `json:"id"`
		From    tUser     `json:"from"`
		Message *tMessage `json:"message"`
		Data    string    `json:"data"`
	} `json:"callback_query"`
	Stopped *struct {
		Chat    tChat  `json:"chat"`
		DraftID string `json:"draft_id"`
	} `json:"stopped_message_generation"`
}

type apiResp struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	status      int
}

type harness struct {
	t   *testing.T
	s   *Server
	srv *httptest.Server
	// fire ends every pending After(); tests use it to end timeouts without waiting for the clock.
	fire chan time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, fire: make(chan time.Time)}
	h.s = New(Options{Token: testToken, After: func(time.Duration) <-chan time.Time { return h.fire }})
	h.srv = httptest.NewServer(h.s)
	t.Cleanup(h.srv.Close)
	return h
}

func decode(t *testing.T, resp *http.Response) apiResp {
	t.Helper()
	defer resp.Body.Close()
	var r apiResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	r.status = resp.StatusCode
	return r
}

// form calls a method the way go-telegram/bot does: multipart, structured values as JSON text.
func (h *harness) form(method string, fields map[string]any, file []byte) apiResp {
	h.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		s, ok := v.(string)
		if !ok {
			b, _ := json.Marshal(v)
			s = string(b)
		}
		_ = w.WriteField(k, s)
	}
	if file != nil {
		fw, _ := w.CreateFormFile("voice", "reply.ogg")
		_, _ = fw.Write(file)
	}
	_ = w.Close()
	resp, err := http.Post(h.srv.URL+"/bot"+testToken+"/"+method, w.FormDataContentType(), &body)
	if err != nil {
		h.t.Fatal(err)
	}
	return decode(h.t, resp)
}

// jsonCall calls a method with a JSON body (the hub's getUpdates does).
func (h *harness) jsonCall(method string, body any) apiResp {
	h.t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(h.srv.URL+"/bot"+testToken+"/"+method, "application/json", bytes.NewReader(b))
	if err != nil {
		h.t.Fatal(err)
	}
	return decode(h.t, resp)
}

func (h *harness) updates(offset int64, allowed []string) []tUpdate {
	h.t.Helper()
	r := h.jsonCall("getUpdates", map[string]any{"offset": offset, "timeout": 0, "allowed_updates": allowed})
	if !r.OK {
		h.t.Fatalf("getUpdates: %d %s", r.ErrorCode, r.Description)
	}
	var u []tUpdate
	if err := json.Unmarshal(r.Result, &u); err != nil {
		h.t.Fatal(err)
	}
	return u
}

func (h *harness) cli(file []byte, args ...string) (Envelope, map[string]any, int) {
	h.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	a, _ := json.Marshal(args)
	_ = w.WriteField("args", string(a))
	if file != nil {
		fw, _ := w.CreateFormFile("file", "voice.ogg")
		_, _ = fw.Write(file)
	}
	_ = w.Close()
	resp, err := http.Post(h.srv.URL+"/_fake/cli", w.FormDataContentType(), &body)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		h.t.Fatalf("cli %v: %v: %s", args, err, raw)
	}
	var data map[string]any
	if env.OK {
		b, _ := json.Marshal(env.Data)
		_ = json.Unmarshal(b, &data)
	}
	exit := 0
	_, _ = fmt.Sscan(resp.Header.Get("X-Exit-Code"), &exit)
	return env, data, exit
}

// waitFor blocks until cond holds (checked on every state change); the deadline only guards a hung test.
func (h *harness) waitFor(cond func() bool) {
	h.t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		h.s.mu.Lock()
		ok := cond()
		ch := h.s.changed
		h.s.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-ch:
		case <-deadline.C:
			h.t.Fatal("condition never held")
		}
	}
}

func msgOf(t *testing.T, r apiResp) tMessage {
	t.Helper()
	if !r.OK {
		t.Fatalf("call failed: %d %s", r.ErrorCode, r.Description)
	}
	var m tMessage
	if err := json.Unmarshal(r.Result, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func isTrue(t *testing.T, r apiResp) {
	t.Helper()
	if !r.OK || string(r.Result) != "true" {
		t.Fatalf("want ok true, got %d %s %s", r.ErrorCode, r.Description, r.Result)
	}
}

func TestGetMeAndAuth(t *testing.T) {
	h := newHarness(t)
	var me tUser
	r := h.form("getMe", nil, nil)
	if err := json.Unmarshal(r.Result, &me); err != nil || !me.IsBot || me.Username != BotUsername || me.ID != 1000001 {
		t.Fatalf("getMe: %+v %v", me, err)
	}
	resp, _ := http.Post(h.srv.URL+"/botwrong:token/getMe", "application/json", nil)
	if r := decode(t, resp); r.status != 401 || r.ErrorCode != 401 {
		t.Fatalf("wrong token: %+v", r)
	}
	if r := h.form("noSuchMethod", nil, nil); r.status != 404 || r.OK {
		t.Fatalf("unknown method: %+v", r)
	}
	if r := h.form("sendMessage", map[string]any{"chat_id": 42, "text": "x"}, nil); r.status != 400 ||
		r.Description != "Bad Request: chat not found" {
		t.Fatalf("unknown chat: %+v", r)
	}
}

func TestTextRoundTrip(t *testing.T) {
	h := newHarness(t)
	env, data, exit := h.cli(nil, "send", "text", "hello", "--from", "owner")
	if !env.OK || exit != 0 {
		t.Fatalf("send: %+v", env)
	}
	if sent := data["sent"].(map[string]any); sent["text"] != "hello" || sent["from"] != "me" {
		t.Fatalf("sent: %v", sent)
	}
	u := h.updates(0, []string{"message", "callback_query", "message_reaction", "stopped_message_generation"})
	if len(u) != 1 || u[0].Message == nil {
		t.Fatalf("updates: %+v", u)
	}
	m := u[0].Message
	if m.From.ID != OwnerID || m.Chat.Type != "private" || m.Chat.ID != OwnerID || m.Text != "hello" || m.From.IsBot {
		t.Fatalf("update message: %+v", m)
	}
	if again := h.updates(u[0].ID+1, nil); len(again) != 0 {
		t.Fatalf("confirmed update came back: %+v", again)
	}
	// The bot answers with a urlencoded call, as curl -d does.
	resp, _ := http.PostForm(h.srv.URL+"/bot"+testToken+"/sendMessage", url.Values{"chat_id": {"1001"}, "text": {"pong"}})
	reply := msgOf(t, decode(t, resp))
	if reply.From == nil || !reply.From.IsBot || reply.Text != "pong" || reply.ID != m.ID+1 {
		t.Fatalf("reply: %+v", reply)
	}
	_, data, _ = h.cli(nil, "read", "messages", "--limit", "2")
	msgs := data["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if len(msgs) != 2 || last["text"] != "pong" || last["from"] != "bot" {
		t.Fatalf("read: %v", msgs)
	}
	// The other user has a chat of their own, with its own message ids.
	h.cli(nil, "send", "text", "/start", "--from", "other")
	u = h.updates(u[0].ID+1, nil)
	if len(u) != 1 || u[0].Message.From.ID != OtherID || u[0].Message.ID != 1 || len(u[0].Message.Entities) != 1 {
		t.Fatalf("other user: %+v", u)
	}
}

func TestSendMessageContract(t *testing.T) {
	h := newHarness(t)
	h.cli(nil, "send", "text", "question")
	kb := map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "Yes", "callback_data": "y"}, {"text": "No", "callback_data": "n"}}}}
	m := msgOf(t, h.form("sendMessage", map[string]any{"chat_id": OwnerID, "text": "Pick one",
		"entities":         []map[string]any{{"type": "code", "offset": 0, "length": 4}},
		"reply_parameters": map[string]any{"message_id": 1, "allow_sending_without_reply": true},
		"reply_markup":     kb, "link_preview_options": map[string]any{"is_disabled": true}}, nil))
	if m.Reply == nil || m.Reply.ID != 1 || m.Markup == nil || len(m.Markup.Inline[0]) != 2 || len(m.Entities) != 1 {
		t.Fatalf("sendMessage result: %+v", m)
	}
	if r := h.form("sendMessage", map[string]any{"chat_id": OwnerID, "text": "x",
		"reply_parameters": map[string]any{"message_id": 99}}, nil); r.Description != "Bad Request: message to be replied not found" {
		t.Fatalf("missing reply target: %+v", r)
	}
	if r := h.form("sendMessage", map[string]any{"chat_id": OwnerID, "text": " "}, nil); r.Description != "Bad Request: message text is empty" {
		t.Fatalf("empty text: %+v", r)
	}
	_, data, _ := h.cli(nil, "read", "messages")
	last := data["messages"].([]any)[1].(map[string]any)
	ents := last["entities"].([]any)[0].(map[string]any)
	if ents["text"] != "Pick" || len(last["inline_keyboard"].([]any)[0].([]any)) != 2 {
		t.Fatalf("client view: %v", last)
	}
}

func TestEditDeleteReactPin(t *testing.T) {
	h := newHarness(t)
	m := msgOf(t, h.form("sendMessage", map[string]any{"chat_id": OwnerID, "text": "v1"}, nil))
	e := msgOf(t, h.form("editMessageText", map[string]any{"chat_id": OwnerID, "message_id": m.ID, "text": "v2"}, nil))
	if e.Text != "v2" || e.EditDate == 0 {
		t.Fatalf("edit: %+v", e)
	}
	if r := h.form("editMessageText", map[string]any{"chat_id": OwnerID, "message_id": m.ID, "text": "v2"}, nil); r.status != 400 ||
		!strings.HasPrefix(r.Description, "Bad Request: message is not modified") {
		t.Fatalf("not modified: %+v", r)
	}
	kb := map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "Go", "callback_data": "go"}}}}
	if e := msgOf(t, h.form("editMessageReplyMarkup", map[string]any{"chat_id": OwnerID, "message_id": m.ID, "reply_markup": kb}, nil)); e.Markup == nil {
		t.Fatalf("editMessageReplyMarkup: %+v", e)
	}
	rk := msgOf(t, h.form("sendMessage", map[string]any{"chat_id": OwnerID, "text": "menu",
		"reply_markup": map[string]any{"keyboard": [][]map[string]string{{{"text": "Status"}}}, "is_persistent": true}}, nil))
	if r := h.form("editMessageText", map[string]any{"chat_id": OwnerID, "message_id": rk.ID, "text": "x"}, nil); r.Description != "Bad Request: message can't be edited" {
		t.Fatalf("edit of a reply-keyboard message: %+v", r)
	}
	_, data, _ := h.cli(nil, "read", "keyboard")
	if k := data["keyboard"].(map[string]any); k["message_id"] != float64(rk.ID) {
		t.Fatalf("keyboard: %v", data)
	}
	isTrue(t, h.form("setMessageReaction", map[string]any{"chat_id": OwnerID, "message_id": m.ID,
		"reaction": []map[string]string{{"type": "emoji", "emoji": "👀"}}}, nil))
	isTrue(t, h.form("sendChatAction", map[string]any{"chat_id": OwnerID, "action": "typing"}, nil))
	isTrue(t, h.form("pinChatMessage", map[string]any{"chat_id": OwnerID, "message_id": m.ID, "disable_notification": true}, nil))
	u := h.updates(0, nil)
	if len(u) != 1 || u[0].Message.Pinned == nil || u[0].Message.Pinned.ID != m.ID || !u[0].Message.From.IsBot {
		t.Fatalf("pin service update: %+v", u)
	}
	_, data, _ = h.cli(nil, "read", "pinned")
	p := data["pinned"].(map[string]any)
	if p["text"] != "v2" || p["reactions"].([]any)[0].(map[string]any)["emoji"] != "👀" {
		t.Fatalf("pinned: %v", p)
	}
	isTrue(t, h.form("unpinChatMessage", map[string]any{"chat_id": OwnerID, "message_id": m.ID}, nil))
	isTrue(t, h.form("deleteMessage", map[string]any{"chat_id": OwnerID, "message_id": m.ID}, nil))
	if r := h.form("editMessageText", map[string]any{"chat_id": OwnerID, "message_id": m.ID, "text": "v3"}, nil); r.Description != "Bad Request: message to edit not found" {
		t.Fatalf("edit after delete: %+v", r)
	}
	isTrue(t, h.form("setMyCommands", map[string]any{"commands": []map[string]string{{"command": "new", "description": "fresh"}}}, nil))
	isTrue(t, h.form("setChatMenuButton", map[string]any{"chat_id": OwnerID,
		"menu_button": map[string]any{"type": "web_app", "text": "Open", "web_app": map[string]string{"url": "https://example.test"}}}, nil))
}

func TestVoiceRoundTrip(t *testing.T) {
	h := newHarness(t)
	ogg, err := os.ReadFile("../../../e2e/fixtures/audio/hello.ogg")
	if err != nil {
		t.Fatal(err)
	}
	if env, _, _ := h.cli(ogg, "send", "voice", "hello.ogg"); !env.OK {
		t.Fatalf("send voice: %+v", env.Error)
	}
	u := h.updates(0, nil)
	v := u[0].Message.Voice
	if v == nil || v.MimeType != "audio/ogg" || v.Duration < 1 || v.FileSize != int64(len(ogg)) {
		t.Fatalf("voice: %+v", u[0].Message)
	}
	r := h.form("getFile", map[string]any{"file_id": v.FileID}, nil)
	var f struct {
		Path string `json:"file_path"`
	}
	_ = json.Unmarshal(r.Result, &f)
	resp, err := http.Get(h.srv.URL + "/file/bot" + testToken + "/" + f.Path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if sha256.Sum256(got) != sha256.Sum256(ogg) {
		t.Fatal("downloaded voice differs from the upload")
	}
	if r := h.form("getFile", map[string]any{"file_id": "nope"}, nil); r.Description != "Bad Request: invalid file_id" {
		t.Fatalf("bad file id: %+v", r)
	}
	sent := msgOf(t, h.form("sendVoice", map[string]any{"chat_id": OwnerID}, ogg))
	if sent.Voice == nil || sent.Voice.Duration < 1 || !sent.From.IsBot {
		t.Fatalf("sendVoice: %+v", sent)
	}
	if env, _, exit := h.cli([]byte("not ogg"), "send", "voice", "x.ogg"); env.OK || exit != 2 {
		t.Fatalf("non-ogg voice accepted: %+v", env)
	}
}

func TestLongPollAndConflict(t *testing.T) {
	h := newHarness(t)
	first := make(chan apiResp, 1)
	go func() { first <- h.jsonCall("getUpdates", map[string]any{"offset": 0, "timeout": 50}) }()
	h.waitFor(func() bool { return h.s.poll != nil })
	second := make(chan apiResp, 1)
	go func() { second <- h.jsonCall("getUpdates", map[string]any{"offset": 0, "timeout": 50}) }()
	if r := <-first; r.status != 409 || r.ErrorCode != 409 {
		t.Fatalf("first poll: %+v", r)
	}
	h.waitFor(func() bool { return h.s.poll != nil })
	h.cli(nil, "send", "text", "wake")
	r := <-second
	var u []tUpdate
	_ = json.Unmarshal(r.Result, &u)
	if len(u) != 1 || u[0].Message.Text != "wake" {
		t.Fatalf("long poll: %+v", r)
	}
	// A poll with nothing pending ends empty when its timeout fires.
	third := make(chan apiResp, 1)
	go func() { third <- h.jsonCall("getUpdates", map[string]any{"offset": u[0].ID + 1, "timeout": 50}) }()
	h.waitFor(func() bool { return h.s.poll != nil })
	h.fire <- time.Time{}
	if r := <-third; !r.OK || string(r.Result) != "[]" {
		t.Fatalf("timed-out poll: %+v %s", r, r.Result)
	}
}

func TestReactionsAreOptIn(t *testing.T) {
	h := newHarness(t)
	m := msgOf(t, h.form("sendMessage", map[string]any{"chat_id": OwnerID, "text": "react to me"}, nil))
	h.cli(nil, "react", "--msg", fmt.Sprint(m.ID), "--emoji", "👍")
	if u := h.updates(0, nil); len(u) != 0 {
		t.Fatalf("reaction delivered without allowed_updates: %+v", u)
	}
	h.updates(0, []string{"message", "message_reaction"})
	h.cli(nil, "react", "--msg", fmt.Sprint(m.ID), "--emoji", "❤️")
	u := h.updates(0, nil)
	if len(u) != 1 || u[0].Reaction == nil || u[0].Reaction.User.ID != OwnerID || u[0].Reaction.New[0].Emoji != "❤" {
		t.Fatalf("reaction update: %+v", u)
	}
}

func TestPressAndAnswer(t *testing.T) {
	h := newHarness(t)
	kb := map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "Yes", "callback_data": "ans:y"}}}}
	m := msgOf(t, h.form("sendMessage", map[string]any{"chat_id": OwnerID, "text": "ok?", "reply_markup": kb}, nil))
	type out struct {
		data map[string]any
		exit int
	}
	done := make(chan out, 1)
	go func() {
		_, d, e := h.cli(nil, "press", "--msg", fmt.Sprint(m.ID), "--button", "Yes")
		done <- out{d, e}
	}()
	h.waitFor(func() bool { return len(h.s.updates) > 0 })
	u := h.updates(0, nil)
	cb := u[0].Callback
	if cb == nil || cb.Data != "ans:y" || cb.From.ID != OwnerID || cb.Message.ID != m.ID {
		t.Fatalf("callback update: %+v", u)
	}
	isTrue(t, h.form("answerCallbackQuery", map[string]any{"callback_query_id": cb.ID, "text": "done"}, nil))
	res := <-done
	if a := res.data["answer"].(map[string]any); res.exit != 0 || a["answered"] != true || a["message"] != "done" {
		t.Fatalf("press: %+v", res)
	}
	if r := h.form("answerCallbackQuery", map[string]any{"callback_query_id": cb.ID}, nil); r.status != 400 {
		t.Fatalf("second answer accepted: %+v", r)
	}
}

func TestDraftsAndStop(t *testing.T) {
	h := newHarness(t)
	done := make(chan map[string]any, 1)
	go func() {
		_, d, _ := h.cli(nil, "wait", "--for", "draft", "--all")
		done <- d
	}()
	h.waitFor(func() bool { return h.s.watching > 0 })
	isTrue(t, h.form("sendMessageDraft", map[string]any{"chat_id": OwnerID, "draft_id": "77", "text": "thinking", "can_stop": true}, nil))
	msgOf(t, h.form("sendMessage", map[string]any{"chat_id": OwnerID, "text": "final"}, nil))
	d := <-done
	w := d["wait"].(map[string]any)
	if ds := w["drafts"].([]any); len(ds) != 1 || ds[0].(map[string]any)["draft_id"] != float64(77) ||
		w["message"].(map[string]any)["text"] != "final" {
		t.Fatalf("draft wait: %v", w)
	}
	h.cli(nil, "stop", "--draft", "77")
	u := h.updates(0, nil)
	if len(u) != 1 || u[0].Stopped == nil || u[0].Stopped.DraftID != "77" || u[0].Stopped.Chat.ID != OwnerID {
		t.Fatalf("stop update: %+v", u)
	}
}

func TestSendWaitReplyAndTimeout(t *testing.T) {
	h := newHarness(t)
	done := make(chan map[string]any, 1)
	go func() {
		_, d, _ := h.cli(nil, "send", "text", "ping", "--wait", "reply")
		done <- d
	}()
	h.waitFor(func() bool { return len(h.s.updates) > 0 })
	msgOf(t, h.form("sendMessage", map[string]any{"chat_id": OwnerID, "text": "pong"}, nil))
	d := <-done
	if w := d["wait"].(map[string]any); w["for"] != "reply" || w["message"].(map[string]any)["text"] != "pong" {
		t.Fatalf("send --wait reply: %v", d)
	}
	type res struct {
		env  Envelope
		exit int
	}
	tc := make(chan res, 1)
	go func() {
		env, _, exit := h.cli(nil, "wait", "--for", "reply", "--timeout", "3")
		tc <- res{env, exit}
	}()
	h.fire <- time.Time{}
	r := <-tc
	if r.env.OK || r.exit != 9 || r.env.Error.Code != "timeout" {
		t.Fatalf("timeout: %+v", r)
	}
}

func TestFaultAndCallLog(t *testing.T) {
	h := newHarness(t)
	if env, _, _ := h.cli(nil, "fault", "sendMessage"); !env.OK {
		t.Fatalf("fault: %+v", env.Error)
	}
	if r := h.form("sendMessage", map[string]any{"chat_id": OwnerID, "text": "a"}, nil); r.status != 500 || r.OK {
		t.Fatalf("faulted call: %+v", r)
	}
	msgOf(t, h.form("sendMessage", map[string]any{"chat_id": OwnerID, "text": "b"}, nil))
	_, d, _ := h.cli(nil, "calls", "--method", "sendMessage")
	calls := d["calls"].([]any)
	if len(calls) != 2 || calls[0].(map[string]any)["status"] != float64(500) || calls[1].(map[string]any)["status"] != float64(200) {
		t.Fatalf("calls: %v", calls)
	}
	if env, _, exit := h.cli(nil, "read", "nonsense"); env.OK || exit != 2 {
		t.Fatalf("usage error: %+v %d", env, exit)
	}
	if env, _, exit := h.cli(nil, "react", "--msg", "999", "--emoji", "👍"); env.OK || exit != 4 {
		t.Fatalf("not found: %+v %d", env, exit)
	}
}

func TestInitDataSignature(t *testing.T) {
	h := newHarness(t)
	resp, err := http.Get(h.srv.URL + "/_fake/initdata?user=other")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	v, err := url.ParseQuery(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	for k := range v {
		fields[k] = v.Get(k)
	}
	if fields["hash"] == "" || SignInitData(testToken, fields) != fields["hash"] || SignInitData("other:token", fields) == fields["hash"] {
		t.Fatal("initData hash does not verify against the token")
	}
	var u struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal([]byte(fields["user"]), &u); u.ID != OtherID {
		t.Fatalf("initData user: %s", fields["user"])
	}
}
