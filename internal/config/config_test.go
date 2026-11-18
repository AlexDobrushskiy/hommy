package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testCfg(t *testing.T, name string, env map[string]string) *Config {
	t.Helper()
	inst, err := NewInstance(name, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(inst, "")
	if err != nil {
		t.Fatal(err)
	}
	c.HostNameFn = func() string { return "mac1" }
	return c.WithEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
}

func TestInstancePaths(t *testing.T) {
	for _, c := range []struct {
		name, base, label, kc, signing string
	}{
		{"", "hommy", "dev.hommy.hub", "hommy", "hommy-signing.keychain-db"},
		{"test", "hommy-test", "dev.hommy.test.hub", "hommy-test", "hommy-test-signing.keychain-db"},
		{"test-s1", "hommy-test-s1", "dev.hommy.test-s1.hub", "hommy-test-s1", "hommy-test-s1-signing.keychain-db"},
	} {
		i, err := NewInstance(c.name, "/h")
		if err != nil {
			t.Fatal(err)
		}
		if i.ConfigFile() != "/h/.config/"+c.base+"/hommy.env" || i.StateDir() != "/h/.local/state/"+c.base ||
			i.ShareDir() != "/h/.local/share/"+c.base || i.BinPath() != "/h/.local/share/"+c.base+"/bin/hommy" ||
			i.Label("hub") != c.label || i.KeychainService() != c.kc ||
			i.SigningKeychain() != "/h/Library/Keychains/"+c.signing ||
			i.PlistPath("hub") != "/h/Library/LaunchAgents/"+c.label+".plist" {
			t.Errorf("instance %q: %+v", c.name, i)
		}
	}
	for _, bad := range []string{"Test", "a_b", strings.Repeat("a", 25), "../x"} {
		if _, err := NewInstance(bad, "/h"); err == nil {
			t.Errorf("instance %q accepted", bad)
		}
	}
}

// Every default URL and listen address is loopback: nothing of anyone's LAN.
func TestDefaultURLsAreLoopback(t *testing.T) {
	c := testCfg(t, "", nil)
	for _, k := range Keys {
		v := c.Get(k.Name)
		for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '|' }) {
			host := ""
			if u, err := url.Parse(part); err == nil && u.Host != "" {
				host = u.Hostname()
			} else if strings.Contains(k.Name, "LISTEN") && part != "" {
				host, _, _ = strings.Cut(part, ":")
			}
			if host != "" && host != "127.0.0.1" && host != "localhost" && !(k.Name == "TELEGRAM_API_URL" && host == "api.telegram.org") {
				t.Errorf("%s default %q is not loopback", k.Name, v)
			}
		}
	}
	if got := c.Get("HUB_LISTEN"); got != "127.0.0.1:8470" {
		t.Errorf("HUB_LISTEN %q", got)
	}
	if got := c.Get("TTS_URL"); got != "http://127.0.0.1:8474/upstream/kokoro/v1" {
		t.Errorf("TTS_URL %q", got)
	}
	if got := c.Get("DEFAULT_TARGET"); got != "new@mac1:"+filepath.Join(c.Inst.Home, "hommy-vault")+":interactive" {
		t.Errorf("DEFAULT_TARGET %q", got)
	}
	if c.Get("AGENT_PERMISSIONS") != "skip" || c.Get("TTS_DEFAULT_VOICE") != "af_heart" || c.Get("AGTERM_WORKSPACE") != "hommy" {
		t.Error("defaults")
	}
	if got := c.Get("TTS_LANG_BACKENDS"); !strings.Contains(got, "es|http://127.0.0.1:8474/upstream/kokoro/v1|mlx-community/Kokoro-82M-bf16|ef_dora") ||
		strings.Count(got, ",") != 4 {
		t.Errorf("TTS_LANG_BACKENDS %q", got)
	}
}

