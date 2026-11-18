package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

func TestTextAnsweredFromHost(t *testing.T) {
	r := newRig(t)
	r.startHost()
	id := r.text("what's in my inbox?")
	cs := r.tg.waitFor(t, "answer", has("send", "answer to [what's in my inbox?]"))
	if count(cs, "react", "✍") != 1 || count(cs, "react", "👀") != 0 {
		t.Errorf("want ✍ only: %+v", cs)
	}
	a := r.answers(cs)[0]
	if a.ReplyTo != id || !strings.Contains(a.Text, "/vault resume=none") {
		t.Errorf("answer %+v", a)
	}

	// The next message resumes the session the first run ended on.
	r.text("and now?")
	cs = r.tg.waitFor(t, "second answer", has("send", "answer to [and now?]"))
	if !strings.Contains(r.answers(cs)[1].Text, "resume=sess-") {
		t.Errorf("second run should resume: %s", r.answers(cs)[1].Text)
	}
}

func TestOfflineQueueThenReconnectAnswersOnce(t *testing.T) {
	r := newRig(t)
	r.startHost().shutdown() // mac1 was up once, then went away
	r.waitConnected(false)
	id := r.text("queued while away")
	r.tg.waitFor(t, "👀", has("react", "👀"))

	r.clk.Add(2*time.Minute + time.Second)
	r.hub.checkHealth(context.Background())
	r.hub.checkHealth(context.Background()) // once per outage, not per check
	cs := r.tg.snapshot()
	if count(cs, "send", "host mac1 silent") != 1 || count(cs, "send", "mac1 is offline; queued") != 1 {
		t.Fatalf("want one alert and one queued notice: %+v", cs)
	}
	if count(cs, "send", "host mac2") != 0 {
		t.Errorf("a host that never connected is not silent: %+v", cs)
	}
	for _, c := range cs {
		if strings.Contains(c.Text, "offline; queued") && c.ReplyTo != id {
			t.Errorf("queued notice should reply to the message: %+v", c)
		}
	}

	r.startHost()
	cs = r.tg.waitFor(t, "answer", has("send", "answer to [queued while away]"))
	if count(cs, "send", "mac1 is back") != 1 {
		t.Errorf("want one back line: %+v", cs)
	}
	r.text("sync") // a later job proves the lane moved on without a second answer
	cs = r.tg.waitFor(t, "sync answer", has("send", "answer to [sync]"))
	if n := count(cs, "send", "answer to [queued while away]"); n != 1 {
		t.Errorf("answered %d times", n)
	}
}

// E7: the owner told "<host> is offline; queued" (no 🔴 alert yet) also hears
// when the host is back.
func TestQueuedNoticeThenBackLine(t *testing.T) {
	r := &rig{t: t, dir: t.TempDir(), tg: newFakeTG(), sp: &fakeSpeech{}, clk: &clock{t: time.Now()}, nextMs: 100,
		mod: func(c *Config) { c.SilentAfter, c.QueuedNotice = time.Hour, 5*time.Second }}
	r.startHub()
	r.startHost().shutdown()
	r.waitConnected(false)
	r.text("echo: queued")
	r.tg.waitFor(t, "👀", has("react", "👀"))
	r.clk.Add(6 * time.Second)
	r.hub.checkHealth(context.Background())
	cs := r.tg.snapshot()
	if count(cs, "send", "mac1 is offline; queued") != 1 || count(cs, "send", "silent") != 0 {
		t.Fatalf("want the queued notice only: %+v", cs)
	}
	r.startHost()
	cs = r.tg.waitFor(t, "back line", has("send", "🟢 mac1 is back (offline for 6s)"))
	r.tg.waitFor(t, "answer", has("send", "answer to [echo: queued]"))
	if n := count(r.tg.snapshot(), "send", "is back"); n != 1 {
		t.Errorf("%d back lines: %+v", n, cs)
	}
	if row, _ := r.st.Host("mac1"); row.QueuedNoticed || row.SilentAlerted {
		t.Errorf("flags not cleared: %+v", row)
	}
}

func TestQueuedForNeverSeenHostGetsNotice(t *testing.T) {
	r := newRig(t)
	r.text("/target vault@mac2")
	id := r.text("hello mac2")
	r.tg.waitFor(t, "👀", has("react", "👀"))
	r.clk.Add(3 * time.Minute)
	r.hub.checkHealth(context.Background())
	cs := r.tg.snapshot()
	if count(cs, "send", "mac2 is offline; queued (1 waiting)") != 1 || count(cs, "send", "silent") != 0 {
		t.Errorf("%+v", cs)
	}
	_ = id
}

