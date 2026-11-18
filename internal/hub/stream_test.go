package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

func lastIndex(cs []call, kind, substr string) int {
	for i := len(cs) - 1; i >= 0; i-- {
		if cs[i].Kind == kind && strings.Contains(cs[i].Text, substr) {
			return i
		}
	}
	return -1
}

func firstOf(cs []call, kind, substr string) call {
	for _, c := range cs {
		if c.Kind == kind && strings.Contains(c.Text, substr) {
			return c
		}
	}
	return call{}
}

func (r *rig) gate() func() {
	p := filepath.Join(r.dir, "gate")
	r.t.Setenv("HC_TEST_GATE", p)
	return func() { os.WriteFile(p, nil, 0o644) }
}

func (r *rig) tapStop(draftID int) {
	r.nextMs++
	r.hub.intake(&models.Update{ID: int64(r.nextMs), StoppedMessageGeneration: &models.MessageGenerationStopped{
		Chat: models.Chat{ID: ownerUser, Type: models.ChatTypePrivate}, DraftID: draftID}})
}

// A run streams into one draft (status line above the text), then the answer
// goes out as a message and no draft follows it.
func TestRunStreamsDraftThenAnswer(t *testing.T) {
	r := newRig(t)
	release := r.gate()
	r.startHost()
	id := r.text("count GATE")
	cs := r.tg.waitFor(t, "first draft", has("draft", "✍ writing\n\n1"))
	d := firstOf(cs, "draft", "1")
	release()
	r.tg.waitFor(t, "answer", has("send", "answer to [count GATE]"))
	cs = r.tg.snapshot()
	if d.MsgID == 0 || lastIndex(cs, "draft", "") > lastIndex(cs, "send", "answer to [count GATE]") {
		t.Errorf("draft id %d; no draft may follow the answer: %+v", d.MsgID, cs)
	}
	for _, c := range cs {
		if c.Kind == "draft" && c.MsgID != d.MsgID {
			t.Errorf("one draft id per run: %+v", c)
		}
	}
	if a := r.answers(cs)[0]; a.ReplyTo != id {
		t.Errorf("answer %+v", a)
	}
}

// Stop: the draft is dropped, the run is cancelled on the host, one
// "⏹ stopped" line with the partial text arrives, never an answer; the next
// message resumes the stopped run's session.
func TestStopCancelsRunAndSessionStaysUsable(t *testing.T) {
	r := newRig(t)
	r.startHost()
	id := r.text("tell a long story SLOW")
	cs := r.tg.waitFor(t, "draft", has("draft", "partial"))
	draft := firstOf(cs, "draft", "partial").MsgID
	r.tapStop(draft)
	cs = r.tg.waitFor(t, "stopped line", has("send", "⏹ stopped"))
	stopped := firstOf(cs, "send", "⏹ stopped")
	if stopped.Text != "⏹ stopped\n\npartial" || stopped.ReplyTo != id {
		t.Errorf("stopped line %+v", stopped)
	}
	r.tapStop(draft) // tapped twice: ignored
	r.text("and now?")
	cs = r.tg.waitFor(t, "next answer", has("send", "answer to [and now?]"))
	if n := count(cs, "send", "⏹ stopped"); n != 1 || count(cs, "send", "answer to [tell") != 0 {
		t.Errorf("want one stopped line, no answer for the stopped run: %+v", cs)
	}
	if !regexp.MustCompile(`resume=sess-\d+`).MatchString(r.answers(cs)[0].Text) {
		t.Errorf("next run should resume the stopped session: %s", r.answers(cs)[0].Text)
	}
	if i, j := lastIndex(cs, "draft", "partial"), lastIndex(cs, "send", "⏹ stopped"); i > j {
		t.Errorf("draft after the stop: %+v", cs)
	}
}

func TestStopForUnknownDraftIgnored(t *testing.T) {
	r := newRig(t)
	r.startHost()
	r.tapStop(4242)
	r.text("hello")
	cs := r.tg.waitFor(t, "answer", has("send", "answer to [hello]"))
	if count(cs, "send", "stopped") != 0 {
		t.Errorf("%+v", cs)
	}
	if jobs, _ := r.st.JobsInStatus(JobQueued); len(jobs) != 0 {
		t.Errorf("no cancel job for an unknown draft: %+v", jobs)
	}
}

