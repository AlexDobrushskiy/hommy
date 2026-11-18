package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/pairing"
)

type fakeWorld struct {
	cmds      map[string]string // "name args" → output; missing = error
	http      map[string]int    // url → status; missing = error
	body      map[string]string
	dial      bool
	secrets   map[string]string
	files     map[string]os.FileMode
	read      map[string]string
	svc       map[string][2]bool
	installed map[string]bool
}

type fi struct{ mode os.FileMode }

func (f fi) Name() string       { return "f" }
func (f fi) Size() int64        { return 1 }
func (f fi) Mode() os.FileMode  { return f.mode }
func (f fi) ModTime() time.Time { return time.Time{} }
func (f fi) IsDir() bool        { return false }
func (f fi) Sys() any           { return nil }

func (w *fakeWorld) probes() Probes {
	return Probes{
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			k := strings.TrimSpace(name + " " + strings.Join(args, " "))
			for pat, out := range w.cmds {
				if strings.HasSuffix(k, pat) {
					return out, nil
				}
			}
			return "", errors.New("exit 1")
		},
		HTTPGet: func(_ context.Context, url string, _ map[string]string) (int, []byte, error) {
			for pat, code := range w.http {
				if strings.Contains(url, pat) {
					return code, []byte(w.body[pat]), nil
				}
			}
			return 0, nil, errors.New("refused")
		},
		Dial: func(string, string) error {
			if w.dial {
				return nil
			}
			return errors.New("refused")
		},
		Secret: func(it string) (string, error) {
			if v, ok := w.secrets[it]; ok {
				return v, nil
			}
			return "", errors.New("not found")
		},
		SecretHas: func(it string) bool { _, ok := w.secrets[it]; return ok },
		Stat: func(p string) (os.FileInfo, error) {
			if m, ok := w.files[p]; ok {
				return fi{m}, nil
			}
			return nil, fs.ErrNotExist
		},
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := w.read[p]; ok {
				return []byte(v), nil
			}
			return nil, fs.ErrNotExist
		},
		Service:   func(n string) (bool, bool) { s := w.svc[n]; return s[0], s[1] },
		Installed: func(n string) bool { return w.installed[n] },
		Pairing: func(context.Context) (pairing.State, error) {
			var st pairing.State
			if w.http["/api/pairing"] != 200 {
				return st, errors.New("down")
			}
			return st, json.Unmarshal([]byte(w.body["/api/pairing"]), &st)
		},
		APIStatus: func(context.Context) (pairing.Status, error) {
			var st pairing.Status
			if w.http["/api/status"] != 200 {
				return st, errors.New("down")
			}
			return st, json.Unmarshal([]byte(w.body["/api/status"]), &st)
		},
		GOOS: "darwin", GOARCH: "arm64", Version: "v1",
	}
}

