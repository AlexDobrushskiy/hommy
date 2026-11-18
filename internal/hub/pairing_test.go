package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/pairing"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
	"github.com/AlexDobrushskiy/hommy/internal/speech"
	"github.com/AlexDobrushskiy/hommy/internal/version"
)

const (
	tess  = int64(2002) // pairs with the code
	other = int64(3003) // someone else who found the bot
)

// client is `hommy pair`'s view of the rig's hub (over loopback).
func (r *rig) client() pairing.Client {
	r.t.Helper()
	base, err := pairing.BaseURL(r.url)
	if err != nil {
		r.t.Fatal(err)
	}
	return pairing.Client{BaseURL: base, HostName: "mac1", Key: hostKey}
}

func (r *rig) newCode() pairing.Code {
	r.t.Helper()
	c, err := r.client().New(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

// from feeds a private-chat text from user; returns the message id.
func (r *rig) from(user int64, text string) int {
	r.t.Helper()
	return r.update(&models.Message{From: &models.User{ID: user, FirstName: "Tess"},
		Chat: models.Chat{ID: user, Type: models.ChatTypePrivate}, Text: text})
}

func (r *rig) update(m *models.Message) int {
	r.t.Helper()
	r.nextMs++
	m.ID = r.nextMs
	if err := r.hub.intake(&models.Update{ID: int64(r.nextMs), Message: m}); err != nil {
		r.t.Fatal(err)
	}
	return m.ID
}

func (r *rig) owner() Owner {
	r.t.Helper()
	o, err := r.st.Owner()
	if err != nil {
		r.t.Fatal(err)
	}
	if c := r.hub.ownerRow(); o.UserID != c.UserID || o.ChatID != c.ChatID || o.FirstName != c.FirstName {
		r.t.Fatalf("stored owner %+v, cached %+v", o, c)
	}
	return o
}

// wrong is a well-formed code that is not c.
func wrong(c pairing.Code, i int) string {
	s := []byte(strings.ReplaceAll(c.Code, "-", ""))
	s[i%len(s)] = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"[(strings.IndexByte("0123456789ABCDEFGHJKMNPQRSTVWXYZ", s[i%len(s)])+1)%32]
	return string(s)
}

func TestPairingBindsOwner(t *testing.T) {
	r := newUnpairedRig(t)
	lt := captureLog(t)
	c := r.client()
	code := r.newCode()
	if !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`).MatchString(code.Code) {
		t.Errorf("code %q", code.Code)
	}
	if code.Bot != "@"+testBotName || code.Link != "https://t.me/"+testBotName+"?start="+code.Code {
		t.Errorf("bot %q link %q", code.Bot, code.Link)
	}
	if !code.ExpiresAt.Equal(r.clk.Now().Add(30 * time.Minute)) {
		t.Errorf("expires %s", code.ExpiresAt)
	}
	if st, err := c.Status(context.Background()); err != nil || st.Paired || !st.Pending || st.ExpiresAt == nil {
		t.Fatalf("before: %+v %v", st, err)
	}

	// Typed lowercase and without the dash.
	id := r.from(tess, "/start "+strings.ToLower(strings.ReplaceAll(code.Code, "-", "")))
	cs := r.tg.waitFor(t, "welcome", has("send", "✅ Paired with @"+testBotName+". Send a text or a voice note."))
	w := lastWith(cs, "send", "✅ Paired")
	if w.ReplyTo != id || w.KB == nil || !strings.Contains(w.Text, "/new_session") {
		t.Errorf("welcome: %+v", w)
	}
	if count(cs, "commands", "") != 1 || count(cs, "menu", "Agents https://agents.test/board") != 1 {
		t.Errorf("commands / menu button not set: %+v", cs)
	}
	if o := r.owner(); o.UserID != tess || o.ChatID != tess || o.FirstName != "Tess" {
		t.Errorf("owner %+v", o)
	}
	st, err := c.Status(context.Background())
	if err != nil || !st.Paired || st.Pending || st.OwnerChatID != tess || st.OwnerFirstName != "Tess" {
		t.Errorf("after: %+v %v", st, err)
	}
	if lt.count(fmt.Sprintf("paired with user %d\n", tess)) != 1 {
		t.Errorf("no pairing log line")
	}

	// The owner talks to the hub; a late /start with the same code from someone else is dropped.
	r.from(tess, "/status")
	r.tg.waitFor(t, "status for the owner", has("send", "hub: up"))
	n := len(r.tg.snapshot())
	late := r.from(other, "/start "+code.Code)
	r.from(other, "hi")
	if cs := r.tg.snapshot(); len(cs) != n {
		t.Errorf("sent to someone else: %+v", cs[n:])
	}
	if o := r.owner(); o.UserID != tess {
		t.Errorf("owner changed to %+v", o)
	}
	if !lt.has(fmt.Sprintf("dropped update %d: pairing code from user %d on a paired hub", late, other)) {
		t.Errorf("late /start not logged as dropped")
	}
}

func TestPairingCodeExpires(t *testing.T) {
	r := newUnpairedRig(t)
	code := r.newCode()
	r.clk.Add(30*time.Minute + time.Second)
	r.from(tess, "/start "+code.Code)
	if o := r.owner(); o.Paired() {
		t.Fatalf("an expired code paired %+v", o)
	}
	if cs := r.tg.snapshot(); len(cs) != 0 {
		t.Errorf("unpaired hub sent %+v", cs)
	}
	if st, _ := r.client().Status(context.Background()); st.Pending {
		t.Errorf("expired code still pending: %+v", st)
	}
}

func TestPairingCodeSingleUse(t *testing.T) {
	r := newUnpairedRig(t)
	now := r.clk.Now()
	hash := pairing.Hash("ABCD0123")
	if err := r.st.NewPairingCode(hash, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.st.ClaimPairing(hash, Owner{UserID: tess, ChatID: tess, PairedAt: now}, now); !ok || err != nil {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if _, err := r.st.db.Exec(`DELETE FROM owner`); err != nil { // as if the owner were gone, the code stays used
		t.Fatal(err)
	}
	if ok, err := r.st.ClaimPairing(hash, Owner{UserID: other, ChatID: other, PairedAt: now}, now); ok || err != nil {
		t.Fatalf("a used code claimed again: %v %v", ok, err)
	}
	// A paired hub never pairs again by code.
	if err := r.st.NewPairingCode(pairing.Hash("ABCD0124"), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetOwner(Owner{UserID: tess, ChatID: tess, PairedAt: now}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.st.ClaimPairing(pairing.Hash("ABCD0124"), Owner{UserID: other, ChatID: other, PairedAt: now}, now); ok {
		t.Fatal("claimed on a paired hub")
	}
}

// Five wrong codes from one user lock that user out until a new code is
// made; they can't burn the owner's code.
func TestPairingWrongCodesPerUser(t *testing.T) {
	r := newUnpairedRig(t)
	lt := captureLog(t)
	code := r.newCode()
	for i := 0; i < pairing.MaxWrong; i++ {
		r.from(other, "/start "+wrong(code, i))
	}
	locked := r.from(other, "/start "+code.Code)
	if o := r.owner(); o.Paired() {
		t.Fatalf("a locked-out user paired: %+v", o)
	}
	if !lt.has(fmt.Sprintf("dropped update %d: pairing: user %d ignored after 5 wrong codes", locked, other)) {
		t.Errorf("lockout not logged")
	}
	if n := lt.count("pairing: wrong or expired code from user 3003"); n != 5 {
		t.Errorf("%d wrong-code lines, want 5", n)
	}
	r.from(tess, "/start "+code.Code)
	if o := r.owner(); o.UserID != tess {
		t.Fatalf("the owner's code was burnt: %+v", o)
	}

	// A new code clears the counters; the replaced code no longer works.
	r2 := newUnpairedRig(t)
	old := r2.newCode()
	for i := 0; i < pairing.MaxWrong; i++ {
		r2.from(other, "/start "+wrong(old, i))
	}
	fresh := r2.newCode()
	r2.from(tess, "/start "+old.Code)
	if o := r2.owner(); o.Paired() {
		t.Fatalf("a replaced code paired: %+v", o)
	}
	r2.from(other, "/start "+fresh.Code)
	if o := r2.owner(); o.UserID != other {
		t.Fatalf("after a new code the user may try again: %+v", o)
	}
	for _, s := range []string{wrong(old, 0), code.Code, fresh.Code} {
		if lt.has(s) {
			t.Errorf("a code reached the log")
		}
	}
}

func TestPairingNeedsPrivateChat(t *testing.T) {
	r := newUnpairedRig(t)
	lt := captureLog(t)
	code := r.newCode()
	id := r.update(&models.Message{From: &models.User{ID: tess}, Chat: models.Chat{ID: -100, Type: models.ChatTypeGroup},
		Text: "/start " + code.Code})
	if o := r.owner(); o.Paired() {
		t.Fatalf("paired from a group: %+v", o)
	}
	if !lt.has(fmt.Sprintf(`dropped update %d: pairing: user %d in chat -100 type "group"`, id, tess)) {
		t.Errorf("group /start not logged")
	}
	r.from(tess, "/start "+code.Code) // the group try neither used the code nor counted
	if o := r.owner(); o.UserID != tess {
		t.Fatalf("owner %+v", o)
	}
}

// I8: before pairing the hub sends nothing to anyone, whatever arrives.
func TestUnpairedHubSendsNothing(t *testing.T) {
	r := newUnpairedRig(t)
	r.startHost()
	lt := captureLog(t)
	code := r.newCode()
	ctx := context.Background()
	priv := models.Chat{ID: ownerUser, Type: models.ChatTypePrivate}
	me := &models.User{ID: ownerUser}
	updates := []*models.Update{
		{Message: &models.Message{ID: 1, From: me, Chat: priv, Text: "hello there"}},
		{Message: &models.Message{ID: 2, From: me, Chat: priv, Text: "/start"}},
		{Message: &models.Message{ID: 3, From: me, Chat: priv, Text: "/status"}},
		{Message: &models.Message{ID: 4, From: me, Chat: priv, Text: "/start " + wrong(code, 1)}},
		{Message: &models.Message{ID: 5, From: me, Chat: priv, Voice: &models.Voice{FileID: "v1"}}},
		{Message: &models.Message{ID: 6, From: me, Chat: models.Chat{ID: -100, Type: models.ChatTypeGroup}, Text: "/start " + code.Code}},
		{Message: &models.Message{ID: 7, Chat: priv, Text: "/start " + code.Code}},
		{EditedMessage: &models.Message{ID: 1, From: me, Chat: priv, Text: "edited"}},
		{MessageReaction: &models.MessageReactionUpdated{MessageID: 1, Chat: priv, User: me}},
		{CallbackQuery: &models.CallbackQuery{ID: "q1", From: *me, Data: "ag:1:0"}},
		{StoppedMessageGeneration: &models.MessageGenerationStopped{Chat: priv, DraftID: 9}},
	}
	for i, u := range updates {
		u.ID = int64(500 + i)
		if err := r.hub.intake(u); err != nil {
			t.Fatal(err)
		}
	}
	if n := lt.count("dropped update"); n != len(updates) {
		t.Errorf("%d drop lines, want %d", n, len(updates))
	}
	if lt.has("hello there") || lt.has("edited") {
		t.Errorf("a dropped update's text reached the log")
	}
	// Notices with nobody to send them to.
	if err := r.st.HostSeen("mac2", nil, r.clk.Now()); err != nil {
		t.Fatal(err)
	}
	r.clk.Add(time.Hour)
	r.hub.checkHealth(ctx)
	if _, err := r.hub.SendPing(ctx, proto.SendRequest{ID: "s1", Text: "from an agent"}); err == nil {
		t.Error("an agent's send went through on an unpaired hub")
	}
	if _, err := r.hub.tg.Send(ctx, ownerUser, "direct", 0); !errors.Is(err, errNotOwner) {
		t.Errorf("guard: %v", err)
	}
	if err := r.hub.tg.SetCommands(ctx, commands); err == nil {
		t.Error("commands set before pairing")
	}
	r.hub.refreshTelegram(ctx) // no commands, no menu button
	var events int
	r.st.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&events)
	if cs := r.tg.snapshot(); len(cs) != 0 || events != 0 {
		t.Errorf("unpaired hub: %d Bot API calls %+v, %d events stored", len(cs), cs, events)
	}
	if o := r.owner(); o.Paired() {
		t.Errorf("paired by %+v", o)
	}
}

func TestPairingResetAndBind(t *testing.T) {
	r := newRig(t)
	c := r.client()
	ctx := context.Background()
	if _, err := c.New(ctx); !isHTTP(err, http.StatusConflict) {
		t.Errorf("a code on a paired hub: %v", err)
	}
	st, err := c.Reset(ctx)
	if err != nil || st.Paired || st.Pending || st.OwnerChatID != 0 {
		t.Fatalf("reset: %+v %v", st, err)
	}
	r.from(ownerUser, "/status")
	if cs := r.tg.snapshot(); len(cs) != 0 {
		t.Errorf("the old owner still gets answers: %+v", cs)
	}
	if _, err := c.Bind(ctx, other); err != nil {
		t.Fatal(err)
	}
	if st, err = c.Status(ctx); err != nil || !st.Paired || st.OwnerChatID != other || st.OwnerFirstName != "" {
		t.Fatalf("bind: %+v %v", st, err)
	}
	if o := r.owner(); o.UserID != other || o.ChatID != other {
		t.Errorf("owner %+v", o)
	}
	cs := r.tg.waitFor(t, "commands after bind", has("commands", ""))
	if count(cs, "menu", "") != 1 {
		t.Errorf("menu button after bind: %+v", cs)
	}
	r.from(other, "/status")
	r.tg.waitFor(t, "the bound owner is answered", has("send", "hub: up"))
}

func isHTTP(err error, code int) bool {
	var e *pairing.APIError
	return errors.As(err, &e) && e.Status == code
}

func TestPairingAPIAuth(t *testing.T) {
	r := newUnpairedRig(t)
	h := r.hub.Handler()
	do := func(method, path, remote string, key bool) int {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.RemoteAddr = remote
		if key {
			req.Header.Set("X-Hommy-Host", "mac1")
			req.Header.Set("Authorization", "Bearer "+hostKey)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	const lo, lan = "127.0.0.1:5000", "192.0.2.10:5000"
	for _, c := range []struct {
		method, path, remote string
		key                  bool
		want                 int
	}{
		{"GET", "/api/pairing", lo, false, 401},
		{"POST", "/api/pairing", lo, false, 401},
		{"GET", "/api/status", lo, false, 401},
		{"POST", "/api/pairing", lan, true, 403},
		{"POST", "/api/pairing", "[::1]:5000", true, 200},
		{"GET", "/api/pairing", lan, true, 200},
		{"GET", "/api/status", lan, true, 200},
		{"DELETE", "/api/pairing", lo, true, 405},
	} {
		if got := do(c.method, c.path, c.remote, c.key); got != c.want {
			t.Errorf("%s %s from %s key=%v: %d, want %d", c.method, c.path, c.remote, c.key, got, c.want)
		}
	}
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Header.Set("X-Hommy-Host", "mac1")
	req.Header.Set("Authorization", "Bearer "+hostKey+"x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: %d", rec.Code)
	}
}

func TestAPIStatus(t *testing.T) {
	r := newRig(t)
	c := r.client()
	ctx := context.Background()
	checks := map[string]string{"vault": "ok", "agterm": "down"}
	if err := r.st.HostSeen("mac2", &proto.Heartbeat{Host: "mac2", Version: "9.9.9", Checks: checks}, r.clk.Now()); err != nil {
		t.Fatal(err)
	}
	r.hub.markTelegramOK()
	st, err := c.APIStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != version.String() || st.Telegram != "ok" || !st.Paired || st.Owner == nil || st.Owner.FirstName != "Owner" {
		t.Errorf("status %+v", st)
	}
	if len(st.Hosts) != 2 || st.Hosts[0].Name != "mac1" || st.Hosts[1].Name != "mac2" {
		t.Fatalf("hosts %+v", st.Hosts)
	}
	if h := st.Hosts[0]; h.Online || h.LastSeen != nil {
		t.Errorf("never-seen host %+v", h)
	}
	if h := st.Hosts[1]; h.Online || h.LastSeen == nil || h.Version != "9.9.9" || h.Checks["vault"] != "ok" || h.Checks["agterm"] != "down" {
		t.Errorf("mac2 %+v", h)
	}
	r.startHost()
	if st, _ = c.APIStatus(ctx); !st.Hosts[0].Online || st.Hosts[0].LastSeen == nil {
		t.Errorf("connected host %+v", st.Hosts[0])
	}

	r.clk.Add(4 * time.Minute)
	if st, _ = c.APIStatus(ctx); st.Telegram != "stale" {
		t.Errorf("4 min without the Bot API: %q", st.Telegram)
	}
	r.tg.mu.Lock()
	r.tg.meErr = fmt.Errorf("%w, Unauthorized", bot.ErrorUnauthorized)
	r.tg.mu.Unlock()
	r.hub.refreshTelegram(ctx)
	if st, _ = c.APIStatus(ctx); st.Telegram != "unauthorized" {
		t.Errorf("refused token: %q", st.Telegram)
	}
	if _, err := c.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ = c.APIStatus(ctx); st.Paired || st.Owner != nil {
		t.Errorf("after reset %+v", st)
	}
}

// The Mini App answers the live owner only.
func TestMiniAppAuthFollowsOwner(t *testing.T) {
	r := newRig(t)
	initData := func(user int64) string {
		return signInitData(testConfig().TelegramToken, url.Values{"auth_date": {fmt.Sprint(r.clk.Now().Unix())},
			"user": {fmt.Sprintf(`{"id":%d}`, user)}})
	}
	board := func(user int64) int {
		code, _ := r.api("/api/board", map[string]string{"X-Telegram-Init-Data": initData(user)})
		return code
	}
	if board(ownerUser) != 200 || board(other) != 401 {
		t.Fatalf("paired: owner %d, other %d", board(ownerUser), board(other))
	}
	ctx := context.Background()
	if _, err := r.client().Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if board(ownerUser) != 401 {
		t.Error("the old owner after a reset")
	}
	if _, err := r.client().Bind(ctx, other); err != nil {
		t.Fatal(err)
	}
	if board(other) != 200 || board(ownerUser) != 401 {
		t.Errorf("after bind: new %d, old %d", board(other), board(ownerUser))
	}
}

// D-11: /new_session on a session target goes back to the default target.
func TestNewSessionLeavesSessionTarget(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.add(sessA)
	r.text("/target session@mac1:" + strings.ToLower(sessA))
	r.tg.waitFor(t, "switch", has("edit", "🎯 Talking to: "))
	r.text("/new_session")
	r.tg.waitFor(t, "back to default", has("send", "next message starts a new session"))
	if tgt, err := r.hub.currentTarget(ownerUser); err != nil || tgt.String() != testConfig().DefaultTarget {
		t.Errorf("target %v %v, want %s", tgt, err, testConfig().DefaultTarget)
	}
}

// SPEECH=off: a voice note gets "voice is off", and nothing is transcribed.
func TestVoiceOff(t *testing.T) {
	r := &rig{t: t, dir: t.TempDir(), tg: newFakeTG(), sp: &fakeSpeech{}, clk: &clock{t: time.Now()}, nextMs: 100,
		mod: func(c *Config) { c.Transcribe = speech.TranscribeConfig{} }}
	r.startHub()
	r.sp.setFailSTT(errors.New("transcription must not be called"))
	id := r.send(func(m *models.Message) { m.Voice = &models.Voice{FileID: "v1"} })
	cs := r.tg.waitFor(t, "voice off", has("send", "voice is off"))
	if c := lastWith(cs, "send", "voice is off"); c.ReplyTo != id {
		t.Errorf("not a reply to the voice note: %+v", c)
	}
	if count(cs, "send", "transcription") != 0 || count(cs, "send", "🎤") != 0 {
		t.Errorf("transcribed anyway: %+v", cs)
	}
}
