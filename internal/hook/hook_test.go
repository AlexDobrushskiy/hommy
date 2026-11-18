package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRelay is a host listening on the hook socket that records the bodies.
type fakeRelay struct {
	mu     sync.Mutex
	bodies []string
}

func (f *fakeRelay) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

func listenFake(t *testing.T) (Config, *fakeRelay) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hk") // unix socket paths are short on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "hook.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRelay{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		f.mu.Unlock()
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return Config{Socket: sock, Spool: filepath.Join(dir, "hookspool")}, f
}

func hookEnv(k string) string {
	return map[string]string{"AGTERM_SESSION_ID": agID, "CLAUDE_CODE_BRIDGE_SESSION_ID": "session_q"}[k]
}

func spooled(t *testing.T, cfg Config) []string {
	t.Helper()
	names, _ := filepath.Glob(filepath.Join(cfg.Spool, "*.json"))
	var out []string
	for _, n := range names {
		b, _ := os.ReadFile(n)
		out = append(out, string(b))
	}
	return out
}

// A turn typed at the Mac stays in the session: its prompt text and its answer
// never leave the hook process, live or spooled.
func TestRunLocalTurnSendsNoContent(t *testing.T) {
	cfg, f := listenFake(t)
	run := func(c Config, ev, in string) {
		t.Helper()
		if _, err := Run(context.Background(), c, AgentClaude, ev, strings.NewReader(in), hookEnv, time.Now); err != nil {
			t.Fatalf("Run(%s): %v", ev, err)
		}
	}
	run(cfg, EventUserPromptSubmit, `{"session_id":"s1","cwd":"/w","prompt":"local secret prompt"}`)
	run(cfg, EventStop, `{"session_id":"s1","last_assistant_message":"local secret answer"}`)
	run(cfg, EventNotify, `{"session_id":"s1","notification_type":"permission_prompt","message":"local secret notice"}`)
	for _, b := range f.got() {
		if strings.Contains(b, "local secret") {
			t.Fatalf("local turn content reached the host: %s", b)
		}
	}
	if n := len(f.got()); n != 1 {
		t.Fatalf("host got %d requests for a local turn, want 1 (the prompt's session info): %q", n, f.got())
	}

	// Host down: nothing of a local turn is written to disk either.
	down := Config{Socket: cfg.Socket + ".gone", Spool: cfg.Spool}
	run(down, EventUserPromptSubmit, `{"session_id":"s2","prompt":"local secret prompt"}`)
	run(down, EventStop, `{"session_id":"s2","last_assistant_message":"local secret answer"}`)
	if s := spooled(t, down); len(s) != 0 {
		t.Fatalf("a local turn was spooled: %q", s)
	}
}

// A Telegram turn still reaches the host, prompt and answer included.
func TestRunTelegramTurnReachesTheHost(t *testing.T) {
	cfg, f := listenFake(t)
	for _, c := range []struct{ ev, in string }{
		{EventUserPromptSubmit, `{"session_id":"s1","prompt":"[Telegram · tg-5] hi"}`},
		{EventStop, `{"session_id":"s1","last_assistant_message":"tg answer"}`},
		{EventUserPromptSubmit, `{"session_id":"s1","prompt":"local next"}`}, // ends the turn at the host
		{EventStop, `{"session_id":"s1","last_assistant_message":"local secret answer"}`},
	} {
		if _, err := Run(context.Background(), cfg, AgentClaude, c.ev, strings.NewReader(c.in), hookEnv, time.Now); err != nil {
			t.Fatal(err)
		}
	}
	got := f.got()
	if len(got) != 3 || !strings.Contains(got[0], "tg-5") || !strings.Contains(got[1], "tg answer") ||
		strings.Contains(got[2], "local next") {
		t.Fatalf("host got %q", got)
	}
}

// Claude Code waits for a hook: a stdin that never closes must not hold the session.
func TestRunGivesUpOnAStdinThatNeverCloses(t *testing.T) {
	cfg, _ := listenFake(t)
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		Run(ctx, cfg, AgentClaude, EventStop, pr, hookEnv, time.Now)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run still reading stdin after its deadline")
	}
}

