package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runT(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := run(args, strings.NewReader(""), &out, &errb)
	return rc, out.String(), errb.String()
}

func scratch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HOMMY_INSTANCE", "")
	t.Setenv("HOMMY_CONFIG", "")
	os.Unsetenv("CLAUDE_CONFIG_DIR")
}

func TestVersion(t *testing.T) {
	rc, out, errs := runT(t, "version")
	if rc != 0 || !strings.HasPrefix(out, "hommy ") {
		t.Fatalf("rc=%d out=%q err=%q", rc, out, errs)
	}
}

func TestUsage(t *testing.T) {
	rc, _, errs := runT(t)
	if rc != 2 || !strings.Contains(errs, "usage: hommy") {
		t.Fatalf("rc=%d stderr=%q", rc, errs)
	}
	if rc, _, _ := runT(t, "nonsense"); rc != 2 {
		t.Fatalf("unknown command rc=%d", rc)
	}
}

func TestSplitGlobals(t *testing.T) {
	g, rest, err := splitGlobals([]string{"config", "--instance", "test", "get", "--config=/x", "HUB_URL", "--", "--instance"})
	if err != nil || g.instance != "test" || g.config != "/x" || strings.Join(rest, " ") != "config get HUB_URL -- --instance" {
		t.Fatalf("%+v %q %v", g, rest, err)
	}
	if _, _, err := splitGlobals([]string{"--instance"}); err == nil {
		t.Fatal("missing value accepted")
	}
}

func TestConfigCommands(t *testing.T) {
	scratch(t)
	rc, out, _ := runT(t, "--instance", "unit", "config", "path")
	want := filepath.Join(os.Getenv("HOME"), ".config/hommy-unit/hommy.env")
	if rc != 0 || strings.TrimSpace(out) != want {
		t.Fatalf("path: %d %q", rc, out)
	}
	if rc, out, errs := runT(t, "--instance", "unit", "config", "set", "HUB_LISTEN", "not-an-address"); rc != 2 ||
		!strings.Contains(errs, "HUB_LISTEN") || out != "" {
		t.Fatalf("invalid set: %d %q %q", rc, out, errs)
	}
	if rc, _, errs := runT(t, "--instance", "unit", "config", "set", "FOO", "1"); rc != 0 || !strings.Contains(errs, "unknown setting FOO") {
		t.Fatalf("unknown set: %d %q", rc, errs)
	}
	if rc, _, _ := runT(t, "--instance", "unit", "config", "permissions", "ask"); rc != 0 {
		t.Fatal("permissions ask")
	}
	if _, out, _ := runT(t, "--instance", "unit", "config", "get", "AGENT_PERMISSIONS"); strings.TrimSpace(out) != "ask" {
		t.Fatalf("AGENT_PERMISSIONS %q", out)
	}
	fi, err := os.Stat(want)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config file: %v %v", fi, err)
	}
	if rc, _, _ := runT(t, "--instance", "unit", "config", "permissions", "never"); rc != 2 {
		t.Fatal("permissions never accepted")
	}
	if rc, _, _ := runT(t, "--instance", "Bad_Name", "config", "path"); rc != 2 {
		t.Fatal("bad instance accepted")
	}
}

// A hook run never prints and exits 0, configured or not.
func TestHookIsSilent(t *testing.T) {
	scratch(t)
	for _, args := range [][]string{{"hook", "stop"}, {"hook"}, {"hook", "bogus"}, {"--instance", "unit", "hook", "notify"}} {
		rc, out, errs := runT(t, args...)
		if rc != 0 || out != "" || errs != "" {
			t.Errorf("%v: %d %q %q", args, rc, out, errs)
		}
	}
}
