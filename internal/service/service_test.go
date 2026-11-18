package service

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/config"
)

// fakeLaunchd answers launchctl like launchd would for the labels it loaded.
type fakeLaunchd struct {
	loaded map[string]int // target → pid
	calls  []string
	pid    int
}

func (f *fakeLaunchd) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "print":
		pid, ok := f.loaded[args[1]]
		if !ok {
			return []byte("Could not find service"), fmt.Errorf("exit 113")
		}
		return []byte(fmt.Sprintf("%s = {\n\tactive count = 1\n\tstate = running\n\tpid = %d\n\tendpoints = {\n\t\tstate = waiting\n\t}\n}\n", args[1], pid)), nil
	case "bootstrap":
		// gui/<uid> /path/<label>.plist
		label := strings.TrimSuffix(filepath.Base(args[2]), ".plist")
		f.pid++
		f.loaded[args[1]+"/"+label] = f.pid
	case "bootout":
		delete(f.loaded, args[1])
	case "kickstart":
		t := args[len(args)-1]
		if _, ok := f.loaded[t]; !ok {
			return nil, fmt.Errorf("not loaded")
		}
		if args[1] == "-k" {
			f.pid++
			f.loaded[t] = f.pid
		}
	}
	return nil, nil
}

func (f *fakeLaunchd) count(verb string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, verb+" ") {
			n++
		}
	}
	return n
}

type rig struct {
	m      *Manager
	f      *fakeLaunchd
	out    *bytes.Buffer
	inst   config.Instance
	exe    string
	signed int
}