// A hub restart mid-stream: the host keeps running, the stream resumes from
// the host's re-sent snapshot, and exactly one answer arrives.
func TestHubRestartMidStreamAnswersOnce(t *testing.T) {
	r := newRig(t)
	release := r.gate()
	r.startHost()
	r.text("count GATE")
	r.tg.waitFor(t, "draft", has("draft", "1"))
	r.stopHub()
	before := len(r.tg.snapshot())
	r.startHub()
	r.waitConnected(true)
	r.tg.waitFor(t, "draft after restart", func(cs []call) bool { return count(cs[before:], "draft", "1") > 0 })
	release()
	r.tg.waitFor(t, "answer", has("send", "answer to [count GATE]"))
	r.text("sync")
	cs := r.tg.waitFor(t, "sync", has("send", "answer to [sync]"))
	if n := count(cs, "send", "answer to [count GATE]"); n != 1 {
		t.Errorf("answered %d times", n)
	}
}

// On 429 the stream waits and then sends only the newest text.
func TestDraft429KeepsNewest(t *testing.T) {
	r := newRig(t)
	j := &JobRow{ID: "j1", ChatID: ownerUser, DraftID: 77, Type: proto.JobRunP}
	in, out := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.tg.setFailDraft(func(string) error {
		var err error
		once.Do(func() {
			in <- struct{}{}
			<-out
			err = &bot.TooManyRequestsError{Message: "too many", RetryAfter: 0}
		})
		return err
	})
	r.hub.streamProgress(j, proto.Progress{Seq: 1, Status: "✍ writing", Text: "a"})
	<-in // the first draft is in flight
	r.hub.streamProgress(j, proto.Progress{Seq: 2, Status: "✍ writing", Text: "ab"})
	r.hub.streamProgress(j, proto.Progress{Seq: 3, Status: "✍ writing", Text: "abc"})
	r.hub.streamProgress(j, proto.Progress{Seq: 2, Status: "✍ writing", Text: "stale"})
	close(out) // → 429
	r.tg.waitFor(t, "draft", has("draft", "abc"))
	r.hub.closeStream(j.ID)
	r.hub.streamProgress(j, proto.Progress{Seq: 4, Status: "✍ writing", Text: "abcd"}) // closed: never shown
	cs := r.tg.snapshot()
	if count(cs, "draft", "") != 1 || cs[0].Text != "✍ writing\n\nabc" || cs[0].MsgID != 77 {
		t.Errorf("want only the newest draft: %+v", cs)
	}
}

func TestRenderDraft(t *testing.T) {
	if got := renderDraft(proto.Progress{Status: "⚙ Read · inbox/a.md"}); got != "⚙ Read · inbox/a.md" {
		t.Errorf("%q", got)
	}
	if got := renderDraft(proto.Progress{Status: "✍ writing", Text: "hello\n<tts>spoken"}); got != "✍ writing\n\nhello" {
		t.Errorf("tts block must not show: %q", got)
	}
	long := renderDraft(proto.Progress{Status: "✍ writing", Text: strings.Repeat("x", 5000) + "END"})
	if n := len([]rune(long)); n > draftLimit || !strings.HasSuffix(long, "END") || !strings.Contains(long, "\n\n…x") {
		t.Errorf("long draft keeps its tail within the limit: %d runes", n)
	}
}

