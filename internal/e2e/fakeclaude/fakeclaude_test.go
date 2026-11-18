package fakeclaude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clock is a fake clock: After advances it at once, so `slow:` takes no wall-clock time.
type clock struct {
	now   time.Time
	waits int
}

func (c *clock) Now() time.Time { return c.now }
func (c *clock) After(d time.Duration) <-chan time.Time {
	c.waits++
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

type result struct {
	code           int
	stdout, stderr string
}

func run(t *testing.T, home string, env map[string]string, stdin string, args ...string) (result, *clock) {
	t.Helper()
	c := &clock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	var out, errb bytes.Buffer
	get := func(k string) string {
		if k == "HOME" {
			return home
		}
		return env[k]
	}
	code := Run(Env{Args: args, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, Getenv: get,
		Cwd: home, Now: c.Now, After: c.After,
		Exec: func(path string, argv []string, _ []string) error {
			out.WriteString("EXEC " + path + " " + strings.Join(argv[1:], " "))
			return nil
		}})
	return result{code, out.String(), errb.String()}, c
}

// event is the part of a stream-json line the source host parses.
type event struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Event     json.RawMessage `json:"event"`
	Message   struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

func events(t *testing.T, s string) []event {
	t.Helper()
	var out []event
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("not JSON: %q", sc.Text())
		}
		out = append(out, e)
	}
	return out
}

var hostArgs = []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--dangerously-skip-permissions",
	"--append-system-prompt", "be brief"}

func deltas(t *testing.T, evs []event) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Type != "stream_event" {
			continue
		}
		var se struct {
			Type  string `json:"type"`
			Delta struct {
				Type, Text string
			} `json:"delta"`
		}
		_ = json.Unmarshal(e.Event, &se)
		if se.Type == "content_block_delta" {
			b.WriteString(se.Delta.Text)
		}
	}
	return b.String()
}

func TestStreamModes(t *testing.T) {
	home := t.TempDir()
	cases := map[string]string{"echo: hello there": "hello there", "tts: hi": "hi\n<tts>hi</tts>", "what's up": "ok"}
	for prompt, want := range cases {
		r, _ := run(t, home, nil, prompt, hostArgs...)
		if r.code != 0 {
			t.Fatalf("%q: exit %d %s", prompt, r.code, r.stderr)
		}
		evs := events(t, r.stdout)
		first, last := evs[0], evs[len(evs)-1]
		if first.Type != "system" || first.Subtype != "init" || last.Type != "result" || last.Subtype != "success" ||
			last.Result != want || last.IsError || last.SessionID != first.SessionID {
			t.Fatalf("%q: %+v … %+v", prompt, first, last)
		}
		if got := deltas(t, evs); got != want {
			t.Fatalf("%q: deltas %q", prompt, got)
		}
		var asst *event
		for i := range evs {
			if evs[i].Type == "assistant" {
				asst = &evs[i]
			}
		}
		if asst == nil || asst.Message.Content[0].Text != want {
			t.Fatalf("%q: assistant event %+v", prompt, asst)
		}
	}
}

func TestSlowUsesTheClock(t *testing.T) {
	r, c := run(t, t.TempDir(), nil, "slow: 2", hostArgs...)
	evs := events(t, r.stdout)
	if r.code != 0 || c.waits != 4 || !strings.HasPrefix(evs[len(evs)-1].Result, "step 1. step 2.") {
		t.Fatalf("slow: exit %d, %d ticks, %q", r.code, c.waits, evs[len(evs)-1].Result)
	}
	if strings.Count(deltas(t, evs), "step") != 5 {
		t.Fatalf("partial text: %q", deltas(t, evs))
	}
}