func newRig(t *testing.T, name string, speech string) *rig {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	inst, err := config.NewInstance(name, home)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(inst.ConfigDir(), 0o700)
	os.WriteFile(inst.ConfigFile(), []byte("SPEECH="+speech+"\n"), 0o600)
	cfg, err := config.Load(inst, inst.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.WithEnv(func(string) (string, bool) { return "", false })
	exe := filepath.Join(t.TempDir(), "hommy")
	os.WriteFile(exe, []byte("binary v1"), 0o755)
	r := &rig{f: &fakeLaunchd{loaded: map[string]int{}}, out: &bytes.Buffer{}, inst: inst, exe: exe}
	r.m = &Manager{Cfg: cfg, Exe: exe, Launchctl: r.f.run, Out: r.out,
		sign: func(p string) error {
			r.signed++
			b, _ := os.ReadFile(p)
			return os.WriteFile(p, append(b, " +sig"...), 0o755)
		},
		signedOK: func(p string) bool { b, _ := os.ReadFile(p); return bytes.HasSuffix(b, []byte(" +sig")) },
		lookPath: func(n string) (string, error) { return "/opt/homebrew/bin/" + n, nil },
		sleep:    func(time.Duration) {},
	}
	return r
}

func TestPlistRendering(t *testing.T) {
	r := newRig(t, "test", "local")
	hub, err := r.m.plist(Hub)
	if err != nil {
		t.Fatal(err)
	}
	s := string(hub)
	for _, want := range []string{
		"<string>dev.hommy.test.hub</string>",
		"<string>" + r.inst.BinPath() + "</string>\n\t\t<string>hub</string>\n\t\t<string>--instance</string>\n\t\t<string>test</string>",
		"<key>RunAtLoad</key>\n\t<true/>", "<key>KeepAlive</key>\n\t<true/>",
		"<key>ThrottleInterval</key>\n\t<integer>10</integer>",
		filepath.Join(r.inst.StateDir(), "hub", "out.log"), filepath.Join(r.inst.StateDir(), "hub", "err.log"),
		"<key>PATH</key>\n\t\t<string>/opt/homebrew/bin:/usr/bin:/bin:" + r.inst.Home + "/.local/bin</string>",
		"<key>HOME</key>\n\t\t<string>" + r.inst.Home + "</string>",
		"<key>HOMMY_INSTANCE</key>\n\t\t<string>test</string>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("hub plist lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "HOMMY_CONFIG") {
		t.Error("HOMMY_CONFIG set for the default config file")
	}
	sp, err := r.m.plist(Speech)
	if err != nil {
		t.Fatal(err)
	}
	want := "<string>/opt/homebrew/bin/llama-swap</string>\n\t\t<string>--config</string>\n\t\t<string>" +
		filepath.Join(r.inst.ConfigDir(), "llama-swap.yaml") + "</string>\n\t\t<string>--listen</string>\n\t\t<string>127.0.0.1:8474</string>\n\t\t<string>--watch-config</string>"
	if !strings.Contains(string(sp), want) {
		t.Errorf("speech plist:\n%s", sp)
	}
	// whisper-server --convert writes temp files into the working directory: never launchd's /
	if !strings.Contains(string(sp), "<key>WorkingDirectory</key>\n\t<string>"+filepath.Join(r.inst.StateDir(), "speech")+"</string>") {
		t.Errorf("speech plist without its WorkingDirectory:\n%s", sp)
	}
	// default instance: no --instance, no HOMMY_INSTANCE; no secret-shaped value anywhere
	d := newRig(t, "", "off")
	b, _ := d.m.plist(Host)
	if strings.Contains(string(b), "--instance") || strings.Contains(string(b), "HOMMY_INSTANCE") ||
		!strings.Contains(string(b), "<string>dev.hommy.host</string>") {
		t.Errorf("default host plist:\n%s", b)
	}
	for _, p := range [][]byte{hub, sp, b} {
		for _, bad := range []string{"token", "key", "secret", "password"} {
			for _, line := range strings.Split(string(p), "\n") {
				if strings.Contains(strings.ToLower(line), bad) && !strings.Contains(line, "<key>") {
					t.Errorf("secret-like value %q in plist line %q", bad, line)
				}
			}
		}
	}
}

func TestInstallIdempotent(t *testing.T) {
	r := newRig(t, "test-unit", "local")
	if err := r.m.Install("all"); err != nil {
		t.Fatal(err)
	}
	if r.f.count("bootstrap") != 3 || r.signed != 1 {
		t.Fatalf("first install: %d bootstraps, %d signs\n%v", r.f.count("bootstrap"), r.signed, r.f.calls)
	}
	for _, svc := range allNames {
		if !exists(r.inst.PlistPath(svc)) || !exists(r.inst.LogDir(svc)) {
			t.Fatalf("%s: plist or log dir missing", svc)
		}
	}
	pids := fmt.Sprint(r.f.loaded)
	binInfo, _ := os.Stat(r.inst.BinPath())
	if binInfo.Mode().Perm() != 0o755 {
		t.Fatalf("binary mode %v", binInfo.Mode())
	}
	r.f.calls = nil
	r.out.Reset()
	if err := r.m.Install("all"); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"bootstrap", "bootout", "kickstart"} {
		if n := r.f.count(verb); n != 0 {
			t.Fatalf("re-run with nothing changed: %d %s calls", n, verb)
		}
	}
	if r.signed != 1 || fmt.Sprint(r.f.loaded) != pids {
		t.Fatalf("re-run changed things: signs %d, pids %s → %v", r.signed, pids, r.f.loaded)
	}
	if got := r.out.String(); got != "unchanged binary\nunchanged hub\nunchanged host\nunchanged speech\n" {
		t.Fatalf("output %q", got)
	}

	// a booted-out service is bootstrapped again; the others stay
	delete(r.f.loaded, domain()+"/dev.hommy.test-unit.host")
	r.f.calls = nil
	r.m.Install()
	if r.f.count("bootstrap") != 1 || r.f.count("bootout") != 0 {
		t.Fatalf("only host should reload: %v", r.f.calls)
	}

	// a changed plist (SPEECH_LISTEN) reloads speech only
	os.WriteFile(r.inst.ConfigFile(), []byte("SPEECH=local\nSPEECH_LISTEN=127.0.0.1:9999\n"), 0o600)
	cfg, _ := config.Load(r.inst, r.inst.ConfigFile())
	r.m.Cfg = cfg.WithEnv(func(string) (string, bool) { return "", false })
	r.f.calls = nil
	r.m.Install()
	if r.f.count("bootout") != 1 || r.f.count("bootstrap") != 1 || !strings.Contains(strings.Join(r.f.calls, ";"), "bootout "+domain()+"/dev.hommy.test-unit.speech") {
		t.Fatalf("speech reload: %v", r.f.calls)
	}
}

