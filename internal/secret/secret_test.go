package secret

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// scratchStore makes a throwaway keychain under a temp HOME, unlocked with a
// known password and no auto-lock, so no call can raise a dialog.
func scratchStore(t *testing.T) Store {
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
	return Store{Service: "hommy-unit", Keychain: kc, Env: env}
}

func TestRoundTrip(t *testing.T) {
	s := scratchStore(t)
	if _, err := s.Get(HostKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing item: %v", err)
	}
	if s.Exists(HostKey) {
		t.Fatal("Exists on a missing item")
	}
	for _, v := range []string{"123456:ABC-def_ghi", `quote " and \ backslash`, "a b,c d", "x"} {
		if err := s.Set(HostKey, v); err != nil {
			t.Fatalf("set %q: %v", v, err)
		}
		got, err := s.Get(HostKey)
		if err != nil || got != v {
			t.Fatalf("get: %q %v, want %q", got, err, v)
		}
	}
	if !s.Exists(HostKey) {
		t.Fatal("Exists after set")
	}
	if err := s.Delete(HostKey); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(HostKey); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if s.Exists(HostKey) {
		t.Fatal("still there after delete")
	}
}

func TestHostKeys(t *testing.T) {
	m := HostKeys("b " + "kb\n# c x\na ka,  c kc ")
	if len(m) != 3 || m["a"] != "ka" || m["b"] != "kb" || m["c"] != "kc" {
		t.Fatalf("%v", m)
	}
	if got := FormatHostKeys(m); got != "a ka,b kb,c kc" {
		t.Fatalf("%q", got)
	}
	if k, err := NewKey(); err != nil || len(k) != 48 {
		t.Fatalf("%q %v", k, err)
	}
}