func TestHubRestartKeepsQueuedJob(t *testing.T) {
	r := newRig(t)
	r.text("survive the restart")
	r.tg.waitFor(t, "👀", has("react", "👀"))
	r.stopHub()
	r.startHub()
	r.startHost()
	r.tg.waitFor(t, "answer", has("send", "answer to [survive the restart]"))
	r.text("after")
	cs := r.tg.waitFor(t, "after", has("send", "answer to [after]"))
	if n := count(cs, "send", "answer to [survive the restart]"); n != 1 {
		t.Errorf("answered %d times", n)
	}
}

func TestRedeliveredUpdateMakesNoSecondJob(t *testing.T) {
	r := newRig(t)
	r.startHost()
	u := &models.Update{ID: 7, Message: &models.Message{ID: 7, Text: "once", From: &models.User{ID: ownerUser},
		Chat: models.Chat{ID: ownerUser, Type: models.ChatTypePrivate}}}
	r.hub.intake(u)
	r.tg.waitFor(t, "answer", has("send", "answer to [once]"))
	r.hub.intake(u) // Telegram redelivers after a crash before the offset was confirmed
	r.text("next")
	cs := r.tg.waitFor(t, "next", has("send", "answer to [next]"))
	if n := count(cs, "send", "answer to [once]"); n != 1 {
		t.Errorf("answered %d times", n)
	}
}

func TestDuplicateResultDeliveredOnce(t *testing.T) {
	r := newRig(t)
	r.text("dup")
	r.hub.drainEvents(context.Background())
	j, _ := r.st.OldestWaiting("mac1")
	res := proto.Result{JobID: j.ID, Status: proto.StatusOK, Data: mustJSONRaw(proto.RunPResult{Text: "answer to [dup]"})}
	r.hub.handleResult(context.Background(), "mac1", res)
	r.hub.handleResult(context.Background(), "mac1", res)
	r.hub.handleResult(context.Background(), "mac2", res) // not its host
	r.tg.waitFor(t, "answer", has("send", "answer to [dup]"))
	waitStatus(t, r.st, j.ID, JobDone)
	if n := count(r.tg.snapshot(), "send", "answer to [dup]"); n != 1 {
		t.Errorf("delivered %d times", n)
	}
	if st, ok, _ := r.st.DoneLedger(j.EventID); !ok || st != proto.StatusOK {
		t.Errorf("done ledger: %q %v", st, ok)
	}
}

func waitStatus(t *testing.T, st *Store, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, _ := st.Job(id)
		if j != nil && j.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s status %v, want %s", id, j, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOrderKeptWithinLane(t *testing.T) {
	r := newRig(t)
	for _, s := range []string{"one", "two", "three"} {
		r.text(s)
	}
	r.startHost()
	cs := r.tg.waitFor(t, "three answers", func(cs []call) bool { return len(r.answers(cs)) == 3 })
	a := r.answers(cs)
	for i, s := range []string{"one", "two", "three"} {
		if !strings.Contains(a[i].Text, "["+s+"]") {
			t.Fatalf("answer %d = %s", i, a[i].Text)
		}
	}
	if !strings.Contains(a[2].Text, "resume=sess-") {
		t.Errorf("queued runs should chain sessions: %s", a[2].Text)
	}
}

func TestNewSessionStartsFresh(t *testing.T) {
	r := newRig(t)
	r.startHost()
	r.text("remember me")
	r.tg.waitFor(t, "first", has("send", "answer to [remember me]"))
	r.text("/new_session")
	r.tg.waitFor(t, "reset", has("send", "session reset"))
	r.text("what did I ask?")
	cs := r.tg.waitFor(t, "fresh", has("send", "answer to [what did I ask?]"))
	if a := r.answers(cs)[1]; !strings.Contains(a.Text, "resume=none") {
		t.Errorf("after /new_session the run must not resume: %s", a.Text)
	}
}

func TestTargetPDir(t *testing.T) {
	r := newRig(t)
	r.startHost()
	briefs := filepath.Join(r.dir, "agent-briefs")
	os.MkdirAll(briefs, 0o755)
	r.text("/target p@mac1:" + briefs)
	r.tg.waitFor(t, "switch", has("send", "🎯 Now talking to agent-briefs · mac1"))
	r.text("ls")
	cs := r.tg.waitFor(t, "answer", has("send", "answer to [ls]"))
	real, _ := filepath.EvalSymlinks(briefs)
	if !strings.Contains(r.answers(cs)[0].Text, " in "+real+" ") {
		t.Errorf("should run in %s: %s", real, r.answers(cs)[0].Text)
	}
	r.text("/target nowhere@mac1")
	r.text("/target vault@ghost")
	r.text("/target vault@mac1")
	cs = r.tg.waitFor(t, "back", has("send", "🎯 Now talking to vault · mac1"))
	if count(cs, "send", "unknown kind") != 1 || count(cs, "send", `unknown host "ghost"`) != 1 {
		t.Errorf("bad targets must be refused: %+v", cs)
	}
	r.text("vault again")
	cs = r.tg.waitFor(t, "vault answer", has("send", "answer to [vault again]"))
	if !strings.Contains(r.answers(cs)[1].Text, "/vault ") {
		t.Errorf("should run in the vault: %s", r.answers(cs)[1].Text)
	}
}

