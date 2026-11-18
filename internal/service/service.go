// Package service installs and drives hommy's launchd agents (hub, host,
// speech) and keeps the signed stable copy of the binary they run (master §4.8).
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/secret"
)

// Service names.
const (
	Hub    = "hub"
	Host   = "host"
	Speech = "speech"
)

var allNames = []string{Hub, Host, Speech}

// Manager installs and controls one instance's services.
type Manager struct {
	Cfg       *config.Config
	Exe       string                               // the running binary ("" = os.Executable)
	Launchctl func(args ...string) ([]byte, error) // nil = /bin/launchctl
	Out       io.Writer                            // progress lines (nil = discarded)

	// test seams
	sign     func(path string) error           // nil = codesign with the instance identity
	signedOK func(path string) bool            // nil = Requirement(path) == nil
	lookPath func(name string) (string, error) // nil = the plist PATH, then the process PATH
	sleep    func(time.Duration)               // nil = time.Sleep
}

// State is one service as launchd sees it.
type State struct {
	Name      string
	Label     string
	Installed bool // the plist exists
	Loaded    bool
	Running   bool
	PID       int
}

func (m *Manager) inst() config.Instance { return m.Cfg.Inst }

func (m *Manager) printf(format string, a ...any) {
	if m.Out != nil {
		fmt.Fprintf(m.Out, format+"\n", a...)
	}
}

func (m *Manager) launchctl(args ...string) ([]byte, error) {
	if m.Launchctl != nil {
		return m.Launchctl(args...)
	}
	return RealLaunchctl(args...)
}

// RealLaunchctl runs /bin/launchctl with a timeout and stdin closed.
func RealLaunchctl(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput()
	if ctx.Err() != nil {
		return out, fmt.Errorf("launchctl %s: timed out", args[0])
	}
	if err != nil {
		return out, fmt.Errorf("launchctl %s: %s", strings.Join(args, " "), firstLine(string(out), err))
	}
	return out, nil
}

func (m *Manager) wait(d time.Duration) {
	if m.sleep != nil {
		m.sleep(d)
		return
	}
	time.Sleep(d)
}

func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func (m *Manager) target(name string) string { return domain() + "/" + m.inst().Label(name) }

func (m *Manager) speechLocal() bool { return m.Cfg.Get("SPEECH") == "local" }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// resolve expands names ("" or "all" = every service the instance has) in the
// canonical order. install: "all" includes speech only with SPEECH=local; else
// "all" also includes speech when its plist exists.
func (m *Manager) resolve(names []string, install bool) (svcs []string, all bool, err error) {
	want := map[string]bool{}
	if len(names) == 0 {
		names = []string{"all"}
	}
	for _, n := range names {
		switch n {
		case "all":
			all = true
			want[Hub], want[Host] = true, true
			if m.speechLocal() || (!install && exists(m.inst().PlistPath(Speech))) {
				want[Speech] = true
			}
		case Hub, Host, Speech:
			want[n] = true
		default:
			return nil, false, fmt.Errorf("unknown service %q (hub, host, speech, all)", n)
		}
	}
	for _, n := range allNames {
		if want[n] {
			svcs = append(svcs, n)
		}
	}
	return svcs, all, nil
}

// Install copies and signs the binary, writes the plists and (re)loads only the
// services whose plist changed or that are not loaded (§4.8, I5).
func (m *Manager) Install(names ...string) error { return m.InstallRestart(false, names...) }

