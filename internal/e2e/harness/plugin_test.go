package harness

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const pluginDir = "../../../plugin"

// runShim runs a plugin/bin script with exactly env and stdin; returns combined output and exit code.
func runShim(t *testing.T, name, stdin string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(filepath.Join(pluginDir, "bin", name), args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// fakeBin writes an executable sh script NAME into a new dir and returns the dir.
func fakeBin(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// recorder is a fake hommy that writes its argv and stdin to OUT.
func recorder(t *testing.T, dir, out string) {
	t.Helper()
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >\"" + out + "\"\ncat >>\"" + out + "\"\necho noise; echo noise >&2; exit 7\n"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hommy"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestHookShimNeverFails(t *testing.T) {
	payload := `{"session_id":"s1","hook_event_name":"Stop"}`
	// no binary anywhere
	if out, code := runShim(t, "hommy-hook", payload, []string{"HOME=/nonexistent", "PATH=/usr/bin:/bin"}, "stop"); out != "" || code != 0 {
		t.Fatalf("missing binary: %q exit %d", out, code)
	}
	// unknown event and no event
	for _, args := range [][]string{{"bogus"}, {}} {
		if out, code := runShim(t, "hommy-hook", payload, []string{"HOME=/nonexistent", "PATH=/usr/bin:/bin"}, args...); out != "" || code != 0 {
			t.Fatalf("%v: %q exit %d", args, out, code)
		}
	}
	// a binary that fails noisily: still silent, exit 0; argv and stdin passed through
	home := t.TempDir()
	rec := filepath.Join(t.TempDir(), "rec")
	recorder(t, filepath.Join(home, ".local/share/hommy/bin"), rec)
	if out, code := runShim(t, "hommy-hook", payload, []string{"HOME=" + home, "PATH=/usr/bin:/bin"}, "stop"); out != "" || code != 0 {
		t.Fatalf("failing binary: %q exit %d", out, code)
	}
	b, _ := os.ReadFile(rec)
	if string(b) != "hook stop\n"+payload {
		t.Fatalf("default instance got %q", b)
	}
}

func TestHookShimInstance(t *testing.T) {
	payload := `{"hook_event_name":"UserPromptSubmit"}`
	home := t.TempDir()
	rec := filepath.Join(t.TempDir(), "rec")
	recorder(t, filepath.Join(home, ".local/share/hommy-test-s2/bin"), rec)
	cases := []struct {
		env  []string
		want string
	}{
		{[]string{"CLAUDE_PLUGIN_OPTION_INSTANCE=test-s2", "HOMMY_INSTANCE=other"}, "--instance test-s2 hook user-prompt-submit\n" + payload},
		{[]string{"HOMMY_INSTANCE=test-s2"}, "--instance test-s2 hook user-prompt-submit\n" + payload},
		// a bad instance name: nothing runs
		{[]string{"CLAUDE_PLUGIN_OPTION_INSTANCE=../x"}, ""},
		{[]string{"CLAUDE_PLUGIN_OPTION_INSTANCE=" + strings.Repeat("a", 25)}, ""},
	}
	for _, c := range cases {
		_ = os.Remove(rec)
		env := append([]string{"HOME=" + home, "PATH=/usr/bin:/bin"}, c.env...)
		if out, code := runShim(t, "hommy-hook", payload, env, "user-prompt-submit"); out != "" || code != 0 {
			t.Fatalf("%v: %q exit %d", c.env, out, code)
		}
		b, _ := os.ReadFile(rec)
		if string(b) != c.want {
			t.Errorf("%v: binary got %q, want %q", c.env, b, c.want)
		}
	}
	// an instance whose share dir has no binary: hommy on PATH, with the instance flag
	path := fakeBin(t, "hommy", "printf '%s' \"$*\" >\""+rec+"\"")
	_ = os.Remove(rec)
	runShim(t, "hommy-hook", "{}", []string{"HOME=" + t.TempDir(), "PATH=" + path + ":/usr/bin:/bin", "HOMMY_INSTANCE=x1"}, "notify")
	if b, _ := os.ReadFile(rec); string(b) != "--instance x1 hook notify" {
		t.Errorf("PATH fallback got %q", b)
	}
}

func TestHookShimGivesUpOnAHungHost(t *testing.T) {
	path := fakeBin(t, "hommy", "exec sleep 60")
	start := time.Now()
	out, code := runShim(t, "hommy-hook", "{}", []string{"HOME=/nonexistent", "PATH=" + path + ":/usr/bin:/bin"}, "stop")
	if out != "" || code != 0 {
		t.Fatalf("hung host: %q exit %d", out, code)
	}
	// the watchdog kills it at 4.2 s; a shim waiting for the binary would take 60 s
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("shim waited %v for a hung host", d)
	}
}

func TestOptionalToolShimsAreNoOpsWithoutTheirTool(t *testing.T) {
	env := []string{"HOME=/nonexistent", "PATH=/usr/bin:/bin"}
	bash := `{"tool_name":"Bash","tool_input":{"command":"telegram-agent-cli send text --chat 1 --message hi"}}`
	if out, code := runShim(t, "telegram-guard", bash, env); out != "" || code != 0 {
		t.Errorf("telegram-guard without its CLI: %q exit %d", out, code)
	}
	if out, code := runShim(t, "agterm-status", "{}", env, "active", "--blink"); out != "" || code != 0 {
		t.Errorf("agterm-status outside agterm: %q exit %d", out, code)
	}
	// with the CLI on PATH, the guard is exactly its hook command
	path := fakeBin(t, "telegram-agent-cli", `echo "args:$*"; exit 0`)
	if out, _ := runShim(t, "telegram-guard", bash, []string{"PATH=" + path + ":/usr/bin:/bin"}); out != "args:hook claude-pretooluse\n" {
		t.Errorf("telegram-guard with its CLI ran %q", out)
	}
}

func guard(t *testing.T, path, command string) string {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": command}})
	out, code := runShim(t, "wacli-guard", string(in), []string{"HOME=" + t.TempDir(), "PATH=" + path})
	if code != 0 {
		t.Fatalf("%q: exit %d: %s", command, code, out)
	}
	if out == "" {
		return "pass"
	}
	var d struct {
		HookSpecificOutput struct{ PermissionDecision string } `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("%q: %q", command, out)
	}
	return d.HookSpecificOutput.PermissionDecision
}

func TestWacliGuard(t *testing.T) {
	send := "wacli send text --to +10000000000 --message hi"
	// no wacli on PATH: everything passes
	if got := guard(t, "/usr/bin:/bin", send); got != "pass" {
		t.Fatalf("without wacli: %s", got)
	}
	path := fakeBin(t, "wacli", "exit 0") + ":/opt/homebrew/bin:/usr/bin:/bin"
	cases := map[string]string{
		"wacli --json chats list --limit 30":                          "pass",
		"wacli --json messages search invoice | head":                 "pass",
		"wacli send text --help":                                      "pass",
		"echo wacli send":                                             "pass",
		"git commit -m 'wacli send needs OK'":                         "pass",
		send:                                                          "ask",
		"WACLI_READONLY=0 wacli chats list":                           "ask",
		"wacli chats mark-read --chat x":                              "ask",
		"cd /tmp && wacli --json send react --to x":                   "ask",
		"sh -c 'wacli send text --to x --message y'":                  "ask",
		"for j in a b; do wacli send text --to $j --message hi; done": "ask",
		// the program path comes from a command substitution
		`"$(command -v wacli)" send text --to x --message y`: "ask",
		"$(which wacli) send text --to x --message y":        "ask",
		"`which wacli` send text --to x --message y":         "ask",
		"w=$(which wacli); $w send text --to x --message y":  "ask",
		`w=$(command -v wacli) && "$w" send text --to x`:     "ask",
		`sh -c '"$(command -v wacli)" send text --to x'`:     "ask",
		`"$(command -v wacli)" --json chats list`:            "pass",
		"w=$(command -v wacli); $w --json messages search x": "pass",
		"n=$(wacli --json chats list | wc -l); echo $n":      "pass",
	}
	for cmd, want := range cases {
		if got := guard(t, path, cmd); got != want {
			t.Errorf("%q: %s, want %s", cmd, got, want)
		}
	}
}

// session-start: WACLI_READONLY=1 into the session's env file only when wacli is installed and the user left it unset.
func TestWacliGuardSessionStart(t *testing.T) {
	withWacli := fakeBin(t, "wacli", "exit 0") + ":/usr/bin:/bin"
	for _, c := range []struct {
		name, path, extra, want string
	}{
		{"wacli installed", withWacli, "", "export WACLI_READONLY=1\n"},
		{"no wacli", "/usr/bin:/bin", "", ""},
		{"user set it", withWacli, "WACLI_READONLY=0", ""},
	} {
		envFile := filepath.Join(t.TempDir(), "env")
		env := []string{"HOME=" + t.TempDir(), "PATH=" + c.path, "CLAUDE_ENV_FILE=" + envFile}
		if c.extra != "" {
			env = append(env, c.extra)
		}
		if out, code := runShim(t, "wacli-guard", "{}", env, "session-start"); out != "" || code != 0 {
			t.Errorf("%s: %q exit %d", c.name, out, code)
		}
		got, _ := os.ReadFile(envFile)
		if string(got) != c.want {
			t.Errorf("%s: env file %q, want %q", c.name, got, c.want)
		}
	}
	// no CLAUDE_ENV_FILE: nothing to write, still exit 0
	if out, code := runShim(t, "wacli-guard", "{}", []string{"PATH=" + withWacli}, "session-start"); out != "" || code != 0 {
		t.Errorf("without CLAUDE_ENV_FILE: %q exit %d", out, code)
	}
}

// hooks.json: the events of master spec §5, every command a plugin/bin executable.
func TestHooksJSON(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(pluginDir, "hooks/hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var h struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type, Command string
				Timeout       int
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	var events []string
	cmds := map[string]bool{}
	re := regexp.MustCompile(`^"\$\{CLAUDE_PLUGIN_ROOT\}/bin/([a-z-]+)"( .*)?$`)
	for ev, groups := range h.Hooks {
		events = append(events, ev)
		for _, g := range groups {
			for _, c := range g.Hooks {
				m := re.FindStringSubmatch(c.Command)
				if m == nil || c.Type != "command" || c.Timeout <= 0 {
					t.Errorf("%s: %+v", ev, c)
					continue
				}
				st, err := os.Stat(filepath.Join(pluginDir, "bin", m[1]))
				if err != nil || st.Mode()&0o111 == 0 {
					t.Errorf("%s: %s is not an executable in plugin/bin", ev, m[1])
				}
				cmds[ev+" "+strings.TrimSpace(m[1]+m[2])] = true
			}
		}
	}
	sort.Strings(events)
	if strings.Join(events, ",") != "Notification,PermissionRequest,PostToolUse,PreToolUse,SessionEnd,SessionStart,Stop,UserPromptSubmit" {
		t.Errorf("events %v", events)
	}
	for _, want := range []string{"SessionStart hommy-hook session-start", "UserPromptSubmit hommy-hook user-prompt-submit",
		"Stop hommy-hook stop", "Notification hommy-hook notify", "PreToolUse wacli-guard", "PreToolUse telegram-guard",
		"SessionStart wacli-guard session-start",
		"UserPromptSubmit agterm-status active --blink", "PostToolUse agterm-status active --blink",
		"Notification agterm-status blocked --blink", "PermissionRequest agterm-status blocked --blink", "SessionEnd agterm-status idle"} {
		if !cmds[want] {
			t.Errorf("missing hook %q", want)
		}
	}
}
