package service

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/config"
)

// TestMain lets launchd run this test binary as a stand-in daemon: the signed
// copy is started as `<bin> hub|host --instance X` and just stays up.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == Hub || os.Args[1] == Host) {
		for {
			time.Sleep(time.Hour)
		}
	}
	os.Exit(m.Run())
}

// TestLaunchdSmoke drives real launchd with labels dev.hommy.test-unit.* in a
// scratch HOME (HOMMY_SERVICE_SMOKE=1 only).
func TestLaunchdSmoke(t *testing.T) {
	if os.Getenv("HOMMY_SERVICE_SMOKE") != "1" {
		t.Skip("HOMMY_SERVICE_SMOKE=1 runs it against the real launchd")
	}
	home, _, _ := scratchHome(t)
	inst, _ := config.NewInstance("test-unit", home)
	os.MkdirAll(inst.ConfigDir(), 0o700)
	os.WriteFile(inst.ConfigFile(), []byte("SPEECH=off\n"), 0o600)
	cfg, _ := config.Load(inst, inst.ConfigFile())
	cfg.WithEnv(func(string) (string, bool) { return "", false })
	exe, _ := os.Executable()
	var out bytes.Buffer
	m := &Manager{Cfg: cfg, Exe: exe, Out: &out}
	t.Cleanup(func() {
		for _, svc := range allNames {
			RealLaunchctl("bootout", m.target(svc))
		}
	})
	waitRunning := func() []State {
		var st []State
		for i := 0; i < 50; i++ {
			st, _ = m.Status()
			if len(st) == 2 && st[0].Running && st[1].Running && st[0].PID > 0 && st[1].PID > 0 {
				return st
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("not running: %+v\n%s", st, out.String())
		return nil
	}

	if err := m.Install(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	first := waitRunning()
	t.Logf("install:\n%s%+v", out.String(), first)
	out.Reset()
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	if again := waitRunning(); again[0].PID != first[0].PID || again[1].PID != first[1].PID {
		t.Fatalf("re-run changed PIDs: %+v → %+v", first, again)
	}
	if out.String() != "unchanged binary\nunchanged hub\nunchanged host\n" {
		t.Fatalf("re-run output %q", out.String())
	}
	if err := m.Restart(Hub); err != nil {
		t.Fatal(err)
	}
	if st := waitRunning(); st[0].PID == first[0].PID {
		t.Fatal("restart kept the hub PID")
	}
	if err := m.Stop(Host); err != nil || m.Running(Host) {
		t.Fatalf("stop: %v", err)
	}
	if err := m.Start(Host); err != nil {
		t.Fatal(err)
	}
	waitRunning()
	out.Reset()
	if err := m.Uninstall(true); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	t.Logf("purge:\n%s", out.String())
	for _, svc := range allNames {
		if b, err := RealLaunchctl("list"); err == nil && strings.Contains(string(b), m.inst().Label(svc)) {
			t.Fatalf("%s still loaded", svc)
		}
	}
	if exists(inst.ConfigDir()) || exists(inst.SigningKeychain()) {
		t.Fatal("purge left files")
	}
}
