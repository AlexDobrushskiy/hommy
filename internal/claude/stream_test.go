package claude

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testdata/stream.jsonl is a real `claude -p --output-format stream-json
// --verbose --include-partial-messages` run (2.1.307): a Bash call, then text.
func TestParserOnRealStream(t *testing.T) {
	f, err := os.Open("testdata/stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := newParser("/v")
	var statuses, texts []string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if p.feed(sc.Bytes()) {
			pr := p.progress()
			if len(statuses) == 0 || statuses[len(statuses)-1] != pr.Status {
				statuses = append(statuses, pr.Status)
			}
			if len(texts) == 0 || texts[len(texts)-1] != pr.Text {
				texts = append(texts, pr.Text)
			}
		}
	}
	want := []string{"💭 thinking", "⚙ Bash", "⚙ Bash · Show first line of hosts file", "💭 thinking", "✍ writing"}
	if strings.Join(statuses, "|") != strings.Join(want, "|") {
		t.Errorf("statuses %q", statuses)
	}
	if len(texts) < 5 || !strings.HasPrefix(texts[2], "1\n2\n3\n4") {
		t.Errorf("text should grow delta by delta: %q", texts)
	}
	if !p.done || p.isError || p.sessionID != "00000000-0000-4000-8000-000000000001" || p.cost == 0 ||
		!strings.HasPrefix(p.result, "1\n2\n3\n4\n5\n\nThe first line") || p.result != p.text {
		t.Errorf("final state %+v", p)
	}
}

func TestParserNewMessageRestartsTextAndSkipsSubagents(t *testing.T) {
	p := newParser("")
	for _, l := range []string{
		`{"type":"stream_event","event":{"type":"message_start"},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Let me look."}},"parent_tool_use_id":null}`,
		`{"type":"stream_event","event":{"type":"message_start"},"parent_tool_use_id":"toolu_1"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"subagent chatter"}},"parent_tool_use_id":"toolu_1"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/home/x/vault/inbox/a.md"}}]}}`,
		`not json`,
	} {
		p.feed([]byte(l))
	}
	if p.text != "Let me look." || p.status != "⚙ Read · /home/x/vault/inbox/a.md" {
		t.Errorf("%+v", p.progress())
	}
	p.dir = "/home/x/vault"
	p.feed([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/home/x/vault/inbox/a.md"}}]}}`))
	p.feed([]byte(`{"type":"stream_event","event":{"type":"message_start"},"parent_tool_use_id":null}`))
	if p.status != "⚙ Read · inbox/a.md" || p.text != "Let me look." {
		t.Errorf("text stays until the next message writes: %+v", p.progress())
	}
	p.feed([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Done."}},"parent_tool_use_id":null}`))
	if p.text != "Done." {
		t.Errorf("new message text %q", p.text)
	}
}

func TestCleanEnv(t *testing.T) {
	got := CleanEnv([]string{"PATH=/bin", "CLAUDE_CODE_SESSION_ID=x", "CLAUDECODE=1", "CLAUDE_CONFIG_DIR=/c",
		"CLAUDE_CODE_BRIDGE_SESSION_ID=y", "CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_OAUTH_TOKEN=t",
		"ANTHROPIC_API_KEY=k", "HOME=/h"})
	if strings.Join(got, " ") != "PATH=/bin CLAUDE_CONFIG_DIR=/c CLAUDE_CODE_OAUTH_TOKEN=t ANTHROPIC_API_KEY=k HOME=/h" {
		t.Errorf("%q", got)
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A real child process: env is clean, args are stream-json, the result and
// session come from the stream.
func TestStreamRunsCleanEnvAndParses(t *testing.T) {
	bin := writeScript(t, `cat >/dev/null
vars=$(env | grep '^CLAUDE' | cut -d= -f1 | sort | tr '\n' ' ')
echo '{"type":"system","subtype":"init","session_id":"s-1"}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}}'
printf '{"type":"result","subtype":"success","is_error":false,"result":"args=%s vars=%s","session_id":"s-1","total_cost_usd":0.5}\n' "$*" "$vars"
`)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "leak")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg")
	var progress []Progress
	res, err := Stream(context.Background(), StreamOptions{Bin: bin, Dir: t.TempDir(),
		OnProgress: func(p Progress) { progress = append(progress, p) }}, "old", "prompt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "--output-format stream-json --verbose --include-partial-messages") ||
		!strings.Contains(res.Text, "--resume old") || res.SessionID != "s-1" || res.CostUSD != 0.5 {
		t.Errorf("%+v", res)
	}
	if !strings.Contains(res.Text, "CLAUDE_CONFIG_DIR") || strings.Contains(res.Text, "CLAUDE_CODE_") || strings.Contains(res.Text, "CLAUDECODE") {
		t.Errorf("CLAUDE_CODE_* / CLAUDECODE must not reach claude, CLAUDE_CONFIG_DIR must: %s", res.Text)
	}
	if len(progress) == 0 || progress[len(progress)-1].Text != "hi" {
		t.Errorf("progress %+v", progress)
	}
}

// Cancel kills the whole process group, escalating to SIGKILL when SIGTERM
// is ignored, and reports the cancel cause with the partial text.
func TestStreamCancelKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	bin := writeScript(t, `trap '' TERM
cat >/dev/null
sleep 300 &
echo $! > `+pidFile+`
echo '{"type":"system","subtype":"init","session_id":"s-2"}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}}'
wait
`)
	cause := errors.New("stopped")
	ctx, cancel := context.WithCancelCause(context.Background())
	started := make(chan int, 1)
	done := make(chan struct{})
	var res StreamResult
	var err error
	go func() {
		defer close(done)
		res, err = Stream(ctx, StreamOptions{Bin: bin, Dir: t.TempDir(), KillGrace: 50 * time.Millisecond,
			OnStart: func(pgid int) { started <- pgid },
			OnProgress: func(p Progress) {
				if p.Text == "partial" {
					cancel(cause)
				}
			}}, "", "x")
	}()
	pgid := <-started
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stream didn't return after cancel")
	}
	if !errors.Is(err, cause) || res.Text != "partial" || res.SessionID != "s-2" {
		t.Errorf("err=%v res=%+v", err, res)
	}
	b, _ := os.ReadFile(pidFile)
	child, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if child == 0 {
		t.Fatal("no child pid")
	}
	for _, pid := range []int{pgid, child} {
		if !gone(pid, 3*time.Second) {
			t.Errorf("process %d survived the cancel", pid)
		}
	}
}

// alive reports a live (non-zombie) process.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err == nil {
		return !strings.Contains(string(out), ") Z ")
	}
	return true
}

// Claude Code runs each Bash-tool command in a process group of its own, so
// killing claude's group alone leaves the tool running. Cancel must reach it
// whether claude ignores SIGTERM (escalation) or exits on it without
// cleaning up.
func TestStreamCancelKillsToolProcessGroups(t *testing.T) {
	for name, tail := range map[string]string{
		"term ignored": "trap '' TERM\nwait\n",
		"term obeyed":  "exec sleep 300\n",
	} {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "tool.pid")
			bin := writeScript(t, `cat >/dev/null
perl -e 'setpgrp(0,0); open(my $f, ">", $ARGV[0]); print $f "$$\n"; close $f; sleep 300' `+pidFile+` &
echo '{"type":"system","subtype":"init","session_id":"s-4"}'
`+tail)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				Stream(ctx, StreamOptions{Bin: bin, Dir: t.TempDir(), KillGrace: 100 * time.Millisecond,
					OnProgress: func(Progress) {
						for !fileHasPID(pidFile) {
							time.Sleep(5 * time.Millisecond)
						}
						cancel()
					}}, "", "x")
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Stream didn't return after cancel")
			}
			b, _ := os.ReadFile(pidFile)
			tool, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			if tool == 0 {
				t.Fatal("no tool pid")
			}
			t.Cleanup(func() { syscall.Kill(tool, syscall.SIGKILL) })
			if !gone(tool, 3*time.Second) {
				t.Errorf("tool process %d (own process group) survived the cancel", tool)
			}
		})
	}
}

