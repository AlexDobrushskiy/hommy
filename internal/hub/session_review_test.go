package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

func inboxRecords(t *testing.T, r *agtermRig, session string) []agterm.InboxRecord {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, "inbox", session+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []agterm.InboxRecord
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec agterm.InboxRecord
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

// A reply's message id and quote reach the inbox, for text and for photos.
func TestSessionLinesKeepReplyMetadata(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.add(sessA)
	r.text("/target session@mac1:" + sessA)
	r.tg.waitFor(t, "switch", has("edit", "🎯 Talking to: "))
	id := r.send(func(m *models.Message) {
		m.Text = "yes, that one"
		m.ReplyToMessage = &models.Message{ID: 5, Text: "🤖 which car?"}
	})
	r.tg.waitFor(t, "✍ text", reactedOn(id, "✍"))
	r.tg.mu.Lock()
	r.tg.files["ph-1"] = []byte("\xff\xd8 jpeg")
	r.tg.mu.Unlock()
	pid := r.send(func(m *models.Message) {
		m.Photo = []models.PhotoSize{{FileID: "ph-1", Width: 10, Height: 10}}
		m.Caption = "this one"
		m.ReplyToMessage = &models.Message{ID: 6, Text: "🤖 send a photo"}
	})
	r.tg.waitFor(t, "✍ photo", reactedOn(pid, "✍"))
	recs := inboxRecords(t, r, sessA)
	if len(recs) != 2 {
		t.Fatalf("inbox = %+v", recs)
	}
	if recs[0].ReplyTo != 5 || recs[0].Quote != "🤖 which car?" || recs[0].Kind != "text" {
		t.Errorf("text record = %+v", recs[0])
	}
	if recs[1].ReplyTo != 6 || recs[1].Quote != "🤖 send a photo" || recs[1].Kind != "photo" {
		t.Errorf("photo record = %+v", recs[1])
	}
}

// A session created whose first message then failed says so.
func TestSpawnReportsAFirstMessageThatFailed(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.failType = true
	r.text("/spawn mac1:~/work ev-new find me a car")
	cs := r.tg.waitFor(t, "warning", has("send", "⚠️"))
	if !has("send", "BEEF0001")(cs) {
		t.Errorf("the warning should name the session that was created: %+v", cs)
	}
}

// A queued line typed while the hub is unreachable still gets its ✍ once the
// host reconnects.
func TestLateOutcomeReachesTelegramAfterAReconnect(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.addMenu(sessA)
	r.text("/target session@mac1:" + sessA)
	id := r.text("go on")
	r.tg.waitFor(t, "👌", reactedOn(id, "👌"))
	r.stopHub()
	r.ag.setMenu(sessA, false)
	r.clock.Sleep(agterm.RetryEvery)
	r.svc.Queue.Tick(r.ctx)
	if got := r.ag.submitted(sessA); len(got) != 1 {
		t.Fatalf("typed %q", got)
	}
	r.startHub()
	r.waitConnected(true)
	r.tg.waitFor(t, "late ✍ after reconnect", reactedOn(id, "✍"))
	// the hub's ack empties the host's outbox
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Join(r.dir, "host", "outbox.json"))
		if s := strings.TrimSpace(string(b)); s == "[]" || s == "null" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbox still holds %s", b)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The host repeats an outcome until the hub acks it; a repeat says nothing twice.
func TestRepeatedTypeOutcomeIsSaidOnce(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.addMenu(sessA)
	r.text("/target session@mac1:" + sessA)
	id := r.text("go on")
	r.tg.waitFor(t, "👌", reactedOn(id, "👌"))
	jobs, err := r.st.queryJobs(`SELECT `+jobCols+` FROM jobs WHERE msg_id=? AND type=?`, id, proto.JobType)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	data, _ := json.Marshal(proto.TypeOutcome{Outcome: "expired"})
	ev := proto.Event{JobID: jobs[0].ID, Kind: proto.EventTypeOutcome, Data: data}
	for i := 0; i < 2; i++ {
		if err := r.hub.handleTypeOutcome(r.ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(r.tg.snapshot(), "send", "⌛"); n != 1 {
		t.Fatalf("⌛ sent %d times", n)
	}
}

const sessB = "BBBB2222-0000-0000-0000-000000000000"

// Lines held for an offline host reach it in the owner's order, one at a time, even
// for different sessions (replies to an ended agent's ping and to its
// successor's name different sessions of one agent): the next line isn't
// pushed while the one before it is still being typed.
func TestHeldLinesForAHostArePushedInOrderOneAtATime(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.add(sessA)
	r.ag.add(sessB)
	r.text("/target session@mac1:" + sessA)
	r.tg.waitFor(t, "switch A", has("send", "Now talking to"))
	r.stop()
	r.waitConnected(false)
	one := r.text("one")
	r.tg.waitFor(t, "👀 one", reactedOn(one, "👀"))
	r.text("/target session@mac1:" + sessB)
	r.tg.waitFor(t, "switch B", func(cs []call) bool { return count(cs, "send", "Now talking to") == 2 })
	two := r.text("two")
	r.tg.waitFor(t, "👀 two", reactedOn(two, "👀"))

	entered, release := make(chan struct{}), make(chan struct{})
	var block sync.Once
	r.ag.mu.Lock()
	r.ag.onType = func(id, _ string) {
		if id == sessA {
			block.Do(func() { close(entered); <-release })
		}
	}
	r.ag.mu.Unlock()
	r.runAgtermHost(r.ag) // reconnects; the hub pushes what it held (line one blocks in the host)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("line one never typed")
	}
	jobs, err := r.st.queryJobs(`SELECT `+jobCols+` FROM jobs WHERE msg_id=? AND type=?`, two, proto.JobType)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	status := jobs[0].Status
	close(release)
	if status != JobQueued {
		t.Errorf("line two was %s while line one was being typed", status)
	}
	r.tg.waitFor(t, "✍ both", func(cs []call) bool { return reactedOn(one, "✍")(cs) && reactedOn(two, "✍")(cs) })
	if a, b := r.ag.submitted(sessA), r.ag.submitted(sessB); len(a) != 1 || len(b) != 1 {
		t.Fatalf("typed A %q B %q", a, b)
	}
}
