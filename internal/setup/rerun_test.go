package setup

import (
	"bufio"
	"io"
	"strings"
	"testing"

	"github.com/AlexDobrushskiy/hommy/internal/config"
)

func testRunner(t *testing.T, hostName string, o Options) *runner {
	t.Helper()
	home := t.TempDir()
	inst, err := config.NewInstance("test-unit", home)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(inst, inst.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	c.WithEnv(func(string) (string, bool) { return "", false })
	c.HostNameFn = func() string { return hostName }
	o.NonInteractive = true
	return &runner{Env: Env{Cfg: c, Out: io.Discard}, o: o, in: bufio.NewReader(strings.NewReader(""))}
}

func reload(t *testing.T, r *runner, hostName string) *config.Config {
	t.Helper()
	c, err := config.Load(r.Cfg.Inst, r.Cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	c.WithEnv(func(string) (string, bool) { return "", false })
	c.HostNameFn = func() string { return hostName }
	return c
}

// The host key is stored under the setup-time name: a renamed Mac keeps it.
func TestConfigStepKeepsHostName(t *testing.T) {
	r := testRunner(t, "studio", Options{})
	if err := r.configStep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := reload(t, r, "renamed").Get("HOST_NAME"); got != "studio" {
		t.Fatalf("HOST_NAME after a rename = %q, want the setup-time studio", got)
	}
}

// Switching speech modes on a re-run drops the values the old mode left.
func TestSpeechModeSwitch(t *testing.T) {
	r := testRunner(t, "studio", Options{Speech: "off"})
	if err := r.speech(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.Cfg = reload(t, r, "studio")
	r.o.Speech = "local"
	if err := r.speech(t.Context()); err != nil {
		t.Fatal(err)
	}
	c := reload(t, r, "studio")
	for _, k := range []string{"TTS_URL", "WHISPER_ENDPOINTS"} {
		if v := c.Get(k); strings.EqualFold(v, "off") || !strings.Contains(v, "127.0.0.1") {
			t.Errorf("local after off: %s=%q", k, v)
		}
	}
	if c.Get("TTS_LANG_BACKENDS") == "" {
		t.Error("local after off: no TTS_LANG_BACKENDS")
	}

	// external over off needs both URLs again
	r.Cfg = reload(t, r, "studio")
	r.o.Speech = "off"
	r.speech(t.Context())
	r.Cfg = reload(t, r, "studio")
	r.o.Speech = "external"
	if err := r.speech(t.Context()); err == nil {
		t.Fatal("external over off without --stt-url/--tts-url accepted")
	}
	r.Cfg = reload(t, r, "studio")
	r.o.STTURL, r.o.TTSURL = "http://127.0.0.1:9000/v1", "http://127.0.0.1:9001/v1"
	if err := r.speech(t.Context()); err != nil {
		t.Fatal(err)
	}
	c = reload(t, r, "studio")
	if c.Get("TTS_URL") != r.o.TTSURL || !strings.Contains(c.Get("WHISPER_ENDPOINTS"), "9000") {
		t.Fatalf("external: %s / %s", c.Get("TTS_URL"), c.Get("WHISPER_ENDPOINTS"))
	}

	// local over external drops the external URLs
	r.Cfg = c
	r.o = Options{NonInteractive: true, Speech: "local"}
	if err := r.speech(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v := reload(t, r, "studio").Get("TTS_URL"); strings.Contains(v, "9001") {
		t.Fatalf("local over external kept TTS_URL=%s", v)
	}
}