func healthy(t *testing.T) (*config.Config, *fakeWorld) {
	t.Helper()
	inst, _ := config.NewInstance("unit", t.TempDir())
	c, _ := config.Load(inst, "")
	c.HostNameFn = func() string { return "mac1" }
	env := map[string]string{"PORT_BASE": "4710", "SPEECH": "external", "CLAUDE_BIN": "/x/claude",
		"WHISPER_ENDPOINTS": "fake|http://127.0.0.1:4712/v1|whisper", "TTS_URL": "http://127.0.0.1:4712/v1",
		"TELEGRAM_API_URL": "http://127.0.0.1:4711"}
	c.WithEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	w := &fakeWorld{
		cmds: map[string]string{"sw_vers -productVersion": "15.1", "version": "hommy v1", "/x/claude --version": "2.1.0 (Claude Code)",
			"/x/claude auth status": "{}", "-r- " + inst.BinPath(): `designated => identifier "dev.hommy" and certificate leaf = H"ab"`},
		http: map[string]int{"/getMe": 200, "4710/healthz": 200, "/api/pairing": 200, "/api/status": 200, "4712/health": 200},
		body: map[string]string{"/getMe": `{"ok":true,"result":{"username":"hommy_test_bot"}}`, "/api/pairing": `{"paired":true}`,
			"/api/status": `{"paired":true,"hosts":[{"name":"mac1","online":true,"last_seen":"` + now + `","checks":{"vault":"ok","agterm":"ok"}}]}`},
		dial:      true,
		secrets:   map[string]string{"host-key": strings.Repeat("k", 48), "telegram-bot-token": "1:x", "hub-host-keys": "mac1 k"},
		files:     map[string]os.FileMode{c.Path: 0o600, inst.BinPath(): 0o755},
		read:      map[string]string{inst.Home + "/.claude.json": `{"hasCompletedOnboarding":true}`},
		svc:       map[string][2]bool{"hub": {true, true}, "host": {true, true}},
		installed: map[string]bool{"hub": true, "host": true},
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	os.Unsetenv("CLAUDE_CONFIG_DIR")
	return c, w
}

func run(c *config.Config, w *fakeWorld) (map[string]Result, string, bool) {
	d := Doctor{Cfg: c, P: w.probes()}
	rs := d.Run(context.Background())
	var b bytes.Buffer
	red := Print(&b, rs)
	m := map[string]Result{}
	for _, r := range rs {
		m[r.ID] = r
	}
	return m, b.String(), red
}

func TestDoctorAllGreen(t *testing.T) {
	c, w := healthy(t)
	m, out, red := run(c, w)
	if red {
		t.Fatalf("red:\n%s", out)
	}
	for _, id := range []string{"platform", "claude", "claude-auth", "config", "secrets", "telegram", "binary", "services",
		"hub", "paired", "host", "vault", "hooks", "speech"} {
		if m[id].Mark != OK {
			t.Errorf("%s: %+v", id, m[id])
		}
	}
	if !strings.Contains(out, "✓ telegram @hommy_test_bot") {
		t.Errorf("%s", out)
	}
}

func TestDoctorTable(t *testing.T) {
	for _, tc := range []struct {
		id     string
		break_ func(c *config.Config, w *fakeWorld)
		fix    string
	}{
		{"speech", func(_ *config.Config, w *fakeWorld) { delete(w.http, "4712/health") }, "check WHISPER_ENDPOINTS / TTS_URL"},
		{"telegram", func(_ *config.Config, w *fakeWorld) { w.http["/getMe"] = 401 }, "@BotFather"},
		{"config", func(c *config.Config, w *fakeWorld) { w.files[c.Path] = 0o644 }, "chmod 600"},
		{"secrets", func(_ *config.Config, w *fakeWorld) { delete(w.secrets, "hub-host-keys") }, "hommy setup"},
		{"binary", func(c *config.Config, w *fakeWorld) {
			w.cmds["-r- "+c.Inst.BinPath()] = `designated => cdhash H"ab"`
		}, "hommy service install"},
		{"services", func(_ *config.Config, w *fakeWorld) { w.svc["host"] = [2]bool{true, false} }, "hommy service restart host"},
		{"hub", func(_ *config.Config, w *fakeWorld) { w.http["4710/healthz"] = 503 }, "hommy service restart hub"},
		{"paired", func(_ *config.Config, w *fakeWorld) { w.body["/api/pairing"] = `{"paired":false}` }, "hommy pair"},
		{"vault", func(_ *config.Config, w *fakeWorld) {
			w.body["/api/status"] = strings.Replace(w.body["/api/status"], `"vault":"ok"`, `"vault":"denied"`, 1)
		}, "Full Disk Access"},
		{"hooks", func(_ *config.Config, w *fakeWorld) { w.dial = false }, "hommy service restart host"},
		{"claude-auth", func(c *config.Config, w *fakeWorld) { delete(w.read, c.Inst.Home+"/.claude.json") }, "run claude once"},
		{"platform", func(_ *config.Config, w *fakeWorld) { w.cmds["sw_vers -productVersion"] = "13.6" }, "macOS 14+"},
	} {
		c, w := healthy(t)
		tc.break_(c, w)
		m, out, red := run(c, w)
		if !red || m[tc.id].Mark != Fail || !strings.Contains(m[tc.id].Fix, tc.fix) || !strings.Contains(out, "  fix: ") {
			t.Errorf("%s: %+v\n%s", tc.id, m[tc.id], out)
		}
	}
}

func TestHealthURL(t *testing.T) {
	for in, want := range map[string]string{"http://127.0.0.1:8474/upstream/kokoro/v1": "http://127.0.0.1:8474/health",
		"http://127.0.0.1:4712/v1": "http://127.0.0.1:4712/health", "http://h:1/v1/": "http://h:1/health"} {
		if got := healthURL(in); got != want {
			t.Errorf("%s → %s", in, got)
		}
	}
}