func TestPortBaseDerives(t *testing.T) {
	c := testCfg(t, "test", map[string]string{"PORT_BASE": "4710"})
	want := map[string]string{"HUB_LISTEN": "127.0.0.1:4710", "HUB_URL": "ws://127.0.0.1:4710/ws",
		"SPEECH_LISTEN": "127.0.0.1:4714", "WHISPER_ENDPOINTS": "local|http://127.0.0.1:4714/upstream/whisper/v1|whisper"}
	for k, v := range want {
		if got := c.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if got := c.Get("HUB_DB"); got != filepath.Join(c.Inst.Home, ".local/state/hommy-test/hub/hub.sqlite3") {
		t.Errorf("HUB_DB %q", got)
	}
}

func TestValidationTable(t *testing.T) {
	c := testCfg(t, "", nil)
	for _, tc := range []struct {
		key, val string
		ok       bool
	}{
		{"HUB_LISTEN", "127.0.0.1:4710", true}, {"HUB_LISTEN", "not-an-address", false}, {"HUB_LISTEN", "0.0.0.0:99999", false},
		{"HUB_URL", "ws://127.0.0.1:1/ws", true}, {"HUB_URL", "http://x/ws", false},
		{"PORT_BASE", "4710", true}, {"PORT_BASE", "80", false}, {"PORT_BASE", "x", false},
		{"QUEUED_NOTICE_AFTER", "5s", true}, {"QUEUED_NOTICE_AFTER", "5", false},
		{"AGENT_PERMISSIONS", "ask", true}, {"AGENT_PERMISSIONS", "never", false},
		{"SPEECH", "external", true}, {"SPEECH", "cloud", false},
		{"TTS_URL", "off", true}, {"TTS_URL", "ftp://x", false},
		{"MINIAPP_URL", "https://example.invalid", true}, {"MINIAPP_URL", "http://example.invalid", false},
		{"MINIAPP_LISTEN", "", true}, {"MINIAPP_LISTEN", "127.0.0.1:4713", true},
		{"DEFAULT_TARGET", "vault@mac1", true}, {"DEFAULT_TARGET", "vault@Mac 1", false}, {"DEFAULT_TARGET", "x", false},
		{"HOST_NAME", "mac-1", true}, {"HOST_NAME", "Mac_1", false},
		{"VAULT_DIR", "~/v", true}, {"VAULT_DIR", "rel/v", false},
		{"OWNER_CHAT_ID", "1001", true}, {"OWNER_CHAT_ID", "abc", false},
		{"WHISPER_MODEL", "small", true}, {"WHISPER_MODEL", "huge", false},
	} {
		err := c.Set(tc.key, tc.val)
		if (err == nil) != tc.ok {
			t.Errorf("set %s=%q: err %v, want ok=%v", tc.key, tc.val, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), tc.key) {
			t.Errorf("error does not name %s: %v", tc.key, err)
		}
	}
	if err := c.Set("FOO", "1"); err == nil {
		t.Error("unknown key accepted by Set")
	}
}

func TestFileRoundTripAndPrecedence(t *testing.T) {
	c := testCfg(t, "test", map[string]string{"HOST_SILENT_AFTER": "9m"})
	os.MkdirAll(filepath.Dir(c.Path), 0o700)
	os.WriteFile(c.Path, []byte("# comment\nHOST_SILENT_AFTER=1h\nFOO=1\nVAULT_DIR=\"~/v\"\n"), 0o644)
	c2, _ := Load(c.Inst, "")
	c2.HostNameFn = c.HostNameFn
	if c2.Get("HOST_SILENT_AFTER") != "1h" || c2.GetPath("VAULT_DIR") != filepath.Join(c.Inst.Home, "v") {
		t.Errorf("file values: %q %q", c2.Get("HOST_SILENT_AFTER"), c2.GetPath("VAULT_DIR"))
	}
	c2.WithEnv(func(k string) (string, bool) {
		return map[string]string{"HOST_SILENT_AFTER": "9m"}[k], k == "HOST_SILENT_AFTER"
	})
	if c2.Get("HOST_SILENT_AFTER") != "9m" {
		t.Error("env must win over the file")
	}
	if u := c2.Unknown(); len(u) != 1 || u[0] != "FOO" {
		t.Errorf("unknown %v", u)
	}
	if err := c2.Set("QUEUED_NOTICE_AFTER", "5s"); err != nil {
		t.Fatal(err)
	}
	if err := c2.Unset("FOO"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(c.Path)
	if string(b) != "# comment\nHOST_SILENT_AFTER=1h\nVAULT_DIR=\"~/v\"\nQUEUED_NOTICE_AFTER=5s\n" {
		t.Errorf("file:\n%s", b)
	}
	fi, _ := os.Stat(c.Path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}
}

func TestSanitizeHostName(t *testing.T) {
	for in, want := range map[string]string{"Mac-Mini": "mac-mini", "My Mac (2)": "my-mac-2", "": "mac", "--x--": "x"} {
		if got := SanitizeHostName(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