func TestFailAndFormats(t *testing.T) {
	home := t.TempDir()
	if r, _ := run(t, home, nil, "fail: boom", hostArgs...); r.code != 1 || !strings.Contains(r.stderr, "boom") {
		t.Fatalf("fail: %+v", r)
	}
	r, _ := run(t, home, nil, "echo: x", "-p", "--output-format", "json", "--dangerously-skip-permissions")
	var res event
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil || res.Type != "result" || res.Result != "x" || res.SessionID == "" {
		t.Fatalf("json: %v %q", err, r.stdout)
	}
	if r, _ := run(t, home, nil, "", "-p", "echo: arg prompt"); r.stdout != "arg prompt\n" {
		t.Fatalf("text: %q", r.stdout)
	}
	if r, _ := run(t, home, nil, "x", "-p", "--output-format", "stream-json"); r.code != 1 || !strings.Contains(r.stderr, "requires --verbose") {
		t.Fatalf("stream-json without --verbose: %+v", r)
	}
}

func TestResumeChain(t *testing.T) {
	home := t.TempDir()
	r, _ := run(t, home, nil, "one", hostArgs...)
	sid := events(t, r.stdout)[0].SessionID
	args := append(append([]string{}, hostArgs...), "--resume", sid)
	r, _ = run(t, home, nil, "two", args...)
	evs := events(t, r.stdout)
	var last map[string]any
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	_ = json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	if evs[0].SessionID != sid || last["num_turns"] != float64(2) {
		t.Fatalf("resume: %s vs %s, turns %v", evs[0].SessionID, sid, last["num_turns"])
	}
	bad := append(append([]string{}, hostArgs...), "--resume", "00000000-0000-4000-8000-000000000000")
	if r, _ := run(t, home, nil, "x", bad...); r.code != 1 || !strings.Contains(r.stderr, "No conversation found") {
		t.Fatalf("unknown resume: %+v", r)
	}
}

func TestVersionAuthForward(t *testing.T) {
	home := t.TempDir()
	if r, _ := run(t, home, nil, "", "--version"); r.stdout != Version+" (Claude Code)\n" {
		t.Fatalf("--version: %q", r.stdout)
	}
	r, _ := run(t, home, nil, "", "auth", "status")
	var st map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &st); err != nil || st["loggedIn"] != true || r.code != 0 ||
		st["configDirectory"] != filepath.Join(home, ".claude") {
		t.Fatalf("auth status: %v %q", err, r.stdout)
	}
	if r, _ := run(t, home, nil, "", "auth", "status", "--text"); r.code != 0 || !strings.Contains(r.stdout, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("auth status --text: %q", r.stdout)
	}
	r, _ = run(t, home, map[string]string{"HOMMY_CLAUDE_BIN": "/opt/real/claude"}, "", "plugin", "list")
	if r.stdout != "EXEC /opt/real/claude plugin list" {
		t.Fatalf("plugin forward: %q", r.stdout)
	}
	if r, _ := run(t, home, map[string]string{"PATH": t.TempDir()}, "", "mcp", "list"); r.code != 1 {
		t.Fatalf("mcp without a real claude: %+v", r)
	}
}

func TestArgvLogHasNoEnv(t *testing.T) {
	home := t.TempDir()
	logPath := filepath.Join(home, "argv.jsonl")
	run(t, home, map[string]string{"HOMMY_STUB_LOG": logPath, "SECRET_TOKEN": "s3cr3t-value"}, "echo: hi", hostArgs...)
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var line struct {
		Argv []string `json:"argv"`
	}
	if err := json.Unmarshal(b, &line); err != nil || len(line.Argv) != len(hostArgs) || strings.Contains(string(b), "s3cr3t") {
		t.Fatalf("argv log: %v %s", err, b)
	}
}

func TestModeTextSkipsTheHubsEnvelope(t *testing.T) {
	for in, want := range map[string]string{
		"[voice message, transcribed]\ntts: weather is fine": "tts: weather is fine",
		"[photo]\necho: x\n\n[Image attached: /p]":           "echo: x\n\n[Image attached: /p]",
		"[Telegram · tg-5] echo: hi":                         "echo: hi",
		"echo: plain":                                        "echo: plain",
		"[not a header] echo: y":                             "[not a header] echo: y",
	} {
		if got := modeText(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