// A stop pressed while a voice note is still being downloaded/transcribed
// must not wait for it: the run keeps going (and may finish) meanwhile.
func TestStopNotQueuedBehindVoiceTranscription(t *testing.T) {
	r := newRig(t)
	r.startHost()
	r.text("tell a long story SLOW")
	cs := r.tg.waitFor(t, "draft", has("draft", "partial"))
	draft := firstOf(cs, "draft", "partial").MsgID
	release := make(chan struct{})
	r.tg.setDownloadHook(func(ctx context.Context, id string) ([]byte, error) {
		select {
		case <-release:
			return []byte("ogg"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	r.send(func(m *models.Message) { m.Voice = &models.Voice{FileID: "v1"} })
	r.tapStop(draft)
	r.tg.waitFor(t, "stopped line while the voice note is still downloading", has("send", "⏹ stopped"))
	close(release)
	r.tg.waitFor(t, "voice note answered after", has("send", "answer to [[voice message"))
}

func runPDraftID(t *testing.T, st *Store) int {
	t.Helper()
	var id int
	if err := st.db.QueryRow(`SELECT draft_id FROM jobs WHERE type='run_p' ORDER BY seq DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A slow sendMessageDraft call must not hold up the cancel: the host is told
// at once, and the "⏹ stopped" line still lands after the hanging draft.
func TestStopCancelsWhileDraftCallHangs(t *testing.T) {
	r := newRig(t)
	r.startHost()
	in, out := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.tg.setFailDraft(func(text string) error {
		if strings.Contains(text, "partial") {
			once.Do(func() { close(in); <-out })
		}
		return nil
	})
	r.text("tell a long story SLOW")
	select {
	case <-in:
	case <-time.After(10 * time.Second):
		t.Fatal("no draft call")
	}
	r.tapStop(runPDraftID(t, r.st))
	eventually(t, "the run stopped on the host while the draft call hangs", func() bool {
		var status string
		r.st.db.QueryRow(`SELECT status FROM jobs WHERE type='run_p'`).Scan(&status)
		return status == JobResult
	})
	close(out)
	cs := r.tg.waitFor(t, "stopped line", has("send", "⏹ stopped"))
	if i, j := lastIndex(cs, "draft", ""), lastIndex(cs, "send", "⏹ stopped"); i > j {
		t.Errorf("draft after the stopped line: %+v", cs)
	}
}

// Every closeStream caller waits for an in-flight draft call, not just the
// first: stop and delivery may both close a stream at once.
func TestCloseStreamCallersAllWaitForInFlightDraft(t *testing.T) {
	r := newRig(t)
	j := &JobRow{ID: "j1", ChatID: ownerUser, DraftID: 77, Type: proto.JobRunP}
	in, out := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.tg.setFailDraft(func(string) error {
		once.Do(func() { close(in); <-out })
		return nil
	})
	r.hub.streamProgress(j, proto.Progress{Seq: 1, Status: "✍ writing", Text: "a"})
	<-in
	first := make(chan struct{})
	go func() { r.hub.closeStream(j.ID); close(first) }()
	eventually(t, "first closer took the stream", func() bool {
		r.hub.streamsMu.Lock()
		defer r.hub.streamsMu.Unlock()
		_, closed := r.hub.streamClosed[j.ID]
		return closed
	})
	second := make(chan struct{})
	go func() { r.hub.closeStream(j.ID); close(second) }()
	select {
	case <-second:
		t.Error("a second closeStream returned while a draft call was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(out)
	<-first
	<-second
}

// A stop that was accepted before the result was stored wins, even when the
// delivering code holds a row read before the stop.
func TestStopAcceptedBeforeResultWinsOverStaleRow(t *testing.T) {
	r := newRig(t)
	now := time.Now()
	j := &JobRow{ID: "j1", EventID: "tg:1", Host: "mac1", Lane: "vault@mac1", Target: "vault@mac1",
		Type: proto.JobRunP, Payload: []byte(`{}`), ChatID: ownerUser, MsgID: 5, DraftID: 77}
	if _, err := r.st.AddJob(j, now); err != nil {
		t.Fatal(err)
	}
	stale, _ := r.st.Job(j.ID) // read by handleResult before the stop lands
	if ok, err := r.st.RequestStop(j.ID); !ok || err != nil {
		t.Fatalf("stop %v %v", ok, err)
	}
	res := proto.Result{JobID: j.ID, Status: proto.StatusOK,
		Data: mustJSONRaw(proto.RunPResult{Text: "the whole story <tts>spoken</tts>", SessionID: "s1"})}
	if stored, err := r.st.SetResult(j.ID, res, now); !stored || err != nil {
		t.Fatalf("result %v %v", stored, err)
	}
	if err := r.hub.deliverRunP(context.Background(), stale, res); err != nil {
		t.Fatal(err)
	}
	cs := r.tg.snapshot()
	if count(cs, "send", "⏹ stopped") != 1 || count(cs, "voice", "") != 0 {
		t.Errorf("want the stopped line and no voice: %+v", cs)
	}
}

// Telegram sends stopped_message_generation.draft_id as a string (seen on the
// dev bot 2026-11-01, update 900000001); go-telegram's model wants an int. The
// poller must still route it as a stop.
func TestStopUpdateWithStringDraftIDThroughPoller(t *testing.T) {
	r := newRig(t)
	r.startHost()
	r.text("tell a long story SLOW")
	cs := r.tg.waitFor(t, "draft", has("draft", "partial"))
	draft := firstOf(cs, "draft", "partial").MsgID
	raw := json.RawMessage(fmt.Sprintf(`{"update_id":900000001,"stopped_message_generation":{"chat":{"id":%d,`+
		`"first_name":"Owner","username":"testowner","type":"private"},"draft_id":"%d"}}`, ownerUser, draft))
	srv := httptest.NewServer(&fakeBotAPI{batch: []json.RawMessage{raw}})
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.hub.poll(ctx, srv.URL, "TOKEN", nil); close(done) }()
	defer func() { cancel(); <-done }()
	r.tg.waitFor(t, "stopped line", has("send", "⏹ stopped"))
}
