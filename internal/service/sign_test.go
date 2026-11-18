package service

import (
	"bytes"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/secret"
)

// realSearchList reads the real user's keychain search list and default (read-only).
func realSearchList(t *testing.T) string {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, args := range [][]string{{"list-keychains", "-d", "user"}, {"default-keychain", "-d", "user"}} {
		cmd := exec.Command("/usr/bin/security", args...)
		cmd.Env = []string{"HOME=" + u.HomeDir, "PATH=/usr/bin:/bin"}
		b, _ := cmd.CombinedOutput()
		out = append(out, b...)
	}
	return string(out)
}

// scratchHome makes a temp HOME whose keychain search list and default are only
// a scratch keychain (unlocked, no auto-lock); it returns HOME, that keychain
// and a security runner bound to HOME.
func scratchHome(t *testing.T) (home, kc string, sec func(args ...string) string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home = t.TempDir()
	t.Setenv("HOME", home)
	env := []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	sec = func(args ...string) string {
		cmd := exec.Command("/usr/bin/security", args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("security %v: %v %s", args, err, out)
		}
		return string(out)
	}
	// the user search list lives in HOME/Library/Preferences; it must exist for -s to stick
	os.MkdirAll(filepath.Join(home, "Library", "Preferences"), 0o700)
	os.MkdirAll(filepath.Join(home, "Library", "Keychains"), 0o700)
	kc = filepath.Join(home, "Library", "Keychains", "scratch.keychain-db")
	sec("create-keychain", "-p", "pw", kc)
	t.Cleanup(func() {
		ks, _ := filepath.Glob(filepath.Join(home, "Library", "Keychains", "*.keychain-db"))
		for _, k := range ks {
			cmd := exec.Command("/usr/bin/security", "delete-keychain", k)
			cmd.Env = env
			cmd.Run()
		}
	})
	sec("unlock-keychain", "-p", "pw", kc)
	sec("set-keychain-settings", kc)
	sec("list-keychains", "-d", "user", "-s", kc)
	sec("default-keychain", "-d", "user", "-s", kc)
	if l := sec("list-keychains", "-d", "user"); !strings.Contains(l, kc) {
		t.Fatalf("scratch search list did not stick: %q", l)
	}
	return home, kc, sec
}

// TestSigningOnScratchKeychain signs a real binary with an identity created in
// a scratch HOME whose search list holds only a scratch keychain, then purges.
func TestSigningOnScratchKeychain(t *testing.T) {
	if _, err := os.Stat("/usr/bin/codesign"); err != nil {
		t.Skip("codesign missing")
	}
	realBefore := realSearchList(t)
	home, kc, sec := scratchHome(t)
	env := []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	inst, _ := config.NewInstance("test-unit", home)
	os.MkdirAll(inst.ConfigDir(), 0o700)
	os.WriteFile(inst.ConfigFile(), []byte("SPEECH=off\n"), 0o600)
	cfg, _ := config.Load(inst, inst.ConfigFile())
	cfg.WithEnv(func(string) (string, bool) { return "", false })
	exe, _ := os.Executable()
	f := &fakeLaunchd{loaded: map[string]int{}}
	var out bytes.Buffer
	m := &Manager{Cfg: cfg, Exe: exe, Launchctl: f.run, Out: &out, sleep: func(time.Duration) {}}

	st := secret.Store{Service: inst.KeychainService(), Keychain: kc, Env: env}
	if err := st.Set(secret.TelegramToken, "123:fake"); err != nil {
		t.Fatal(err)
	}
	if err := m.Install(Hub, Host); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	req, err := Requirement(inst.BinPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(req, `identifier "dev.hommy" and certificate leaf = H"`) || strings.Contains(req, "cdhash") {
		t.Fatalf("requirement %q", req)
	}
	if l := sec("list-keychains", "-d", "user"); strings.Contains(l, "signing") || !strings.Contains(l, kc) {
		t.Fatalf("search list not restored: %q", l)
	}
	if !st.Exists(secret.SigningPassword) {
		t.Fatal("signing-keychain-password not in the instance service")
	}
	if !strings.Contains(sec("find-identity", "-p", "codesigning", inst.SigningKeychain()), `"hommy-test-unit local signing"`) {
		t.Fatal("identity CN")
	}
	// re-run: nothing re-signed
	out.Reset()
	if err := m.Install(Hub, Host); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "unchanged binary\n") {
		t.Fatalf("re-run: %q", out.String())
	}
	// a second signature with the existing identity keeps the same leaf
	os.Remove(m.stampPath())
	if err := m.Install(Hub); err != nil {
		t.Fatal(err)
	}
	if req2, _ := Requirement(inst.BinPath()); req2 != req {
		t.Fatalf("leaf changed: %q → %q", req, req2)
	}

	if err := m.Uninstall(true); err != nil {
		t.Fatalf("purge: %v\n%s", err, out.String())
	}
	if exists(inst.SigningKeychain()) || exists(inst.ShareDir()) || exists(inst.ConfigDir()) {
		t.Fatal("purge left files")
	}
	for _, item := range []string{secret.TelegramToken, secret.SigningPassword} {
		if st.Exists(item) {
			t.Fatalf("purge left %s", item)
		}
	}
	if l := sec("list-keychains", "-d", "user"); !strings.Contains(l, kc) {
		t.Fatalf("scratch keychain dropped from its list: %q", l)
	}
	if realSearchList(t) != realBefore {
		t.Fatal("the real user's keychain search list changed")
	}
}
