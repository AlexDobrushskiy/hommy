package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// logTail captures the standard logger for the test.
type logTail struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logTail) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logTail) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.buf.String(), s)
}

func (l *logTail) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.buf.String(), s)
}

func captureLog(t *testing.T) *logTail {
	lt := &logTail{}
	log.SetOutput(lt)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return lt
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// failWrites makes the named statement kind fail in SQLite while kv[key] is set.
func failWrites(t *testing.T, st *Store, key, trigger string) (on, off func()) {
	t.Helper()
	if _, err := st.db.Exec(fmt.Sprintf(trigger, key)); err != nil {
		t.Fatal(err)
	}
	return func() { st.Set(key, "1") }, func() { st.db.Exec(`DELETE FROM kv WHERE key=?`, key) }
}

// A photo queued before /new_session belongs to the old conversation; the
// message after the reset starts fresh.
func TestPhotoQueuedBeforeNewSessionKeepsItsGeneration(t *testing.T) {
	r := newRig(t)
	hr := r.startHost()
	r.text("remember me")
	r.tg.waitFor(t, "first", has("send", "answer to [remember me]"))
	hr.shutdown()
	r.waitConnected(false)

	r.tg.files["ph"] = []byte("JPEG")
	r.send(func(m *models.Message) { m.Caption = "describe"; m.Photo = []models.PhotoSize{{FileID: "ph"}} })
	r.text("/new_session")
	r.tg.waitFor(t, "reset", has("send", "session reset"))
	r.text("fresh question")
	r.startHost()
	cs := r.tg.waitFor(t, "both answers", func(cs []call) bool { return len(r.answers(cs)) == 3 })
	a := r.answers(cs)
	if !strings.Contains(a[1].Text, "[[photo]\ndescribe") || !strings.Contains(a[1].Text, "resume=sess-") {
		t.Errorf("the photo should continue the old conversation: %s", a[1].Text)
	}
	if !strings.Contains(a[2].Text, "[fresh question]") || !strings.Contains(a[2].Text, "resume=none") {
		t.Errorf("the message after /new_session should start fresh: %s", a[2].Text)
	}
}

// The hub stores the host's result and dies before anything else: the session
// the run ended on must still be the next run's resume.
func TestSessionKeptWhenHubDiesAfterResult(t *testing.T) {
	r := newRig(t)
	r.text("first")
	r.hub.drainEvents(context.Background())
	j, _ := r.st.OldestWaiting("mac1")
	res := proto.Result{JobID: j.ID, Status: proto.StatusOK,
		Data: mustJSONRaw(proto.RunPResult{Text: "answer to [first] in x resume=none", SessionID: "sess-crash"})}
	if ok, err := r.st.SetResult(j.ID, res, r.clk.Now()); !ok || err != nil {
		t.Fatal(ok, err)
	}
	r.stopHub()
	r.startHub()
	r.startHost()
	r.tg.waitFor(t, "recovered answer", has("send", "answer to [first]"))
	r.text("second")
	cs := r.tg.waitFor(t, "second", has("send", "answer to [second]"))
	if a := r.answers(cs)[1]; !strings.Contains(a.Text, "resume=sess-crash") {
		t.Errorf("second run should resume the recovered session: %s", a.Text)
	}
}

// A redeploy while a voice note downloads leaves the message to be processed
// after the restart, not dropped.
func TestShutdownMidDownloadKeepsTheMessage(t *testing.T) {
	r := newRig(t)
	started := make(chan struct{}, 1)
	r.tg.setDownloadHook(func(ctx context.Context, id string) ([]byte, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	r.send(func(m *models.Message) { m.Voice = &models.Voice{FileID: "v1"} })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download never started")
	}
	r.stopHub()
	r.tg.setDownloadHook(nil)
	r.tg.files["v1"] = []byte("OggS")
	r.startHub()
	r.startHost()
	cs := r.tg.waitFor(t, "answer", has("send", "answer to [[voice message, transcribed]"))
	if count(cs, "send", "download failed") != 0 {
		t.Errorf("a shutdown is not a download failure: %+v", cs)
	}
}

// A Telegram send that fails transiently is retried; chunks already sent are
// not sent again.
func TestAnswerSendFailureIsRetried(t *testing.T) {
	r := newRig(t)
	r.startHost()
	var mu sync.Mutex
	fails := map[string]int{"answer to [flaky]": 2, "tail-chunk": 1}
	r.tg.setFailSend(func(text string) error {
		mu.Lock()
		defer mu.Unlock()
		for k, n := range fails {
			if n > 0 && (strings.HasPrefix(text, k) || (k == "tail-chunk" && strings.HasPrefix(text, "x x") && strings.Contains(text, "TAIL"))) {
				fails[k] = n - 1
				return errors.New("connection reset by peer")
			}
		}
		return nil
	})
	r.text("flaky")
	r.tg.waitFor(t, "flaky answer", has("send", "answer to [flaky]"))

	long := "long " + strings.Repeat("x ", 2500) + "TAIL"
	r.text(long)
	r.tg.waitFor(t, "tail chunk", has("send", "TAIL"))
	r.text("after")
	cs := r.tg.waitFor(t, "after", has("send", "answer to [after]"))
	if n := count(cs, "send", "answer to [flaky]"); n != 1 {
		t.Errorf("flaky answered %d times", n)
	}
	if n, m := count(cs, "send", "answer to [long x"), count(cs, "send", "TAIL"); n != 1 || m != 1 {
		t.Errorf("long answer: head sent %d times, tail %d times", n, m)
	}
}

// A send Telegram refuses for good doesn't hold the lane forever.
func TestAnswerPermanentSendFailureMovesOn(t *testing.T) {
	r := newRig(t)
	r.startHost()
	r.tg.setFailSend(func(text string) error {
		if strings.HasPrefix(text, "answer to [refused]") {
			return fmt.Errorf("%w, message is too long", bot.ErrorBadRequest)
		}
		return nil
	})
	r.text("refused")
	r.text("next")
	r.tg.waitFor(t, "next", has("send", "answer to [next]"))
}

// A result the hub fails to store is replayed once storage works again.
func TestResultStoreFailureIsReplayed(t *testing.T) {
	lt := captureLog(t)
	r := newRig(t)
	on, off := failWrites(t, r.st, "fail_result", `CREATE TRIGGER fail_result BEFORE UPDATE OF result ON jobs
		WHEN EXISTS(SELECT 1 FROM kv WHERE key='%s') BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`)
	on()
	r.startHost()
	r.text("stored late")
	eventually(t, "a failed result write", func() bool { return lt.has("store result") })
	off()
	cs := r.tg.waitFor(t, "answer", has("send", "answer to [stored late]"))
	if n := count(cs, "send", "answer to [stored late]"); n != 1 {
		t.Errorf("answered %d times", n)
	}
}

// fakeBotAPI is a Bot API stand-in for the poller and the file download.
type fakeBotAPI struct {
	mu      sync.Mutex
	offsets []int64
	batch   []json.RawMessage // returned once by getUpdates
	stall   chan struct{}     // file downloads hang until closed
}

func (f *fakeBotAPI) requested() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.offsets...)
}

