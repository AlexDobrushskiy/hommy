package hub

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/host"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

const newSess = "BEEF0001-0000-0000-0000-000000000000"

func (r *agtermRig) waitLines(t *testing.T, id string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.ag.mu.Lock()
		s, ok := r.ag.sessions[id]
		var got []string
		if ok {
			got = append(got, s.submitted...)
		}
		r.ag.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s: typed %q, want %d lines", id, got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *agtermRig) sessionCount() int {
	r.ag.mu.Lock()
	defer r.ag.mu.Unlock()
	return len(r.ag.sessions)
}

// Rows 1, 2, 7: picker → new interactive → the first message creates the
// session (brief folder, typed line, 🆕 line), the target becomes it, and the
// next message goes to it.
func TestNewInteractiveSessionFromFirstMessage(t *testing.T) {
	r := startAgtermHost(t)
	r.webAppData(`{"v":1,"target":"new@mac1:~/work/proj:interactive"}`)
	cs := r.tg.waitFor(t, "switch", has("send", "Now talking to new interactive: proj · mac1"))
	if kb := lastWith(cs, "send", "Now talking").KB; kb.InputFieldPlaceholder != "→ new interactive: proj · mac1" {
		t.Errorf("placeholder %q", kb.InputFieldPlaceholder)
	}
	id := r.text("create a file hello.txt containing hi, then tell me done")
	cs = r.tg.waitFor(t, "🆕", has("send", "🆕 create-file-hello-txt started on mac1"))
	nw := lastWith(cs, "send", "🆕")
	if nw.ReplyTo != id || !strings.Contains(nw.Text, "🎯 Now talking to create-file-hello-txt · mac1") {
		t.Errorf("🆕 line: %+v", nw)
	}
	if nw.KB == nil || nw.KB.InputFieldPlaceholder != "→ create-file-hello-txt · mac1" {
		t.Errorf("the 🆕 line must carry the new keyboard: %+v", nw.KB)
	}
	r.tg.waitFor(t, "✍", reactedOn(id, "✍"))
	typed := r.waitLines(t, newSess, 1)
	if want := fmt.Sprintf("[Telegram · tg-%d] create a file hello.txt containing hi, then tell me done", id); typed[0] != want {
		t.Errorf("typed %q, want %q", typed[0], want)
	}
	if st, err := os.Stat(filepath.Join(r.dir, "work", "proj")); err != nil || !st.IsDir() {
		t.Errorf("the picked folder should be created: %v", err)
	}
	brief := filepath.Join(r.dir, "agent-briefs", time.Now().Format("2006-01-02")+"-create-file-hello-txt")
	if b, err := os.ReadFile(filepath.Join(brief, "brief.md")); err != nil || string(b) != "create a file hello.txt containing hi, then tell me done\n" {
		t.Errorf("brief.md = %q %v", b, err)
	}
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "session@mac1:"+newSess {
		t.Errorf("target %q", tgt)
	}
	if rs := r.hub.recents(ownerUser); len(rs) == 0 || rs[0].Label != "create-file-hello-txt · mac1" {
		t.Errorf("recents %+v", rs)
	}

	id2 := r.text("and one more thing")
	r.tg.waitFor(t, "second ✍", reactedOn(id2, "✍"))
	typed = r.waitLines(t, newSess, 2)
	if !strings.HasSuffix(typed[1], "] and one more thing") {
		t.Errorf("second line %q", typed[1])
	}
	if n := r.sessionCount(); n != 1 {
		t.Errorf("%d sessions, want 1", n)
	}
}

