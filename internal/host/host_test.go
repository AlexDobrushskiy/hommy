package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/claude"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

type frames struct {
	mu sync.Mutex
	fs []proto.Frame
	ch chan struct{}
}

func (f *frames) add(fr proto.Frame) {
	f.mu.Lock()
	f.fs = append(f.fs, fr)
	f.mu.Unlock()
	f.ch <- struct{}{}
}

func (f *frames) waitN(t *testing.T, kind string, n int) []proto.Frame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		f.mu.Lock()
		var out []proto.Frame
		for _, fr := range f.fs {
			if fr.Type == kind {
				out = append(out, fr)
			}
		}
		f.mu.Unlock()
		if len(out) >= n {
			return out
		}
		select {
		case <-f.ch:
		case <-deadline:
			t.Fatalf("want %d %s frames, have %d", n, kind, len(out))
		}
	}
}

func newTestHost(t *testing.T, dir string) (*Host, *frames) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "vault"), 0o755)
	h, err := New(Config{Name: "mac1", DataDir: filepath.Join(dir, "data"), VaultDir: filepath.Join(dir, "vault"),
		MediaDir: filepath.Join(dir, "media"), Home: dir, ClaudeBin: "claude", ClaudeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	f := &frames{ch: make(chan struct{}, 100)}
	h.sendHook = f.add
	return h, f
}

func runPJob(id, event, prompt string) proto.Job {
	p, _ := json.Marshal(proto.RunP{Prompt: prompt})
	return proto.Job{ID: id, EventID: event, Type: proto.JobRunP, Payload: p}
}

func stubClaude(t *testing.T, fn func(opts claude.StreamOptions, sid, prompt string) (string, string, error)) {
	stubStream(t, func(_ context.Context, o claude.StreamOptions, sid, p string) (claude.StreamResult, error) {
		text, s, err := fn(o, sid, p)
		return claude.StreamResult{Text: text, SessionID: s}, err
	})
}

func stubStream(t *testing.T, fn func(ctx context.Context, o claude.StreamOptions, sid, prompt string) (claude.StreamResult, error)) {
	old := streamClaude
	streamClaude = fn
	t.Cleanup(func() { streamClaude = old })
}

func TestRunPOnceThenLedger(t *testing.T) {
	dir := t.TempDir()
	runs := 0
	release := make(chan struct{})
	var mu sync.Mutex
	stubClaude(t, func(o claude.StreamOptions, sid, p string) (string, string, error) {
		mu.Lock()
		runs++
		mu.Unlock()
		<-release
		return "hi " + p, "s1", nil
	})
	h, f := newTestHost(t, dir)
	ctx := context.Background()
	h.receive(ctx, runPJob("j1", "e1", "x"))
	f.waitN(t, proto.FrameEvent, 1)
	h.receive(ctx, runPJob("j1", "e1", "x")) // re-pushed while running: not run again, started re-sent
	f.waitN(t, proto.FrameEvent, 2)
	close(release)
	res := f.waitN(t, proto.FrameResult, 1)[0].Result
	var d proto.RunPResult
	json.Unmarshal(res.Data, &d)
	if res.Status != proto.StatusOK || d.Text != "hi x" || d.SessionID != "s1" || res.JobID != "j1" {
		t.Fatalf("result %+v %+v", res, d)
	}

	// After a host restart the ledger answers a re-push without running it.
	h.Close()
	h2, f2 := newTestHost(t, dir)
	h2.receive(ctx, runPJob("j1", "e1", "x"))
	if r := f2.waitN(t, proto.FrameResult, 1)[0].Result; r.Status != proto.StatusOK {
		t.Errorf("ledger result %+v", r)
	}
	if runs != 1 {
		t.Errorf("ran %d times", runs)
	}
}

func TestUnknownJobTypeIsError(t *testing.T) {
	h, f := newTestHost(t, t.TempDir())
	h.receive(context.Background(), proto.Job{ID: "j", EventID: "e", Type: "shell", Payload: json.RawMessage(`{"cmd":"rm -rf /"}`)})
	r := f.waitN(t, proto.FrameResult, 1)[0].Result
	if r.Status != proto.StatusError || !strings.Contains(r.Error, `unknown job type "shell"`) {
		t.Errorf("%+v", r)
	}
}

func TestRunPErrorKeepsSessionAndPanicIsCaught(t *testing.T) {
	stubClaude(t, func(o claude.StreamOptions, sid, p string) (string, string, error) {
		if p == "panic" {
			panic("boom")
		}
		return "", "s9", os.ErrDeadlineExceeded
	})
	h, f := newTestHost(t, t.TempDir())
	h.receive(context.Background(), runPJob("a", "ea", "fail"))
	h.receive(context.Background(), runPJob("b", "eb", "panic"))
	rs := f.waitN(t, proto.FrameResult, 2)
	for _, fr := range rs {
		if fr.Result.Status != proto.StatusError {
			t.Errorf("%+v", fr.Result)
		}
		if fr.Result.JobID == "a" && !strings.Contains(string(fr.Result.Data), "s9") {
			t.Errorf("error result should carry the session id: %s", fr.Result.Data)
		}
	}
}

func TestRunPDirMustBeUnderHome(t *testing.T) {
	dir := t.TempDir()
	var gotDir string
	stubClaude(t, func(o claude.StreamOptions, sid, p string) (string, string, error) {
		gotDir = o.Dir
		return "ok", "", nil
	})
	h, f := newTestHost(t, dir)
	os.MkdirAll(filepath.Join(dir, "briefs"), 0o755)
	os.Symlink("/etc", filepath.Join(dir, "escape"))
	for i, c := range []struct {
		dir string
		ok  bool
	}{{"~/briefs", true}, {"/etc", false}, {"~/escape", false}, {"~/missing", false}, {"relative", false}, {"~/../..", false}} {
		p, _ := json.Marshal(proto.RunP{Prompt: "x", Dir: c.dir})
		id := string(rune('a' + i))
		h.receive(context.Background(), proto.Job{ID: id, EventID: "e" + id, Type: proto.JobRunP, Payload: p})
		r := f.waitN(t, proto.FrameResult, i+1)[i].Result
		if (r.Status == proto.StatusOK) != c.ok {
			t.Errorf("dir %q: %+v", c.dir, r)
		}
	}
	real, _ := filepath.EvalSymlinks(filepath.Join(dir, "briefs"))
	if gotDir != real {
		t.Errorf("ran in %q, want %q", gotDir, real)
	}
}

func TestSaveFile(t *testing.T) {
	dir := t.TempDir()
	h, f := newTestHost(t, dir)
	for i, ext := range []string{".jpg", "../../x", ".pdf"} {
		p, _ := json.Marshal(proto.SaveFile{Ext: ext, Data: []byte("data" + ext)})
		id := string(rune('a' + i))
		h.receive(context.Background(), proto.Job{ID: id, EventID: "tg:1:" + id, Type: proto.JobSaveFile, Payload: p})
		r := f.waitN(t, proto.FrameResult, i+1)[i].Result
		var d proto.SaveFileResult
		json.Unmarshal(r.Data, &d)
		if r.Status != proto.StatusOK || filepath.Dir(d.Path) != filepath.Join(dir, "media") {
			t.Fatalf("%q: %+v", ext, r)
		}
		if b, _ := os.ReadFile(d.Path); string(b) != "data"+ext {
			t.Errorf("content %q", b)
		}
		if ext == "../../x" && !strings.HasSuffix(d.Path, ".bin") {
			t.Errorf("bad ext must become .bin: %s", d.Path)
		}
	}
}

func TestLedgerSurvivesTornLineAndExpires(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	now := time.Now()
	l, _ := OpenLedger(path, now.Add(-40*24*time.Hour))
	l.Put("old", proto.Result{Status: "ok"}, now.Add(-40*24*time.Hour))
	l.Put("new", proto.Result{Status: "ok"}, now)
	l.Close()
	fh, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fh.WriteString(`{"event_id":"torn","at":`)
	fh.Close()
	l2, err := OpenLedger(path, now)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if _, ok := l2.Get("new"); !ok {
		t.Error("recent entry lost")
	}
	if _, ok := l2.Get("old"); ok {
		t.Error("expired entry kept")
	}
	l2.Put("after", proto.Result{Status: "ok"}, now)
	if _, ok := l2.Get("after"); !ok {
		t.Error("append after rewrite")
	}
}

func TestBackoff(t *testing.T) {
	for _, c := range []struct {
		attempt  int
		r        float64
		min, max time.Duration
	}{{0, 0, 500 * time.Millisecond, 500 * time.Millisecond}, {0, 1, time.Second, time.Second},
		{3, 0.5, 6 * time.Second, 6 * time.Second}, {20, 1, time.Minute, time.Minute}, {20, 0, 30 * time.Second, 30 * time.Second}} {
		if d := backoff(c.attempt, c.r); d < c.min || d > c.max {
			t.Errorf("backoff(%d,%v)=%s", c.attempt, c.r, d)
		}
	}
	for _, c := range []struct {
		attempt int
		r       float64
		want    time.Duration
	}{{0, 0, 500 * time.Millisecond}, {2, 1, 4 * time.Second}, {20, 1, 8 * time.Second}, {20, 0, 4 * time.Second}} {
		if d := reconnectBackoff(c.attempt, c.r); d != c.want {
			t.Errorf("reconnectBackoff(%d,%v)=%s, want %s", c.attempt, c.r, d, c.want)
		}
	}
}

func TestCountClaudeAndAgentFolders(t *testing.T) {
	ps := "/Users/tester/.local/bin/claude\n/Applications/Claude.app/Contents/MacOS/Claude\nclaude\n/Users/a/.local/share/claude/versions/2.1.316\n/bin/zsh\n"
	if n := countClaude(ps); n != 3 {
		t.Errorf("count %d", n)
	}
	dir := t.TempDir()
	for i, n := range []string{"2026-a", "2026-b", ".hidden"} {
		p := filepath.Join(dir, n)
		os.Mkdir(p, 0o755)
		ts := time.Now().Add(time.Duration(i) * time.Minute)
		os.Chtimes(p, ts, ts)
	}
	os.WriteFile(filepath.Join(dir, "file"), nil, 0o644)
	if got := agentFolders(dir, 5); len(got) != 2 || got[0] != "2026-b" {
		t.Errorf("folders %v", got)
	}
}

func TestLoadConfig(t *testing.T) {
	for k, v := range map[string]string{"HOST_NAME": "mac1", "HUB_URL": "ws://127.0.0.1:1/ws", "HOST_DATA_DIR": "~/state",
		"MEDIA_DIR": "", "CLAUDE_TIMEOUT": "", "VAULT_DIR": "", "AGENT_COMMAND": "/opt/x/claude --flag",
		"AGENT_PERMISSIONS": "skip", "AGTERM_WORKSPACE": "ws-1", "CLAUDE_CONFIG_DIR": ""} {
		t.Setenv(k, v)
		if v == "" {
			os.Unsetenv(k)
		}
	}
	key := strings.Repeat("k", 32)
	cfg, err := LoadConfig(func() (string, error) { return key + "\n", nil })
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if cfg.Name != "mac1" || cfg.Key != key || cfg.DataDir != filepath.Join(home, "state") ||
		cfg.MediaDir != filepath.Join(home, "state/media") || cfg.ClaudeTimeout != 30*time.Minute ||
		cfg.Workspace != "ws-1" || cfg.VaultDir != filepath.Join(home, "hommy-vault") {
		t.Errorf("%+v", cfg)
	}
	want := "/bin/zsh -l -c 'env HOME=" + home + " /opt/x/claude --flag --dangerously-skip-permissions; exec /bin/zsh -l'"
	if got := cfg.AgentCommandLine(); got != want {
		t.Errorf("command line\n got %s\nwant %s", got, want)
	}
	t.Setenv("AGENT_PERMISSIONS", "ask")
	t.Setenv("CLAUDE_CONFIG_DIR", "/c d/cfg")
	cfg, _ = LoadConfig(func() (string, error) { return key, nil })
	want = "/bin/zsh -l -c 'env HOME=" + home + " CLAUDE_CONFIG_DIR='\\''/c d/cfg'\\'' /opt/x/claude --flag; exec /bin/zsh -l'"
	if got := cfg.AgentCommandLine(); got != want {
		t.Errorf("ask command line\n got %s\nwant %s", got, want)
	}
	if _, err := LoadConfig(func() (string, error) { return "short", nil }); err == nil {
		t.Error("short key must fail")
	}
}

func (h *Host) inflightCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.inflight)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A re-push that arrives just as the original run finishes must not run the job again.
func TestRepushRacingCompletionRunsOnce(t *testing.T) {
	var mu sync.Mutex
	runs := 0
	release := make(chan struct{})
	stubClaude(t, func(o claude.StreamOptions, sid, p string) (string, string, error) {
		mu.Lock()
		runs++
		mu.Unlock()
		<-release
		return "done", "s1", nil
	})
	h, f := newTestHost(t, t.TempDir())
	ctx := context.Background()
	h.receive(ctx, runPJob("j1", "e1", "x"))
	f.waitN(t, proto.FrameEvent, 1)

	// The re-push looks for a result, then the original run completes before it claims the job.
	h.beforeClaim = func() {
		h.beforeClaim = nil
		close(release)
		f.waitN(t, proto.FrameResult, 1)
		waitUntil(t, "original run to finish", func() bool { return h.inflightCount() == 0 })
	}
	h.receive(ctx, runPJob("j1", "e1", "x"))
	f.waitN(t, proto.FrameResult, 2) // answered from the ledger
	waitUntil(t, "no job running", func() bool { return h.inflightCount() == 0 })
	mu.Lock()
	defer mu.Unlock()
	if runs != 1 {
		t.Errorf("ran %d times", runs)
	}
}

