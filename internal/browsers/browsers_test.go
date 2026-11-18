package browsers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexDobrushskiy/hommy/internal/config"
)

func cfg(t *testing.T, name string, env map[string]string) *config.Config {
	t.Helper()
	inst, err := config.NewInstance(name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(inst, "")
	if err != nil {
		t.Fatal(err)
	}
	c.WithEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	return c
}

// Instance X: its own ports (PORT_BASE+6/+8), labels, bundle id, state dir, pool 0 (master §3.1).
func TestConfigEnvPerInstance(t *testing.T) {
	c := cfg(t, "test", map[string]string{"PORT_BASE": "4710"})
	got := string(ConfigEnv(c, "/opt/homebrew/opt/node/bin/node"))
	for _, want := range []string{"HB_PORT_BASE=4716\n", "HB_DEBUG_BASE=4718\n", "HB_POOL=0\n",
		"HB_LABEL_PREFIX=dev.hommy.test.browsers\n", "HB_BUNDLE_ID=dev.hommy.test.agent-chrome\n",
		"HB_STATE_DIR=" + c.Inst.Home + "/.local/state/hommy-test-browsers\n", "HB_NODE=/opt/homebrew/opt/node/bin/node\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if Home(c) != c.Inst.Home+"/.local/share/hommy-test-browsers" {
		t.Errorf("HB_HOME %s", Home(c))
	}
	if l, i := Ports(c); l != 4716 || i != 4717 {
		t.Errorf("ports %d %d", l, i)
	}
}

// The default instance keeps the engine's defaults.
func TestConfigEnvDefaultInstance(t *testing.T) {
	c := cfg(t, "", nil)
	got := string(ConfigEnv(c, ""))
	if strings.Contains(got, "HB_PORT_BASE") || strings.Contains(got, "HB_LABEL_PREFIX") || strings.Contains(got, "HB_NODE") {
		t.Errorf("default instance overrides engine defaults:\n%s", got)
	}
	if l, _ := Ports(c); l != DefaultPortBase {
		t.Errorf("lanes %d", l)
	}
}

func TestWriteConfigIdempotent(t *testing.T) {
	c := cfg(t, "test", map[string]string{"PORT_BASE": "4710"})
	os.MkdirAll(Home(c), 0o755)
	for i, want := range []bool{true, false} {
		if changed, err := WriteConfig(c, ""); err != nil || changed != want {
			t.Fatalf("write %d: %v %v", i, changed, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(Home(c), "config.env"))
	if LanesPort(c, ParseEnv(b)) != 4716 {
		t.Errorf("lanes port from file: %d", LanesPort(c, ParseEnv(b)))
	}
}