func TestTargetOutsideHomeRefusedByHost(t *testing.T) {
	r := newRig(t)
	r.startHost()
	r.text("/target p@mac1:/etc")
	r.text("cat passwd")
	cs := r.tg.waitFor(t, "error", has("send", "outside"))
	if len(r.answers(cs)) != 0 {
		t.Errorf("must not run outside home: %+v", cs)
	}
}

func TestPhotoSavedThenDescribed(t *testing.T) {
	r := newRig(t)
	hr := r.startHost()
	r.tg.files["ph-big"] = []byte("JPEGDATA")
	r.send(func(m *models.Message) {
		m.Caption = "describe this"
		m.Photo = []models.PhotoSize{{FileID: "ph-small"}, {FileID: "ph-big"}}
	})
	r.text("after the photo")
	cs := r.tg.waitFor(t, "two answers", func(cs []call) bool { return len(r.answers(cs)) == 2 })
	a := r.answers(cs)
	if !strings.Contains(a[0].Text, "[[photo]\ndescribe this\n\n[Image attached: "+hr.cfg.MediaDir) {
		t.Fatalf("photo run first, with the saved path: %s", a[0].Text)
	}
	if !strings.Contains(a[1].Text, "[after the photo]") {
		t.Errorf("text after the photo answered second: %s", a[1].Text)
	}
	var bigPayloads int
	r.st.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type='save_file' AND length(payload) > 2`).Scan(&bigPayloads)
	if bigPayloads != 0 {
		t.Errorf("file bytes should be dropped once the job is done")
	}
	path := strings.TrimSuffix(strings.SplitN(strings.SplitN(a[0].Text, "[Image attached: ", 2)[1], "]", 2)[0], "]")
	if b, err := os.ReadFile(path); err != nil || string(b) != "JPEGDATA" {
		t.Errorf("saved file %s: %q %v", path, b, err)
	}
}

func TestVoiceTranscribedAndLongAnswerSpoken(t *testing.T) {
	r := newRig(t)
	r.startHost()
	r.tg.files["v1"] = []byte("OggSvoice")
	id := r.send(func(m *models.Message) { m.Voice = &models.Voice{FileID: "v1"} })
	cs := r.tg.waitFor(t, "answer", has("send", "answer to [[voice message, transcribed]\nwhat is two plus two]"))
	if count(cs, "send", "🎤 (mac2) what is two plus two") != 1 {
		t.Errorf("transcript line missing: %+v", cs)
	}
	r.text("LONG please")
	cs = r.tg.waitFor(t, "voice reply", has("voice", "OggSspoken version"))
	for _, c := range cs {
		if c.Kind == "send" && strings.Contains(c.Text, "<tts>") {
			t.Errorf("tts block must be stripped: %s", c.Text)
		}
	}
	_ = id
}

func TestUnauthorizedDroppedSilently(t *testing.T) {
	r := newRig(t)
	for _, u := range []*models.Update{
		{ID: 1, Message: &models.Message{ID: 1, Text: "hi", From: &models.User{ID: 42}, Chat: models.Chat{ID: 42, Type: models.ChatTypePrivate}}},
		{ID: 2, Message: &models.Message{ID: 2, Text: "hi", From: &models.User{ID: ownerUser}, Chat: models.Chat{ID: -100, Type: models.ChatTypeGroup}}},
		{ID: 3, EditedMessage: &models.Message{ID: 3, Text: "hi", From: &models.User{ID: 42}, Chat: models.Chat{ID: 42, Type: models.ChatTypePrivate}}},
		{ID: 4, MessageReaction: &models.MessageReactionUpdated{MessageID: 5, User: &models.User{ID: 42}, Chat: models.Chat{ID: 42, Type: models.ChatTypePrivate}}},
		{ID: 5, MessageReaction: &models.MessageReactionUpdated{MessageID: 5, Chat: models.Chat{ID: ownerUser, Type: models.ChatTypePrivate}}},
		{ID: 6, CallbackQuery: &models.CallbackQuery{ID: "c", From: models.User{ID: 42}}},
		{ID: 7, ChannelPost: &models.Message{ID: 7, Text: "hi", Chat: models.Chat{ID: -5, Type: models.ChatTypeChannel}}},
	} {
		r.hub.intake(u)
	}
	r.hub.drainEvents(context.Background())
	if cs := r.tg.snapshot(); len(cs) != 0 {
		t.Errorf("rejects must get no reply: %+v", cs)
	}
	if e, _ := r.st.NextEvent(); e != nil {
		t.Errorf("nothing should be stored: %+v", e)
	}
	// the owner's own reaction is stored and routed (to nothing here: message 5 is unknown).
	r.hub.intake(&models.Update{ID: 8, MessageReaction: &models.MessageReactionUpdated{MessageID: 5,
		User: &models.User{ID: ownerUser}, Chat: models.Chat{ID: ownerUser, Type: models.ChatTypePrivate},
		NewReaction: []models.ReactionType{{Type: models.ReactionTypeTypeEmoji, ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: "👍"}}}}})
	r.hub.drainEvents(context.Background())
	var n int
	// (parked: an unknown message may be an agent's whose spool entry is still to come)
	r.st.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind='message_reaction' AND status=?`, EventParked).Scan(&n)
	if n != 1 {
		t.Errorf("reaction from the owner should be stored and processed, got %d", n)
	}
	if cs := r.tg.snapshot(); len(cs) != 0 {
		t.Errorf("a reaction on an unknown message is ignored: %+v", cs)
	}
}

