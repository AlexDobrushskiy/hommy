package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTgRealRefusesBeforeAnyCall runs scripts/tg-real.sh with a stub named like the real CLI first on PATH: the stub
// records any call and fails. Every case must exit 2 without the stub being called.
func TestTgRealRefusesBeforeAnyCall(t *testing.T) {
	stubDir, home := t.TempDir(), t.TempDir()
	record := filepath.Join(stubDir, "called")
	stub := "#!/bin/sh\necho \"$@\" >>" + record + "\nexit 99\n"
	if err := os.WriteFile(filepath.Join(stubDir, "telegram-agent-cli"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(extra []string, args ...string) (int, string) {
		cmd := exec.Command("../../../scripts/tg-real.sh", args...)
		cmd.Env = append([]string{"PATH=" + stubDir + ":/usr/bin:/bin", "HOME=" + home}, extra...)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return code, string(out)
	}
	good := "HOMMY_TEST_BOT=@hommy_test_abc123_bot"
	cases := []struct {
		name  string
		extra []string
		args  []string
	}{
		{"no test bot configured", nil, []string{"send", "text", "hi"}},
		{"no test bot, read", nil, []string{"read", "messages"}},
		{"not a test bot", []string{"HOMMY_TEST_BOT=@some_dev_bot"}, []string{"send", "text", "hi"}},
		{"--from owner", []string{good}, []string{"send", "text", "hi", "--from", "owner"}},
		{"read --from other", []string{good}, []string{"read", "messages", "--from", "other"}},
		{"read --from=owner", []string{good}, []string{"read", "messages", "--from=owner"}},
		{"send --from bot", []string{good}, []string{"send", "text", "hi", "--from", "bot"}},
		{"--chat other", []string{good}, []string{"read", "messages", "--chat", "other"}},
		{"--chat=@x", []string{good}, []string{"wait", "--for", "reply", "--chat=@x"}},
		{"fake-only verb", []string{good}, []string{"token"}},
		{"fake-only fault", []string{good}, []string{"fault", "sendMessage"}},
		{"no verb", []string{good}, nil},
	}
	for _, c := range cases {
		if code, out := run(c.extra, c.args...); code != 2 || !strings.Contains(out, `"code":"usage"`) {
			t.Errorf("%s: exit %d, %s", c.name, code, out)
		}
	}
	// A config file with a non-test bot is refused too.
	cfg := filepath.Join(home, ".config", "hommy-test")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "test-bot"), []byte("@some_dev_bot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := run(nil, "send", "text", "hi"); code != 2 {
		t.Errorf("config with a non-test bot: exit %d, %s", code, out)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		b, _ := os.ReadFile(record)
		t.Fatalf("the CLI stub was called: %s", b)
	}
}

// TestTgRealAlwaysPassesTheTestBot: a valid call reaches the (stubbed) CLI with the test bot as --chat, never the CLI's
// default chat; `read --from bot` (the CLI's own filter) passes through.
func TestTgRealAlwaysPassesTheTestBot(t *testing.T) {
	stubDir := t.TempDir()
	record := filepath.Join(stubDir, "called")
	stub := "#!/bin/sh\necho \"$@\" >>" + record + "\necho '{\"ok\":true}'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "telegram-agent-cli"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		bot, want string
		args      []string
	}{
		{"@hommy_test_abc123_bot", "e2e send text hi --chat @hommy_test_abc123_bot", []string{"send", "text", "hi"}},
		{"hommy_test_abc123_bot", "e2e read messages --from bot --chat @hommy_test_abc123_bot",
			[]string{"read", "messages", "--from", "bot"}},
		{"@hommy_test_abc123_bot", "e2e send voice --path a.ogg --wait reply --chat @hommy_test_abc123_bot",
			[]string{"send", "voice", "a.ogg", "--wait", "reply"}},
	} {
		os.Remove(record)
		cmd := exec.Command("../../../scripts/tg-real.sh", c.args...)
		cmd.Env = []string{"PATH=" + stubDir + ":/usr/bin:/bin", "HOME=" + t.TempDir(), "HOMMY_TEST_BOT=" + c.bot}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", c.args, err, out)
		}
		got, _ := os.ReadFile(record)
		if strings.TrimSpace(string(got)) != c.want {
			t.Fatalf("CLI argv %q, want %q", got, c.want)
		}
	}
}