func fileHasPID(p string) bool {
	b, _ := os.ReadFile(p)
	return strings.HasSuffix(string(b), "\n")
}

func TestTreeGroups(t *testing.T) {
	ps := "  1     0     1\n" + // launchd
		" 50     1    50\n" + // the host
		"100    50   100\n" + // claude (its own group)
		"101   100   100\n" + // an MCP server in claude's group
		"200   100   200\n" + // a Bash-tool shell, own group
		"201   200   200\n" + // the tool command
		"300   201    50\n" + // a descendant that joined the host's group: never signalled
		"400     1   400\n" // unrelated
	got := treeGroups(ps, 100, 50)
	if fmt.Sprint(got) != "[100 200]" {
		t.Errorf("groups %v", got)
	}
}

func TestStreamRetriesVanishedSession(t *testing.T) {
	bin := writeScript(t, `cat >/dev/null
case "$*" in *--resume*) echo "No conversation found with session ID: gone" >&2; exit 1;; esac
echo '{"type":"result","subtype":"success","is_error":false,"result":"fresh","session_id":"s-3"}'
`)
	res, err := Stream(context.Background(), StreamOptions{Bin: bin, Dir: t.TempDir()}, "gone", "x")
	if err != nil || res.Text != "fresh" || res.SessionID != "s-3" {
		t.Errorf("res=%+v err=%v", res, err)
	}
}

// gone waits up to d for pid to disappear: a signalled process takes a moment
// to exit, and on macOS an exited one stays visible until its new parent
// (launchd) reaps it, which lags under load.
func gone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for alive(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}