// A result is acked only once it is in the ledger; until then a re-push doesn't run it again.
func TestResultAckedOnlyAfterLedgerWrite(t *testing.T) {
	var mu sync.Mutex
	runs := 0
	var h *Host
	stubClaude(t, func(o claude.StreamOptions, sid, p string) (string, string, error) {
		mu.Lock()
		runs++
		mu.Unlock()
		h.ledger.f.Close() // the ledger file stops taking writes
		return "done", "s1", nil
	})
	dir := t.TempDir()
	h, f := newTestHost(t, dir)
	retrying, gate := make(chan struct{}, 10), make(chan struct{})
	h.waitRetry = func(ctx context.Context, attempt int) bool {
		retrying <- struct{}{}
		<-gate
		return true
	}
	ctx := context.Background()
	h.receive(ctx, runPJob("j1", "e1", "x"))
	select {
	case <-retrying:
	case <-time.After(5 * time.Second):
		t.Fatal("a failed ledger write should be retried")
	}
	h.receive(ctx, runPJob("j1", "e1", "x")) // re-pushed while the result isn't durable yet
	f.mu.Lock()
	for _, fr := range f.fs {
		if fr.Type == proto.FrameResult {
			t.Errorf("result acked before it was in the ledger: %+v", fr.Result)
		}
	}
	f.mu.Unlock()

	close(gate)
	if r := f.waitN(t, proto.FrameResult, 1)[0].Result; r.Status != proto.StatusOK || r.JobID != "j1" {
		t.Errorf("result %+v", r)
	}
	waitUntil(t, "no job running", func() bool { return h.inflightCount() == 0 })
	h.Close()
	h2, f2 := newTestHost(t, dir) // the retried write made it to disk
	h2.receive(ctx, runPJob("j1", "e1", "x"))
	f2.waitN(t, proto.FrameResult, 1)
	mu.Lock()
	defer mu.Unlock()
	if runs != 1 {
		t.Errorf("ran %d times", runs)
	}
}