func (f *fakeBotAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ok := func(v any) { json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": v}) }
	switch {
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		ok(map[string]any{"id": 1, "is_bot": true, "first_name": "dev"})
	case strings.HasSuffix(r.URL.Path, "/getUpdates"):
		var p struct {
			Offset int64 `json:"offset"`
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			r.ParseMultipartForm(1 << 20)
			p.Offset, _ = strconv.ParseInt(r.FormValue("offset"), 10, 64)
		} else {
			json.NewDecoder(r.Body).Decode(&p)
		}
		f.mu.Lock()
		f.offsets = append(f.offsets, p.Offset)
		batch := f.batch
		f.batch = nil
		f.mu.Unlock()
		if batch == nil {
			select { // a short long-poll
			case <-r.Context().Done():
			case <-time.After(5 * time.Millisecond):
			}
			batch = []json.RawMessage{}
		}
		ok(batch)
	case strings.HasSuffix(r.URL.Path, "/getFile"):
		ok(map[string]any{"file_id": "f1", "file_path": "voice/f1.ogg"})
	case strings.Contains(r.URL.Path, "/file/bot"):
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OggS"))
		w.(http.Flusher).Flush()
		select {
		case <-f.stall:
		case <-r.Context().Done():
		}
	default:
		ok(true)
	}
}

// Telegram must not be told an update was received until the hub has stored it.
func TestPollerConfirmsOnlyStoredUpdates(t *testing.T) {
	lt := captureLog(t)
	r := newRig(t)
	on, off := failWrites(t, r.st, "fail_events", `CREATE TRIGGER fail_events BEFORE INSERT ON events
		WHEN EXISTS(SELECT 1 FROM kv WHERE key='%s') BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`)
	on()
	upd, _ := json.Marshal(models.Update{ID: 10, Message: &models.Message{ID: 55, Text: "one",
		From: &models.User{ID: ownerUser}, Chat: models.Chat{ID: ownerUser, Type: models.ChatTypePrivate}}})
	api := &fakeBotAPI{batch: []json.RawMessage{upd}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.hub.poll(ctx, srv.URL, "TOKEN", []string{"message"}); close(done) }()
	defer func() { cancel(); <-done }()

	confirmed := func() bool {
		for _, o := range api.requested() {
			if o == 11 {
				return true
			}
		}
		return false
	}
	eventually(t, "a confirmation or repeated store attempts", func() bool {
		return confirmed() || lt.count("store update 10") >= 3
	})
	if confirmed() {
		t.Fatalf("update 10 confirmed (offset 11) before it was stored; offsets %v", api.requested())
	}
	off()
	eventually(t, "the stored update to be confirmed", confirmed)
	var n int
	r.st.db.QueryRow(`SELECT COUNT(*) FROM events WHERE event_id='tg:1001:55'`).Scan(&n)
	if n != 1 {
		t.Errorf("update stored %d times", n)
	}
}

// A file download that stalls mid-body gives up instead of blocking the event queue.
func TestDownloadStallTimesOut(t *testing.T) {
	api := &fakeBotAPI{stall: make(chan struct{})}
	srv := httptest.NewServer(api)
	defer srv.Close()
	defer close(api.stall)
	tg, err := newBotTelegram("TOKEN", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tg.downloadTimeout = 50 * time.Millisecond
	errc := make(chan error, 1)
	go func() { _, err := tg.Download(context.Background(), "f1"); errc <- err }()
	select {
	case err := <-errc:
		if err == nil {
			t.Error("a stalled download must fail")
		} else if strings.Contains(err.Error(), "TOKEN") {
			t.Errorf("the error goes to the chat and must not carry the token: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download still blocked after 5s")
	}
}