// Row 5 (+7 while queued): host offline → 👀; messages sent before it is back
// queue behind the creation; on reconnect exactly one session, lines in order.
func TestNewSessionQueuedWhileHostOffline(t *testing.T) {
	r := newRig(t)
	r.text("/target new@mac1:~/work:interactive")
	r.tg.waitFor(t, "switch", has("send", "Now talking to new interactive: work · mac1"))
	id1 := r.text("first message")
	r.tg.waitFor(t, "👀 1", reactedOn(id1, "👀"))
	id2 := r.text("second message")
	r.tg.waitFor(t, "👀 2", reactedOn(id2, "👀"))
	if n, _ := r.st.QueueDepth("mac1"); n != 2 {
		t.Fatalf("queued %d jobs, want 2 (one new_session, one line behind it)", n)
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	ar := r.runAgtermHost(&fakeAgterm{sessions: map[string]*fakeSess{}})
	typed := ar.waitLines(t, newSess, 2)
	if !strings.HasSuffix(typed[0], "] first message") || !strings.HasSuffix(typed[1], "] second message") {
		t.Errorf("typed %q", typed)
	}
	r.tg.waitFor(t, "acks", func(cs []call) bool { return reactedOn(id1, "✍")(cs) && reactedOn(id2, "✍")(cs) })
	if n := ar.sessionCount(); n != 1 {
		t.Errorf("%d sessions, want 1", n)
	}
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "session@mac1:"+newSess {
		t.Errorf("target %q", tgt)
	}
}

// A photo and then a text queued behind a creation are typed in that order.
func TestNewSessionFollowUpsKeepOrderWithAPhoto(t *testing.T) {
	r := newRig(t)
	r.tg.mu.Lock()
	r.tg.files["ph-big"] = []byte("JPEGDATA")
	r.tg.mu.Unlock()
	r.text("/target new@mac1:~/work:interactive")
	r.tg.waitFor(t, "switch", has("send", "Now talking to new interactive: work · mac1"))
	id1 := r.text("first message")
	r.tg.waitFor(t, "👀 1", reactedOn(id1, "👀"))
	id2 := r.send(func(m *models.Message) {
		m.Caption = "the receipt"
		m.Photo = []models.PhotoSize{{FileID: "ph-big"}}
	})
	r.tg.waitFor(t, "👀 2", reactedOn(id2, "👀"))
	id3 := r.text("third message")
	r.tg.waitFor(t, "👀 3", reactedOn(id3, "👀"))
	ar := r.runAgtermHost(&fakeAgterm{sessions: map[string]*fakeSess{}})
	typed := ar.waitLines(t, newSess, 3)
	if !strings.HasSuffix(typed[0], "] first message") || !strings.Contains(typed[1], "the receipt") || !strings.HasSuffix(typed[2], "] third message") {
		t.Errorf("typed out of order: %q", typed)
	}
}

// A voice note sent while the session is being created, whose transcript is
// ready only after it exists, goes to that session (no second one).
func TestNewSessionSlowVoiceFollowUpMakesNoSecond(t *testing.T) {
	r := startAgtermHost(t)
	gate := make(chan struct{})
	r.sp.mu.Lock()
	r.sp.sttGate = gate
	r.sp.mu.Unlock()
	r.tg.mu.Lock()
	r.tg.files["v1"] = []byte("OGG")
	r.tg.mu.Unlock()
	r.text("/target new@mac1:~/work:interactive")
	r.tg.waitFor(t, "switch", has("send", "Now talking to new interactive"))
	r.text("first message")
	vid := r.send(func(m *models.Message) { m.Voice = &models.Voice{FileID: "v1"} })
	r.tg.waitFor(t, "🆕", has("send", "🆕 first-message started on mac1"))
	close(gate)
	typed := r.waitLines(t, newSess, 2)
	if !strings.HasSuffix(typed[1], "what is two plus two") {
		t.Errorf("typed %q", typed)
	}
	r.tg.waitFor(t, "✍ on the voice note", reactedOn(vid, "✍"))
	if n := r.sessionCount(); n != 1 {
		t.Errorf("%d sessions, want 1", n)
	}
}

// A redelivered update makes no second session.
func TestNewSessionOncePerEvent(t *testing.T) {
	r := startAgtermHost(t)
	r.text("/target new@mac1:~/work:interactive")
	r.tg.waitFor(t, "switch", has("send", "Now talking to new interactive"))
	r.nextMs++
	u := &models.Update{ID: int64(r.nextMs), Message: &models.Message{ID: r.nextMs, From: &models.User{ID: ownerUser},
		Chat: models.Chat{ID: ownerUser, Type: models.ChatTypePrivate}, Text: "only once"}}
	r.hub.intake(u)
	r.hub.intake(u)
	r.tg.waitFor(t, "🆕", has("send", "🆕 only-once started on mac1"))
	r.waitLines(t, newSess, 1)
	if jobs, _ := r.st.queryJobs(`SELECT `+jobCols+` FROM jobs WHERE type=?`, proto.JobNewSession); len(jobs) != 1 {
		t.Errorf("%d new_session jobs, want 1", len(jobs))
	}
	if n := r.sessionCount(); n != 1 {
		t.Errorf("%d sessions, want 1", n)
	}
	if c := count(r.tg.snapshot(), "send", "🆕"); c != 1 {
		t.Errorf("%d 🆕 lines", c)
	}
}

// Row 4: new -p → the target becomes p@<host>:<dir> with a fresh session and
// the message runs in the (created) folder; the next one resumes it.
func TestNewPSessionRunsInItsFolder(t *testing.T) {
	r := newRig(t)
	r.startHost()
	r.webAppData(`{"v":1,"target":"new@mac1:~/work/pnew:-p"}`)
	r.tg.waitFor(t, "switch", has("send", "Now talking to new -p: pnew · mac1"))
	id := r.text("pwd")
	cs := r.tg.waitFor(t, "answer", has("send", "answer to [pwd]"))
	a := lastWith(cs, "send", "answer to [pwd]")
	real, _ := filepath.EvalSymlinks(r.dir)
	if !strings.Contains(a.Text, "in "+filepath.Join(real, "work", "pnew")+" resume=none") || a.ReplyTo != id {
		t.Errorf("answer %+v", a)
	}
	if !has("send", "Now talking to pnew · mac1 (new claude -p session)")(cs) {
		t.Errorf("switch to the p@ target missing: %+v", cs)
	}
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "p@mac1:~/work/pnew" {
		t.Errorf("target %q", tgt)
	}
	r.text("again")
	cs = r.tg.waitFor(t, "second", has("send", "answer to [again]"))
	if strings.Contains(lastWith(cs, "send", "answer to [again]").Text, "resume=none") {
		t.Error("the second message must resume the new session")
	}
}

// Row 6: a folder outside the allowed roots is refused by the picker (hub),
// and by the host when forced via /target; the target then stays new@….
func TestNewSessionFolderOutsideRootsRefused(t *testing.T) {
	r := startAgtermHost(t)
	deadline := time.Now().Add(5 * time.Second)
	for _, roots := r.hub.newRoots("mac1"); len(roots) == 0; _, roots = r.hub.newRoots("mac1") {
		if time.Now().After(deadline) {
			t.Fatal("no roots in mac1's heartbeat")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.webAppData(`{"v":1,"target":"new@mac1:/etc:interactive"}`)
	r.tg.waitFor(t, "picker refusal", has("send", "⚠️ picker: /etc is outside the folders new sessions may use on mac1"))
	r.webAppData(`{"v":1,"target":"new@mac1:~/work/../.ssh:interactive"}`)
	r.tg.waitFor(t, "dot-dot refusal", has("send", "⚠️ picker: ~/work/../.ssh is outside"))
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "" {
		t.Errorf("target %q after refusals", tgt)
	}
	r.text("/target new@mac1:/etc:interactive")
	r.tg.waitFor(t, "forced", has("send", "Now talking to new interactive: etc · mac1"))
	r.text("make something")
	cs := r.tg.waitFor(t, "host refusal", has("send", "⚠️ new session on mac1 failed: folder /etc is outside the allowed folders"))
	if !has("send", "🎯 still new interactive: etc · mac1: send again to retry, or tap 🎯 for another folder")(cs) {
		t.Errorf("retry hint missing: %+v", lastWith(cs, "send", "failed"))
	}
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "new@mac1:/etc:interactive" {
		t.Errorf("target %q", tgt)
	}
	if n := r.sessionCount(); n != 0 {
		t.Errorf("%d sessions created", n)
	}
}

// A new -p session under a configured root outside home keeps working after
// its first run.
func TestNewPSessionUnderRootOutsideHome(t *testing.T) {
	outside := t.TempDir()
	r := startAgtermHost(t, func(c *host.Config) { c.NewRoots = append(c.NewRoots, outside) })
	r.text("/target new@mac1:" + outside + "/proj:-p")
	r.tg.waitFor(t, "switch", has("send", "Now talking to new -p: proj · mac1"))
	r.text("pwd")
	r.tg.waitFor(t, "first", has("send", "answer to [pwd]"))
	r.text("again")
	cs := r.tg.waitFor(t, "second", func(cs []call) bool { return has("send", "answer to [again]")(cs) || count(cs, "send", "⚠️") > 0 })
	if !has("send", "answer to [again]")(cs) {
		t.Errorf("second run refused: %+v", lastWith(cs, "send", "⚠️"))
	}
}

// A /spawn result stored by the previous build (no "created" field) still
// switches to the session it created.
func TestNewSessionResultFromPreviousBuild(t *testing.T) {
	r := newRig(t)
	old := `{"status":"ok","session":"` + newSess + `","brief_dir":"/Users/tester/agent-briefs/2026-11-01-old"}`
	j := &JobRow{ID: "oldspawn", EventID: "tg:1:old", Seq: 1, Host: "mac1", Lane: "session@mac1:", Target: "session@mac1:",
		Type: proto.JobNewSession, Payload: []byte(`{}`), ChatID: ownerUser, MsgID: 77}
	if _, err := r.st.AddDelivery(j, proto.Result{Status: proto.StatusOK, Data: []byte(old)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	stored, err := r.st.Job("oldspawn")
	if err != nil || stored == nil {
		t.Fatal(err)
	}
	r.hub.goDeliver(stored)
	cs := r.tg.waitFor(t, "a line", func(cs []call) bool { return has("send", "🆕")(cs) || has("send", "failed")(cs) })
	if has("send", "failed")(cs) {
		t.Errorf("an old successful result reported as failed: %q", lastWith(cs, "send", "failed").Text)
	}
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "session@mac1:"+newSess {
		t.Errorf("target %q", tgt)
	}
}

// A forced new -p folder (inside home, outside the roots) refused by the host:
// the target stays new@… and no later message runs claude -p there.
func TestNewPFolderOutsideRootsRefused(t *testing.T) {
	r := startAgtermHost(t)
	if err := os.MkdirAll(filepath.Join(r.dir, "secret"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.text("/target new@mac1:~/secret:-p")
	r.tg.waitFor(t, "switch", has("send", "Now talking to new -p: secret · mac1"))
	r.text("pwd")
	cs := r.tg.waitFor(t, "refusal", has("send", "is outside the allowed folders"))
	if l := lastWith(cs, "send", "is outside"); !strings.Contains(l.Text, "🎯 still new -p: secret · mac1: send again to retry") {
		t.Errorf("refusal line %q", l.Text)
	}
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "new@mac1:~/secret:-p" {
		t.Errorf("target %q after the refused first run", tgt)
	}
	r.text("again")
	cs = r.tg.waitFor(t, "second refusal", func(cs []call) bool {
		return count(cs, "send", "is outside the allowed folders") >= 2 || has("send", "answer to [again]")(cs)
	})
	if has("send", "answer to [again]")(cs) {
		t.Errorf("claude -p ran in the refused folder: %+v", lastWith(cs, "send", "answer to [again]"))
	}
}

// Row 8: a photo + caption as the first message: saved in the brief folder,
// path + caption typed.
func TestNewSessionFromPhoto(t *testing.T) {
	r := startAgtermHost(t)
	r.tg.mu.Lock()
	r.tg.files["ph-big"] = []byte("JPEGDATA")
	r.tg.mu.Unlock()
	r.text("/target new@mac1:~/work:interactive")
	r.tg.waitFor(t, "switch", has("send", "Now talking to new interactive"))
	id := r.send(func(m *models.Message) {
		m.Caption = "what is on this receipt"
		m.Photo = []models.PhotoSize{{FileID: "ph-small"}, {FileID: "ph-big"}}
	})
	r.tg.waitFor(t, "🆕", has("send", "🆕 what-receipt started on mac1"))
	brief := filepath.Join(r.dir, "agent-briefs", time.Now().Format("2006-01-02")+"-what-receipt")
	photo := filepath.Join(brief, fmt.Sprintf("photo-%d.jpg", id))
	if b, err := os.ReadFile(photo); err != nil || string(b) != "JPEGDATA" {
		t.Fatalf("photo %s: %q %v", photo, b, err)
	}
	typed := r.waitLines(t, newSess, 1)
	if !strings.Contains(typed[0], "[photo] ⏎ what is on this receipt") || !strings.Contains(typed[0], "[Image attached: "+photo+"]") {
		t.Errorf("typed %q", typed[0])
	}
	if b, _ := os.ReadFile(filepath.Join(brief, "brief.md")); !strings.HasPrefix(string(b), "what is on this receipt\n\n[photo: "+photo+"]") {
		t.Errorf("brief.md %q", b)
	}
}

// A composer that never gets ready: the session is closed, one error line,
// the target stays new@… for a retry.
func TestNewSessionComposerNeverReady(t *testing.T) {
	r := newRig(t)
	if err := os.MkdirAll(filepath.Join(r.dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	ar := r.runAgtermHost(&fakeAgterm{sessions: map[string]*fakeSess{}, newMenu: true})
	r.text("/target new@mac1:~/work:interactive")
	r.tg.waitFor(t, "switch", has("send", "Now talking to new interactive"))
	r.text("hello there")
	cs := r.tg.waitFor(t, "error", has("send", "⚠️ new session on mac1 failed: Claude's composer wasn't ready within 2m0s (it showed: a menu or dialog); the session was closed"))
	if !has("send", "send again to retry, or tap 🎯 for another folder")(cs) {
		t.Errorf("retry hint missing")
	}
	if n := ar.sessionCount(); n != 0 {
		t.Errorf("the session should be closed (%d left)", n)
	}
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "new@mac1:~/work:interactive" {
		t.Errorf("target %q", tgt)
	}
	// A line sent while that creation ran is not lost silently.
	if count(cs, "send", "🆕") != 0 {
		t.Error("no 🆕 line for a failed creation")
	}
}

// A line queued behind a creation that fails gets one "not sent" line.
func TestFollowUpOfFailedCreationSaysNotSent(t *testing.T) {
	r := newRig(t)
	r.text("/target new@mac1:/etc:interactive")
	r.tg.waitFor(t, "switch", has("send", "Now talking to new interactive"))
	id1 := r.text("first")
	id2 := r.text("second")
	r.tg.waitFor(t, "👀", func(cs []call) bool { return reactedOn(id1, "👀")(cs) && reactedOn(id2, "👀")(cs) })
	if n, _ := r.st.QueueDepth("mac1"); n != 2 {
		t.Fatalf("queued %d", n)
	}
	r.runAgtermHost(&fakeAgterm{sessions: map[string]*fakeSess{}})
	cs := r.tg.waitFor(t, "not sent", has("send", "wasn't sent"))
	if c := lastWith(cs, "send", "wasn't sent"); c.ReplyTo != id2 || !strings.Contains(c.Text, "the new session wasn't created") {
		t.Errorf("follow-up line %+v", c)
	}
}

func TestSlugFor(t *testing.T) {
	for in, want := range map[string]string{
		"create a file hello.txt containing hi, then tell me done": "create-file-hello-txt",
		"Найди мне электромобиль до 30 тысяч":                      "naydi-elektromobil-30-tysyach",
		"Café über naïve":             "cafe-uber-naive",
		"   ":                         "tg-7",
		"🙂":                           "tg-7",
		"please":                      "tg-7",
		strings.Repeat("x", 60):       strings.Repeat("x", 32),
		"one two three four five six": "one-two-three-four",
	} {
		if got := slugFor(in, 7); got != want {
			t.Errorf("slugFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// hooksReport makes mac1's host report these hook sessions in its heartbeats
// (as its hook relay would), sends one now and waits for the hub to store it.
// Writing them into the stored heartbeat instead raced the host's own.
func (ar *agtermRig) hooksReport(t *testing.T, hooks func() []proto.HookSession) {
	ar.h.SetHookSessions(hooks)
	ar.h.HeartbeatNow()
	want := len(hooks())
	ar.waitHeartbeat(func(hb proto.Heartbeat) bool { return len(hb.Hooks) == want })
}

// The 🆕 line carries the session's claude.ai link once its hooks report it
// (a host whose sessions have links: Remote Control on).
func TestNewSessionLineCarriesLink(t *testing.T) {
	r := newRig(t)
	if err := os.MkdirAll(filepath.Join(r.dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.hub.cfg.NewLinkWait = 5 * time.Second
	ar := r.runAgtermHost(&fakeAgterm{sessions: map[string]*fakeSess{}})
	r.waitFor(t, "the host's first heartbeat", func() bool { _, roots := r.hub.newRoots("mac1"); return len(roots) > 0 })
	var typed atomic.Bool
	old := proto.HookSession{AgtermID: sessA, Agent: "claude", BridgeID: "session_OLD", UpdatedAt: time.Now()}
	ar.hooksReport(t, func() []proto.HookSession {
		if typed.Load() {
			return []proto.HookSession{{AgtermID: newSess, Agent: "claude", BridgeID: "session_01ABC", UpdatedAt: time.Now()}, old}
		}
		return []proto.HookSession{old}
	})
	r.waitFor(t, "links reported", func() bool { return r.hub.hostHasLinks("mac1") })
	r.text("/target new@mac1:~/work:interactive")
	r.tg.waitFor(t, "switch", has("send", "Now talking to new interactive"))
	r.text("link me")
	ar.waitLines(t, newSess, 1)
	typed.Store(true)   // the first prompt's hook reports the bridge id
	ar.h.HeartbeatNow() // and a heartbeat brings it to the hub
	cs := r.tg.waitFor(t, "🆕", has("send", "🆕 link started on mac1"))
	if l := lastWith(cs, "send", "🆕"); !strings.HasSuffix(l.Text, "🔗 https://claude.ai/code/session_01ABC") {
		t.Errorf("🆕 line %q", l.Text)
	}
}

// On a host where no session has a link (Remote Control off) the 🆕 line
// doesn't wait for one.
func TestNewSessionLineNoWaitWithoutRemoteControl(t *testing.T) {
	r := newRig(t)
	if err := os.MkdirAll(filepath.Join(r.dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.hub.cfg.NewLinkWait = time.Hour
	r.runAgtermHost(&fakeAgterm{sessions: map[string]*fakeSess{}})
	r.text("/target new@mac1:~/work:interactive")
	r.tg.waitFor(t, "switch", has("send", "Now talking to new interactive"))
	r.text("no link here")
	cs := r.tg.waitFor(t, "🆕 at once", has("send", "🆕 link-here started on mac1"))
	if strings.Contains(lastWith(cs, "send", "🆕").Text, "🔗") {
		t.Error("no link expected")
	}
}

func (r *rig) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A new_session still queued (no result yet) must not hold or fail another
// session's report on the same host.
func TestStartNoticeIgnoresUnfinishedCreationsOfOtherMessages(t *testing.T) {
	r := newRig(t)
	id := r.text("hello new session")
	nj := &JobRow{ID: newID(), EventID: "tg:test:new-queued", Host: "mac1", Lane: "new:q", Target: "new@mac1:~/work:interactive",
		Type: proto.JobNewSession, Payload: []byte("{}"), ChatID: ownerUser, MsgID: id}
	if _, err := r.st.AddJob(nj, r.clk.Now()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	other := &JobRow{ID: newID(), Host: "mac1", ChatID: ownerUser, MsgID: id + 100}
	if err := r.hub.awaitStartNotice(ctx, other, "CAFE0000-0000"); err != nil {
		t.Fatalf("an unrelated session's report is held: %v", err)
	}
}

// 08 e2e: a session created on reconnect answered before its "🆕 … started"
// line. A hook report of a session being created waits for that line.
func TestStartNoticeComesBeforeTheSessionsFirstAnswer(t *testing.T) {
	r := newRig(t)
	id := r.text("hello new session")
	res := proto.Result{Status: proto.StatusOK, Data: mustJSONRaw(agterm.NewSessionResult{Status: proto.StatusOK,
		Session: newSess, Slug: "hello-new", BriefDir: "/x/agent-briefs/2026-11-01-hello-new", Created: true})}
	nj := &JobRow{ID: newID(), EventID: "tg:test:new", Host: "mac1", Lane: "new:x", Target: "new@mac1:~/work:interactive",
		Type: proto.JobNewSession, Payload: []byte("{}"), ChatID: ownerUser, MsgID: id}
	if _, err := r.st.AddDelivery(nj, res, r.clk.Now()); err != nil { // result in, 🆕 not out yet
		t.Fatal(err)
	}
	answer := func(hid string) proto.Event {
		data := mustJSONRaw(proto.HookAnswer{ID: hid, Agent: "claude", AgtermID: newSess, TgMsgID: int64(id), Text: "hi from " + hid})
		return proto.Event{JobID: "hook:" + hid, Kind: proto.EventHookAnswer, Data: data}
	}
	if err := r.hub.handleHookEvent("mac1", answer("a1")); err != nil {
		t.Fatal(err)
	}
	// the answer's delivery runs and holds: its job stays undelivered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hj, _ := r.st.JobByEventID("hook:mac1:a1")
	if err := r.hub.awaitStartNotice(ctx, hj, newSess); err == nil {
		t.Fatal("awaitStartNotice returned while the 🆕 line is pending")
	}
	other := *hj
	other.MsgID = id + 100 // a later message's answer from the same session: matched by session
	if err := r.hub.awaitStartNotice(ctx, &other, newSess); err == nil {
		t.Fatal("a later answer of the new session must wait too")
	}
	if err := r.hub.awaitStartNotice(ctx, &other, "CAFE0000-0000"); err != nil {
		t.Fatalf("an unrelated session waited: %v", err)
	}

	r.hub.goDeliver(nj)
	cs := r.tg.waitFor(t, "answer", has("send", "hi from a1"))
	iNew, iAns := -1, -1
	for i, c := range cs {
		if c.Kind == "send" && strings.HasPrefix(c.Text, "🆕 hello-new started on mac1") {
			iNew = i
		}
		if c.Kind == "send" && strings.Contains(c.Text, "hi from a1") {
			iAns = i
		}
	}
	if iNew < 0 || iNew > iAns {
		t.Fatalf("🆕 at %d, answer at %d: %+v", iNew, iAns, cs)
	}
	if err := r.hub.awaitStartNotice(ctx, hj, newSess); err != nil {
		t.Fatalf("after the 🆕 line: %v", err)
	}
}
