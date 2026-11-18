// Package harness tests the harness scripts end to end (scripts/sx.sh).
package harness

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sx = "../../../scripts/sx.sh"

// runSx runs scripts/sx.sh with exactly env (plus a temp scratch base) and returns its environment as a map.
func runSx(t *testing.T, env []string, args ...string) (map[string]string, string, int) {
	t.Helper()
	base := t.TempDir()
	cmd := exec.Command(sx, append(args, "/usr/bin/env")...)
	cmd.Env = append(env, "HOMMY_SCRATCH_BASE="+base)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	return got, base, code
}

func TestSxScrubsTheEnvironment(t *testing.T) {
	planted := []string{
		"PATH=/usr/bin:/bin", "HOME=/nonexistent/real-home", "USER=tester", "LANG=en_US.UTF-8", "LC_ALL=C",
		"CLAUDE_CONFIG_DIR=/nonexistent/claude-config", "CLAUDE_CODE_X=1", "CLAUDE_CODE_OAUTH_TOKEN=planted",
		"ANTHROPIC_X=1", "ANTHROPIC_API_KEY=planted", "CLAUDECODE=1", "AGTERM_SESSION_ID=planted-session",
		"SOME_WRAPPER_VAR=1", "HOMMY_FOO=bar", "HOMMY_CLAUDE_BIN=/usr/bin/true",
	}
	got, base, code := runSx(t, planted)
	if code != 0 {
		t.Fatalf("sx exit %d: %v", code, got)
	}
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_X", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_X",
		"ANTHROPIC_API_KEY", "CLAUDECODE", "AGTERM_SESSION_ID", "SOME_WRAPPER_VAR"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s passed through sx", k)
		}
	}
	root := filepath.Join(base, "hm-s0")
	want := map[string]string{
		"HOME": filepath.Join(root, "home"), "PATH": filepath.Join(root, "bin") + ":/opt/homebrew/bin:/usr/bin:/bin",
		"HOMEBREW_NO_AUTO_UPDATE": "1", "HOMMY_FOO": "bar", "USER": "tester", "LANG": "en_US.UTF-8", "LC_ALL": "C",
		"HOMMY_SLOT": "0",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want)+2 { // + HOMMY_CLAUDE_BIN, HOMMY_SCRATCH_BASE
		t.Errorf("unexpected variables: %v", got)
	}
}

func TestSxSlotAndAgtermSession(t *testing.T) {
	env := []string{"PATH=/usr/bin:/bin", "HOMMY_CLAUDE_BIN=/usr/bin/true"}
	got, base, code := runSx(t, env, "--slot", "3", "--agterm-session", "ABC-123")
	if code != 0 || got["AGTERM_SESSION_ID"] != "ABC-123" || got["HOME"] != filepath.Join(base, "hm-s3", "home") || got["HOMMY_SLOT"] != "3" {
		t.Fatalf("exit %d: %v", code, got)
	}
}

func TestSxRefusesWithoutTheRealClaude(t *testing.T) {
	for _, env := range [][]string{
		{"PATH=/usr/bin:/bin"},
		{"PATH=/usr/bin:/bin", "HOMMY_CLAUDE_BIN=relative/claude"},
	} {
		if _, _, code := runSx(t, env); code != 3 {
			t.Fatalf("%v: exit %d, want 3", env, code)
		}
	}
}
