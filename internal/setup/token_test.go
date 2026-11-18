package setup

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexDobrushskiy/hommy/internal/secret"
)

// scratchStore is a throwaway keychain under a temp HOME, unlocked, no auto-lock: no call can raise a dialog.
func scratchStore(t *testing.T) secret.Store {
	t.Helper()
	home := t.TempDir()
	kc := filepath.Join(home, "Library", "Keychains", "t.keychain-db")
	os.MkdirAll(filepath.Dir(kc), 0o700)
	env := []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	sec := func(args ...string) {
		cmd := exec.Command("/usr/bin/security", args...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("security %v: %v %s", args, err, out)
		}
	}
	sec("create-keychain", "-p", "pw", kc)
	t.Cleanup(func() {
		cmd := exec.Command("/usr/bin/security", "delete-keychain", kc)
		cmd.Env = env
		cmd.Run()
	})
	sec("unlock-keychain", "-p", "pw", kc)
	sec("set-keychain-settings", kc)
	return secret.Store{Service: "hommy-unit", Keychain: kc, Env: env}
}

// A re-run with --token-stdin and the token already stored changes nothing (I5): no Keychain write, no
// secretsChanged (which restarts hub and host); a different token is stored and counts as a change.
func TestTokenStdinSameTokenIsKept(t *testing.T) {
	const tok = "123456:synthetic-token-for-tests"
	r := testRunner(t, "studio", Options{TokenStdin: true})
	r.Store = scratchStore(t)
	r.Probes.HTTPGet = func(_ context.Context, url string, _ map[string]string) (int, []byte, error) {
		return 200, []byte(`{"ok":true,"result":{"username":"hommy_unit_bot"}}`), nil
	}
	if err := r.Store.Set(secret.TelegramToken, tok); err != nil {
		t.Fatal(err)
	}
	r.in = bufio.NewReader(strings.NewReader(tok + "\n"))
	if err := r.token(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.secretsChanged {
		t.Fatal("the same token from stdin counted as a change (hub and host would restart)")
	}
	r.in = bufio.NewReader(strings.NewReader("654321:another-synthetic-token\n"))
	if err := r.token(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Get(secret.TelegramToken); !r.secretsChanged || got != "654321:another-synthetic-token" {
		t.Fatalf("a new token: changed %v, stored %q", r.secretsChanged, got)
	}
}

// A run that changed a daemon input (here a new token) and ended before `services` restarted hub and host leaves a
// mark: the next run restarts them although it changes nothing itself (same token, same config), once.
func TestHalfFinishedRunRestartsOnRerun(t *testing.T) {
	a := testRunner(t, "studio", Options{})
	a.cfgBefore, _ = os.ReadFile(a.Cfg.Path)
	a.secretsChanged = true // the token step stored a new token; a later step then failed
	a.stepEnded()

	var restarts []bool
	install := func(restart bool, _ ...string) (int, error) { restarts = append(restarts, restart); return 0, nil }
	for range 2 {
		b := testRunner(t, "studio", Options{})
		b.Cfg = reload(t, a, "studio")
		b.cfgBefore, _ = os.ReadFile(b.Cfg.Path)
		b.Install = install
		if err := b.services(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(restarts) != 2 || !restarts[0] || restarts[1] {
		t.Fatalf("restart per re-run = %v, want [true false]", restarts)
	}
}