func TestStatusAndHealthz(t *testing.T) {
	r := newRig(t)
	r.hub.markTelegramOK()
	r.text("/status")
	cs := r.tg.waitFor(t, "status", has("send", "hosts:"))
	if st := cs[len(cs)-1].Text; !strings.Contains(st, "• mac1: offline · never seen") || !strings.Contains(st, "queue: 0") {
		t.Errorf("status: %s", st)
	}
	r.startHost()
	r.hub.handleFrame(context.Background(), r.hub.conn("mac1"), proto.Frame{Type: proto.FrameHeartbeat,
		Heartbeat: &proto.Heartbeat{Host: "mac1", Version: "v9", ClaudeSessions: 3}})
	r.text("/status")
	cs = r.tg.waitFor(t, "status online", has("send", "• mac1: online"))
	last := cs[len(cs)-1].Text
	if !strings.Contains(last, "last seen 0s ago · v v9 · claude sessions 3") || !strings.Contains(last, "target: vault@mac1") {
		t.Errorf("status: %s", last)
	}

	srv := r.hub.Handler()
	rec := &respRec{h: http.Header{}}
	req, _ := http.NewRequest("GET", "/healthz", nil)
	srv.ServeHTTP(rec, req)
	var hz healthz
	json.Unmarshal(rec.body, &hz)
	if rec.code != 0 && rec.code != 200 || !hz.OK || !hz.Hosts["mac1"] {
		t.Errorf("healthz %d %s", rec.code, rec.body)
	}
	r.clk.Add(5 * time.Minute) // Bot API unreachable for too long
	rec = &respRec{h: http.Header{}}
	srv.ServeHTTP(rec, req)
	if rec.code != http.StatusServiceUnavailable {
		t.Errorf("stale telegram should fail healthz: %d", rec.code)
	}
}

type respRec struct {
	h    http.Header
	code int
	body []byte
}

func (r *respRec) Header() http.Header         { return r.h }
func (r *respRec) Write(b []byte) (int, error) { r.body = append(r.body, b...); return len(b), nil }
func (r *respRec) WriteHeader(c int)           { r.code = c }

func TestWSRejectsBadKey(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct{ host, key string }{{"mac1", "wrong-key-0123456789abcdef0123456789"}, {"ghost", hostKey}, {"", ""}} {
		req, _ := http.NewRequest("GET", strings.Replace(r.url, "ws://", "http://", 1), nil)
		req.Header.Set("X-Hommy-Host", c.host)
		req.Header.Set("Authorization", "Bearer "+c.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%+v: status %d", c, resp.StatusCode)
		}
	}
}

func TestUnknownCommandsPassThrough(t *testing.T) {
	r := newRig(t)
	r.startHost()
	r.text("/morning_processing")
	r.tg.waitFor(t, "removed command passes through", has("send", "answer to [/morning_processing]"))
	r.text("/unknown_cmd x")
	r.tg.waitFor(t, "passthrough", has("send", "answer to [/unknown_cmd x]"))
}

func TestVoiceCommand(t *testing.T) {
	r := newRig(t)
	r.text("/voice am_michael")
	r.text("/voice nope")
	r.text("/voice")
	cs := r.tg.waitFor(t, "voice list", has("send", "current voice: am_michael"))
	if count(cs, "send", "voice set to am_michael") != 1 || count(cs, "send", "unknown voice: nope") != 1 {
		t.Errorf("%+v", cs)
	}
	if b := r.hub.backendFor(""); b.Voice != "am_michael" {
		t.Errorf("default backend voice %q", b.Voice)
	}
}