// InstallRestart is Install; with restart it also restarts a loaded hub or host
// whose plist is unchanged, for settings or secrets they read only at start.
func (m *Manager) InstallRestart(restart bool, names ...string) error {
	svcs, all, err := m.resolve(names, true)
	if err != nil {
		return err
	}
	binChanged := false
	if slices.Contains(svcs, Hub) || slices.Contains(svcs, Host) {
		if binChanged, err = m.ensureBinary(); err != nil {
			return err
		}
	}
	for _, svc := range svcs {
		if err := m.installOne(svc, binChanged, restart, slices.Contains(names, Speech)); err != nil {
			return err
		}
	}
	if all && !m.speechLocal() && exists(m.inst().PlistPath(Speech)) {
		if err := m.remove(Speech); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) installOne(svc string, binChanged, restart, explicit bool) error {
	inst := m.inst()
	data, err := m.plist(svc)
	if errors.Is(err, errNoLlamaSwap) && !explicit {
		m.printf("skipped %s (llama-swap not found: run install.sh)", svc)
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(inst.LogDir(svc), 0o700); err != nil {
		return err
	}
	changed, err := writeIfChanged(inst.PlistPath(svc), data, 0o644)
	if err != nil {
		return err
	}
	loaded := m.loaded(svc)
	switch {
	case changed || !loaded:
		if loaded {
			if err := m.bootout(svc); err != nil {
				return err
			}
		}
		if err := m.bootstrap(svc); err != nil {
			return err
		}
		m.printf("installed %s (%s)", svc, inst.Label(svc))
	case (binChanged || restart) && svc != Speech:
		if _, err := m.launchctl("kickstart", "-k", m.target(svc)); err != nil {
			return err
		}
		why := "new settings"
		if binChanged {
			why = "new binary"
		}
		m.printf("restarted %s (%s)", svc, why)
	default:
		m.printf("unchanged %s", svc)
	}
	return nil
}

// loaded reports whether launchd knows the label.
func (m *Manager) loaded(svc string) bool {
	_, err := m.launchctl("print", m.target(svc))
	return err == nil
}

func (m *Manager) bootout(svc string) error {
	if _, err := m.launchctl("bootout", m.target(svc)); err != nil && m.loaded(svc) {
		return err
	}
	for i := 0; i < 50 && m.loaded(svc); i++ {
		m.wait(100 * time.Millisecond)
	}
	return nil
}

// bootstrap loads the plist; launchd can refuse for a moment right after a bootout.
func (m *Manager) bootstrap(svc string) error {
	var err error
	for i := 0; i < 10; i++ {
		if _, err = m.launchctl("bootstrap", domain(), m.inst().PlistPath(svc)); err == nil {
			return nil
		}
		if m.loaded(svc) {
			return nil
		}
		m.wait(300 * time.Millisecond)
	}
	return err
}

// remove boots the service out and deletes its plist.
func (m *Manager) remove(svc string) error {
	p := m.inst().PlistPath(svc)
	loaded := m.loaded(svc)
	if !loaded && !exists(p) {
		m.printf("not installed %s", svc)
		return nil
	}
	if loaded {
		if err := m.bootout(svc); err != nil {
			return err
		}
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	m.printf("removed %s (%s)", svc, m.inst().Label(svc))
	return nil
}

// Uninstall boots out and removes the plists; purge also removes exactly what
// hommy created for the instance (§4.8) and always covers every service.
func (m *Manager) Uninstall(purge bool, names ...string) error {
	svcs := allNames
	if !purge {
		var err error
		if svcs, _, err = m.resolve(names, false); err != nil {
			return err
		}
	}
	for _, svc := range svcs {
		if err := m.remove(svc); err != nil {
			return err
		}
	}
	if purge {
		return m.purge()
	}
	return nil
}

func (m *Manager) purge() error {
	inst := m.inst()
	var errs []error
	s := m.signer()
	if exists(inst.SigningKeychain()) {
		if err := s.deleteKeychain(); err != nil {
			errs = append(errs, err)
		} else {
			m.printf("purged %s", inst.SigningKeychain())
		}
	}
	for _, item := range []string{secret.TelegramToken, secret.HostKey, secret.HubHostKeys, secret.GeminiKey,
		secret.SigningPassword} {
		if !s.store().Exists(item) {
			continue
		}
		if err := s.store().Delete(item); err != nil {
			errs = append(errs, err)
		} else {
			m.printf("purged keychain item %s %s", inst.KeychainService(), item)
		}
	}
	return errors.Join(append(errs, m.purgeFiles())...)
}

// purgeFiles removes hommy's files: the config file, llama-swap.yaml, the
// state and share dirs, and the config dir only when nothing else is in it.
func (m *Manager) purgeFiles() error {
	inst := m.inst()
	var errs []error
	for _, f := range []string{inst.ConfigFile(), filepath.Join(inst.ConfigDir(), "llama-swap.yaml")} {
		if err := os.Remove(f); err == nil {
			m.printf("purged %s", f)
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	for _, d := range []string{inst.StateDir(), inst.ShareDir()} {
		if !exists(d) {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			errs = append(errs, err)
		} else {
			m.printf("purged %s", d)
		}
	}
	if err := os.Remove(inst.ConfigDir()); err == nil {
		m.printf("purged %s", inst.ConfigDir())
	}
	return errors.Join(errs...)
}

// Start loads a stopped service (or starts a loaded one that is not running).
func (m *Manager) Start(names ...string) error {
	return m.each(names, func(svc string) error {
		if !exists(m.inst().PlistPath(svc)) {
			return fmt.Errorf("%s is not installed: run hommy service install", svc)
		}
		if !m.loaded(svc) {
			if err := m.bootstrap(svc); err != nil {
				return err
			}
		} else if _, err := m.launchctl("kickstart", m.target(svc)); err != nil {
			return err
		}
		m.printf("started %s", svc)
		return nil
	})
}

// Stop unloads the service (KeepAlive would restart a killed one); the plist stays.
func (m *Manager) Stop(names ...string) error {
	return m.each(names, func(svc string) error {
		if !m.loaded(svc) {
			m.printf("not running %s", svc)
			return nil
		}
		if err := m.bootout(svc); err != nil {
			return err
		}
		m.printf("stopped %s", svc)
		return nil
	})
}

// Restart kills and restarts a loaded service, or loads a stopped one.
func (m *Manager) Restart(names ...string) error {
	return m.each(names, func(svc string) error {
		if !exists(m.inst().PlistPath(svc)) {
			return fmt.Errorf("%s is not installed: run hommy service install", svc)
		}
		if !m.loaded(svc) {
			if err := m.bootstrap(svc); err != nil {
				return err
			}
		} else if _, err := m.launchctl("kickstart", "-k", m.target(svc)); err != nil {
			return err
		}
		m.printf("restarted %s", svc)
		return nil
	})
}

func (m *Manager) each(names []string, fn func(string) error) error {
	svcs, _, err := m.resolve(names, false)
	if err != nil {
		return err
	}
	for _, svc := range svcs {
		if err := fn(svc); err != nil {
			return err
		}
	}
	return nil
}

// Status reads each service from `launchctl print gui/<uid>/<label>`.
func (m *Manager) Status(names ...string) ([]State, error) {
	svcs, _, err := m.resolve(names, false)
	if err != nil {
		return nil, err
	}
	var out []State
	for _, svc := range svcs {
		st := State{Name: svc, Label: m.inst().Label(svc), Installed: exists(m.inst().PlistPath(svc))}
		if b, err := m.launchctl("print", m.target(svc)); err == nil {
			st.Loaded = true
			st.Running, st.PID = parsePrint(string(b))
		}
		out = append(out, st)
	}
	return out, nil
}

// Running reports whether the service is loaded and running.
func (m *Manager) Running(name string) bool {
	st, err := m.Status(name)
	return err == nil && len(st) == 1 && st[0].Running
}

// parsePrint reads the job's top-level `state =` and `pid =` lines.
func parsePrint(s string) (running bool, pid int) {
	seenState, seenPID := false, false
	for _, line := range strings.Split(s, "\n") {
		l := strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(l, "state = "); ok && !seenState {
			seenState, running = true, v == "running"
		}
		if v, ok := strings.CutPrefix(l, "pid = "); ok && !seenPID {
			seenPID = true
			pid, _ = strconv.Atoi(v)
		}
	}
	return running, pid
}

// writeIfChanged writes data atomically when the file's bytes differ.
func writeIfChanged(path string, data []byte, mode os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hommy-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}

func firstLine(msg string, err error) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return err.Error()
	}
	l, _, _ := strings.Cut(msg, "\n")
	return l
}