// Without last_assistant_message the answer is read from the transcript when
// the hook runs: a spooled Stop replayed later must not pick up a newer turn.
func TestRunCapturesTheTranscriptAnswerBeforeSpooling(t *testing.T) {
	rr := newRelayRig(t)
	cfg := Config{Socket: filepath.Join(rr.dir, "nobody.sock"), Spool: filepath.Join(rr.dir, "spool")}
	tr := filepath.Join(rr.dir, "t.jsonl")
	write := func(lines ...string) {
		f, _ := os.OpenFile(tr, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		f.WriteString(strings.Join(lines, "\n") + "\n")
		f.Close()
	}
	write(`{"type":"user","message":{"role":"user","content":"[Telegram · tg-8] q"}}`,
		`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"the telegram answer"}]}}`)
	at := rr.now
	clock := func() time.Time { at = at.Add(time.Millisecond); return at }
	for _, c := range []struct{ ev, in string }{
		{EventUserPromptSubmit, `{"session_id":"s1","transcript_path":"` + tr + `","prompt":"[Telegram · tg-8] q"}`},
		{EventStop, `{"session_id":"s1","transcript_path":"` + tr + `"}`},
	} {
		if _, err := Run(context.Background(), cfg, AgentClaude, c.ev, strings.NewReader(c.in), hookEnv, clock); err != nil {
			t.Fatal(err)
		}
	}
	// A later local turn lands in the transcript before the host is back.
	write(`{"type":"user","message":{"role":"user","content":"local"}}`,
		`{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"a local answer"}]}}`)
	if err := rr.r.DrainSpool(); err != nil {
		t.Fatal(err)
	}
	if got := rr.emitted(); len(got) != 1 || got[0].A.Text != "the telegram answer" {
		t.Fatalf("emitted %+v", got)
	}
}

const taskNote = "<task-notification>\n<task-id>b0fzqr5zc</task-id>\n<status>completed</status>\n<summary>Background command \"Wait 20 seconds\" completed (exit code 0)</summary>\n</task-notification>"

// Row 7 (blind e2e r1): the Telegram turn ran `sleep 20` in the background;
// its completion came as a UserPromptSubmit. The turn's Stop must still reach
// the host, and the notification's text must not.
func TestRunTaskNotificationContinuesTheTelegramTurn(t *testing.T) {
	cfg, f := listenFake(t)
	run := func(ev, in, want string) {
		t.Helper()
		what, err := Run(context.Background(), cfg, AgentClaude, ev, strings.NewReader(in), hookEnv, time.Now)
		if err != nil || what != want {
			t.Fatalf("Run(%s %s) = %q, %v; want %q", ev, in, what, err, want)
		}
	}
	note, _ := json.Marshal(taskNote)
	run(EventUserPromptSubmit, `{"session_id":"s1","prompt_id":"p1","prompt":"[Telegram · tg-605] run sleep 20"}`, "sent")
	run(EventUserPromptSubmit, `{"session_id":"s1","prompt_id":"p2","prompt":`+string(note)+`}`, "sent")
	run(EventStop, `{"session_id":"s1","prompt_id":"p1","last_assistant_message":"OUTAGE_ONCE_R1_06"}`, "sent")
	run(EventUserPromptSubmit, `{"session_id":"s1","prompt_id":"p3","prompt":"typed at the Mac"}`, "sent")
	run(EventStop, `{"session_id":"s1","last_assistant_message":"local"}`, "local turn")
	got := f.got()
	if len(got) != 4 || !strings.Contains(got[1], `"system_prompt":true`) || strings.Contains(got[1], "task-id") ||
		!strings.Contains(got[2], "OUTAGE_ONCE_R1_06") || strings.Contains(got[3], "system_prompt") {
		t.Fatalf("host got %q", got)
	}
}

// Who wrote a prompt: the transcript's origin (written before the hook runs)
// beats the text; without one, a <task-notification> is the system's.
func TestPromptIsFromAPerson(t *testing.T) {
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(tr, []byte(strings.Join([]string{
		`{"type":"user","promptId":"h1","origin":{"kind":"human"},"message":{"role":"user","content":"<task-notification> pasted by hand"}}`,
		`{"type":"user","promptId":"c1","origin":{"kind":"scheduled-task"},"message":{"role":"user","content":"check the build"}}`,
		`{"type":"attachment","attachment":{"type":"queued_command","prompt":"mid-turn note","origin":{"kind":"task-notification"}}}`,
	}, "\n")+"\n"), 0o600)
	for _, c := range []struct {
		id, prompt string
		human      bool
	}{
		{"h1", "<task-notification> pasted by hand", true},
		{"c1", "check the build", false},
		{"x", "mid-turn note", false},
		{"x", taskNote, false},     // not in the transcript: by its text
		{"x", "typed later", true}, // not in the transcript, plain text
		// Absorbed mid-turn (seen live): the notification's prompt hook
		// carries the turn's prompt id, whose entry is the person's prompt,
		// and runs before its attachment is written.
		{"h1", taskNote, false},
	} {
		if got := promptFromPerson(tr, c.id, c.prompt); got != c.human {
			t.Errorf("promptFromPerson(%s, %.20q) = %v, want %v", c.id, c.prompt, got, c.human)
		}
	}
}

// A Stop handed to the host (or its durable spool) consumes the Telegram
// marker. Later background wakes and their notices/answers stay local.
func TestRunStopEndsTelegramTurn(t *testing.T) {
	for _, down := range []bool{false, true} {
		t.Run(fmt.Sprint("hostDown=", down), func(t *testing.T) {
			cfg, f := listenFake(t)
			if down {
				cfg.Socket += ".gone"
			}
			run := func(ev, in string) string {
				t.Helper()
				what, err := Run(context.Background(), cfg, AgentClaude, ev, strings.NewReader(in), hookEnv, time.Now)
				if err != nil {
					t.Fatal(err)
				}
				return what
			}
			run(EventUserPromptSubmit, `{"session_id":"s1","prompt":"[Telegram · tg-5] start"}`)
			run(EventStop, `{"session_id":"s1","last_assistant_message":"STARTED"}`)
			if fileExists(filepath.Join(TurnsDir(cfg), markerName("s1"))) {
				t.Fatal("Stop left Telegram marker")
			}
			note, _ := json.Marshal(taskNote)
			run(EventUserPromptSubmit, `{"session_id":"s1","prompt":`+string(note)+`}`)
			for _, ev := range []string{EventStop, EventNotify} {
				if got := run(ev, `{"session_id":"s1","last_assistant_message":"PRIVATE_WAKE","message":"PRIVATE_NOTICE"}`); got != "local turn" {
					t.Fatalf("%s = %s", ev, got)
				}
			}
			bodies := f.got()
			if down {
				bodies = spooled(t, cfg)
			}
			for _, b := range bodies {
				if strings.Contains(b, "PRIVATE_") {
					t.Fatalf("local content escaped: %s", b)
				}
			}
			// A later Telegram line starts its own turn normally.
			run(EventUserPromptSubmit, `{"session_id":"s1","prompt":"[Telegram · tg-6] again"}`)
			run(EventStop, `{"session_id":"s1","last_assistant_message":"SECOND"}`)
		})
	}
}

func TestRunFailedStopKeepsTelegramMarker(t *testing.T) {
	cfg, _ := listenFake(t)
	run := func(ev, in string) error {
		_, err := Run(context.Background(), cfg, AgentClaude, ev, strings.NewReader(in), hookEnv, time.Now)
		return err
	}
	if err := run(EventUserPromptSubmit, `{"session_id":"s1","prompt":"[Telegram · tg-5] hi"}`); err != nil {
		t.Fatal(err)
	}
	cfg.Socket += ".gone"
	cfg.Spool = filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(cfg.Spool, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stop := `{"session_id":"s1","last_assistant_message":"RETRY_ME"}`
	if err := run(EventStop, stop); err == nil {
		t.Fatal("want failed delivery")
	}
	if !fileExists(filepath.Join(TurnsDir(cfg), markerName("s1"))) {
		t.Fatal("failed Stop consumed marker")
	}
	cfg.Spool = filepath.Join(t.TempDir(), "spool")
	if err := run(EventStop, stop); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(TurnsDir(cfg), markerName("s1"))) {
		t.Fatal("durable retry left marker")
	}
	if got := spooled(t, cfg); len(got) != 1 || !strings.Contains(got[0], "RETRY_ME") {
		t.Fatalf("retry = %q", got)
	}
}