func TestBinaryCopiedOnlyOnChange(t *testing.T) {
	r := newRig(t, "test-unit", "off")
	if err := r.m.Install(); err != nil {
		t.Fatal(err)
	}
	if r.signed != 1 {
		t.Fatalf("signs %d", r.signed)
	}
	if b, _ := os.ReadFile(r.inst.BinPath()); string(b) != "binary v1 +sig" {
		t.Fatalf("copy %q", b)
	}
	r.m.Install()
	if r.signed != 1 {
		t.Fatalf("unchanged source re-signed: %d", r.signed)
	}
	// a broken signature on the copy is re-signed without a new source
	os.WriteFile(r.inst.BinPath(), []byte("binary v1"), 0o755)
	r.m.Install()
	if r.signed != 2 {
		t.Fatalf("invalid signature not repaired: %d", r.signed)
	}
	// a new source: copied, signed, loaded services kickstarted so they run it
	os.WriteFile(r.exe, []byte("binary v2"), 0o755)
	r.f.calls = nil
	r.out.Reset()
	if err := r.m.Install(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(r.inst.BinPath()); string(b) != "binary v2 +sig" || r.signed != 3 {
		t.Fatalf("upgrade: %q signs %d", b, r.signed)
	}
	if r.f.count("kickstart") != 2 || r.f.count("bootstrap") != 0 {
		t.Fatalf("upgrade calls: %v", r.f.calls)
	}
	if !strings.Contains(r.out.String(), "signed "+r.inst.BinPath()+"\n") || !strings.Contains(r.out.String(), "restarted hub") {
		t.Fatalf("output %q", r.out.String())
	}
	// a failed signature leaves the old signed copy in place
	os.WriteFile(r.exe, []byte("binary v3"), 0o755)
	r.m.sign = func(string) error { return fmt.Errorf("no identity") }
	if err := r.m.Install(); err == nil {
		t.Fatal("sign failure not reported")
	}
	if b, _ := os.ReadFile(r.inst.BinPath()); string(b) != "binary v2 +sig" {
		t.Fatalf("after failed sign: %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(r.inst.BinPath()), ".hommy-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestSpeechFollowsSetting(t *testing.T) {
	r := newRig(t, "test-unit", "local")
	r.m.Install()
	os.WriteFile(r.inst.ConfigFile(), []byte("SPEECH=external\n"), 0o600)
	cfg, _ := config.Load(r.inst, r.inst.ConfigFile())
	r.m.Cfg = cfg.WithEnv(func(string) (string, bool) { return "", false })
	r.m.Install()
	if exists(r.inst.PlistPath(Speech)) || len(r.f.loaded) != 2 {
		t.Fatalf("speech not removed: %v", r.f.loaded)
	}
	// llama-swap missing: "all" skips speech, a named speech fails
	r2 := newRig(t, "test-unit", "local")
	r2.m.lookPath = func(string) (string, error) { return "", fmt.Errorf("nope") }
	if err := r2.m.Install(); err != nil || !strings.Contains(r2.out.String(), "skipped speech") {
		t.Fatalf("%v %q", err, r2.out.String())
	}
	if err := r2.m.Install(Speech); err == nil {
		t.Fatal("named speech without llama-swap succeeded")
	}
}

func TestStatusStartStop(t *testing.T) {
	r := newRig(t, "test-unit", "off")
	if err := r.m.Start(Hub); err == nil {
		t.Fatal("start before install")
	}
	r.m.Install()
	st, err := r.m.Status()
	if err != nil || len(st) != 2 || !st[0].Running || st[0].PID == 0 || st[0].Label != "dev.hommy.test-unit.hub" || !st[1].Installed {
		t.Fatalf("%+v %v", st, err)
	}
	r.m.Stop(Host)
	if r.m.Running(Host) || !r.m.Running(Hub) {
		t.Fatal("stop host")
	}
	r.m.Start(Host)
	pid := r.f.loaded[r.m.target(Hub)]
	r.m.Restart(Hub)
	if !r.m.Running(Host) || r.f.loaded[r.m.target(Hub)] == pid {
		t.Fatal("start/restart")
	}
	if _, err := r.m.Status("bogus"); err == nil {
		t.Fatal("unknown name accepted")
	}
	if run, pid := parsePrint("x = {\n\tstate = not running\n\tendpoints = {\n\t\tstate = active\n\t}\n}"); run || pid != 0 {
		t.Fatal("parsePrint")
	}
}

func TestPurgeRemovesOnlyItsOwn(t *testing.T) {
	r := newRig(t, "test-unit", "local")
	r.m.Install()
	inst := r.inst
	os.WriteFile(filepath.Join(inst.ConfigDir(), "llama-swap.yaml"), []byte("x"), 0o600)
	foreignParent := filepath.Join(filepath.Dir(inst.ConfigDir()), "other-app.conf")
	foreignAgent := filepath.Join(inst.Home, "Library", "LaunchAgents", "com.example.other.plist")
	foreignState := filepath.Join(filepath.Dir(inst.StateDir()), "other")
	os.WriteFile(foreignParent, []byte("x"), 0o600)
	os.WriteFile(foreignAgent, []byte("x"), 0o600)
	os.MkdirAll(foreignState, 0o700)

	// keep secret calls off any real keychain: the purge's Keychain part is covered by the signing test
	if err := r.m.Uninstall(false); err != nil {
		t.Fatal(err)
	}
	if len(r.f.loaded) != 0 || exists(inst.PlistPath(Hub)) || !exists(inst.ConfigFile()) {
		t.Fatal("plain uninstall")
	}
	if err := r.m.purgeFiles(); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{inst.ConfigFile(), inst.StateDir(), inst.ShareDir(), inst.ConfigDir()} {
		if exists(gone) {
			t.Errorf("%s survived purge", gone)
		}
	}
	for _, kept := range []string{foreignParent, foreignAgent, foreignState} {
		if !exists(kept) {
			t.Errorf("foreign %s removed", kept)
		}
	}
	// a foreign file inside the config dir keeps the dir
	r2 := newRig(t, "test-unit", "off")
	mine := filepath.Join(r2.inst.ConfigDir(), "notes.txt")
	os.WriteFile(mine, []byte("x"), 0o600)
	r2.m.purgeFiles()
	if !exists(mine) || exists(r2.inst.ConfigFile()) {
		t.Fatal("config dir with a foreign file")
	}
}

// A setup re-run that changed settings or secrets restarts the loaded hub and
// host (they read both only at start); speech and unchanged runs stay put.
func TestInstallRestartAppliesNewSettings(t *testing.T) {
	r := newRig(t, "test-unit", "local")
	if err := r.m.Install("all"); err != nil {
		t.Fatal(err)
	}
	pids := map[string]int{}
	for k, v := range r.f.loaded {
		pids[k] = v
	}
	r.f.calls = nil
	r.out.Reset()
	if err := r.m.InstallRestart(true, "all"); err != nil {
		t.Fatal(err)
	}
	if n := r.f.count("kickstart"); n != 2 {
		t.Fatalf("want 2 kickstarts (hub, host): %v", r.f.calls)
	}
	for _, svc := range []string{"hub", "host"} {
		k := domain() + "/dev.hommy.test-unit." + svc
		if r.f.loaded[k] == pids[k] {
			t.Fatalf("%s kept pid %d", svc, pids[k])
		}
	}
	if k := domain() + "/dev.hommy.test-unit.speech"; r.f.loaded[k] != pids[k] {
		t.Fatalf("speech restarted")
	}
	if got := r.out.String(); got != "unchanged binary\nrestarted hub (new settings)\nrestarted host (new settings)\nunchanged speech\n" {
		t.Fatalf("output %q", got)
	}
	r.f.calls = nil
	if err := r.m.InstallRestart(false, "all"); err != nil || r.f.count("kickstart") != 0 {
		t.Fatalf("restart=false kickstarted: %v %v", err, r.f.calls)
	}
}
