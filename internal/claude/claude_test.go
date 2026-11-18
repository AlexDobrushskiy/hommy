package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude writes an executable script and returns Options pointing at it.
func fakeClaude(t *testing.T, script string) Options {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return Options{Bin: bin, Dir: dir, Timeout: 5 * time.Second, AppendSystemPrompt: SystemPrompt(false, 0, nil)}
}

func TestRunClaudeSuccess(t *testing.T) {
	cfg := fakeClaude(t, `echo '{"type":"result","result":"hello","session_id":"s1","is_error":false}'`)
	out, sid, err := Run(context.Background(), cfg, "", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello" || sid != "s1" {
		t.Errorf("got out=%q sid=%q", out, sid)
	}
}

func TestRunClaudePassesResumeFlag(t *testing.T) {
	// echo args into result so we can assert --resume was passed
	cfg := fakeClaude(t, `echo "{\"result\":\"args:$*\",\"session_id\":\"s2\"}"`)
	out, _, err := Run(context.Background(), cfg, "old-id", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if want := "--resume old-id"; !containsStr(out, want) {
		t.Errorf("args missing %q: %s", want, out)
	}
}

func TestRunClaudePassesAppendSystemPromptFlag(t *testing.T) {
	// echo args into result so we can assert --append-system-prompt was passed.
	// Don't assert the full prompt text in args — it contains spaces and gets
	// split across multiple $* fields by the shell.
	cfg := fakeClaude(t, `echo "{\"result\":\"args:$*\",\"session_id\":\"s5\"}"`)
	out, _, err := Run(context.Background(), cfg, "", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if want := "--append-system-prompt"; !containsStr(out, want) {
		t.Errorf("args missing %q: %s", want, out)
	}
}

func TestRunClaudeRetriesOnGoneSession(t *testing.T) {
	// Fail when --resume present, succeed without it.
	cfg := fakeClaude(t, `case "$*" in *--resume*) echo "No conversation found" >&2; exit 1;; *) echo '{"result":"fresh","session_id":"s3"}';; esac`)
	out, sid, err := Run(context.Background(), cfg, "gone-id", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if out != "fresh" || sid != "s3" {
		t.Errorf("got out=%q sid=%q", out, sid)
	}
}

func TestRunClaudeErrorSurfacesStderr(t *testing.T) {
	cfg := fakeClaude(t, `echo "boom detail" >&2; exit 2`)
	_, _, err := Run(context.Background(), cfg, "", "hi")
	if err == nil || !containsStr(err.Error(), "boom detail") {
		t.Errorf("expected stderr in error, got %v", err)
	}
}

func TestRunClaudeTimeout(t *testing.T) {
	cfg := fakeClaude(t, `sleep 5`)
	cfg.Timeout = 200 * time.Millisecond
	_, _, err := Run(context.Background(), cfg, "", "hi")
	if err == nil || !containsStr(err.Error(), "timed out") {
		t.Errorf("expected timeout error, got %v", err)
	}
}

func TestRunClaudeIsErrorReturnsSessionID(t *testing.T) {
	cfg := fakeClaude(t, `echo '{"result":"something broke","session_id":"s4","is_error":true}'`)
	reply, sid, err := Run(context.Background(), cfg, "", "hi")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !containsStr(err.Error(), "something broke") {
		t.Errorf("expected 'something broke' in error, got %v", err)
	}
	if sid != "s4" {
		t.Errorf("expected session id 's4', got %q", sid)
	}
	if reply != "" {
		t.Errorf("expected empty reply on error, got %q", reply)
	}
}

func containsStr(s, sub string) bool { return len(s) >= len(sub) && strings.Contains(s, sub) }

func TestRunUsesDir(t *testing.T) {
	cfg := fakeClaude(t, `printf '{"result":"%s","session_id":"d"}' "$(pwd -P)"`)
	want, _ := filepath.EvalSymlinks(cfg.Dir)
	out, _, err := Run(context.Background(), cfg, "", "hi")
	if err != nil || out != want {
		t.Fatalf("out=%q want %q err=%v", out, want, err)
	}
}

func TestSystemPrompt(t *testing.T) {
	if p := SystemPrompt(false, 400, []string{"ru"}); strings.Contains(p, "VOICE NOTE") {
		t.Error("tts disabled must omit the voice instruction")
	}
	p := SystemPrompt(true, 400, []string{"ru", "ru-coach"})
	if !strings.Contains(p, "longer than 400 characters") || !strings.Contains(p, "en (default), ru, ru-coach") {
		t.Errorf("prompt: %s", p)
	}
	if strings.Contains(SystemPrompt(true, 400, nil), "lang=") {
		t.Error("no extra langs: no lang clause")
	}
}
