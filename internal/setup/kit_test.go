package setup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexDobrushskiy/hommy/internal/browsers"
)

// fakeClaude is `claude plugin …` over an in-memory plugin registry.
type fakeClaude struct {
	markets map[string]string // name → source
	plugins map[string]*fakePlugin
	calls   []string
}

type fakePlugin struct {
	enabled bool
	version string
	config  map[string]string
}

func (f *fakeClaude) exec(_ context.Context, stdin, name string, args ...string) (string, error) {
	line := strings.Join(args, " ")
	f.calls = append(f.calls, filepath.Base(name)+" "+line)
	if strings.HasSuffix(name, "install.sh") {
		return "== summary\nchanged: 0\n", nil
	}
	switch {
	case line == "plugin list --json":
		var rows []map[string]any
		for id, p := range f.plugins {
			rows = append(rows, map[string]any{"id": id, "enabled": p.enabled, "version": p.version})
		}
		b, _ := json.Marshal(rows)
		return string(b), nil
	case line == "plugin marketplace list --json":
		var rows []map[string]any
		for n := range f.markets {
			rows = append(rows, map[string]any{"name": n})
		}
		b, _ := json.Marshal(rows)
		return string(b), nil
	case strings.HasPrefix(line, "plugin marketplace add "):
		src := args[3]
		name := map[string]string{RetainerSource: "retainer"}[src]
		if name == "" {
			name = filepath.Base(src)
			if strings.Contains(src, "browsers") {
				name = "hommy-browsers"
			} else {
				name = "hommy"
			}
		}
		f.markets[name] = src
	case strings.HasPrefix(line, "plugin install "):
		p := &fakePlugin{enabled: true, version: "1.0.0", config: map[string]string{}}
		for i := 3; i+1 < len(args); i += 2 {
			k, v, _ := strings.Cut(args[i+1], "=")
			p.config[k] = v
		}
		f.plugins[args[2]] = p
	case strings.HasPrefix(line, "plugin enable "):
		f.plugins[args[2]].enabled = true
	case strings.HasSuffix(line, "--values-stdin"):
		var v map[string]string
		json.Unmarshal([]byte(stdin), &v)
		for k, x := range v {
			f.plugins[args[2]].config[k] = x
		}
	case strings.HasPrefix(line, "plugin configure "):
		b, _ := json.Marshal(map[string]any{"inputs": f.plugins[args[2]].config})
		return string(b), nil
	}
	return "", nil
}

func TestKitPluginsIdempotent(t *testing.T) {
	r := testRunner(t, "studio", Options{PluginSource: "/src/hommy"})
	hb := browsers.Home(r.Cfg)
	os.MkdirAll(hb, 0o755)
	os.WriteFile(filepath.Join(hb, "install.sh"), []byte("#!/bin/sh\n"), 0o755)
	f := &fakeClaude{markets: map[string]string{}, plugins: map[string]*fakePlugin{}}
	r.Exec = f.exec
	run := func() int {
		t.Helper()
		r.changes = 0
		if err := r.kit(context.Background()); err != nil {
			t.Fatal(err)
		}
		return r.changes
	}
	if n := run(); n < 7 { // config.env, 3 marketplaces, 3 plugins
		t.Fatalf("first run: %d changes\n%s", n, strings.Join(f.calls, "\n"))
	}
	if got := f.plugins["hommy@hommy"].config["instance"]; got != "test-unit" {
		t.Errorf("hommy instance %q", got)
	}
	hbc := f.plugins["hommy-browsers@hommy-browsers"].config
	if hbc["lanes_port"] != "8476" || hbc["identity_port"] != "8477" || hbc["pool"] != "0" {
		t.Errorf("hommy-browsers config %v", hbc)
	}
	if f.markets["hommy"] != "/src/hommy" || f.markets["retainer"] != RetainerSource || f.markets["hommy-browsers"] != hb {
		t.Errorf("marketplaces %v", f.markets)
	}
	if n := run(); n != 0 {
		t.Fatalf("re-run: %d changes\n%s", n, strings.Join(f.calls, "\n"))
	}
	f.plugins["hommy@hommy"].enabled = false
	f.plugins["hommy@hommy"].config["instance"] = "other"
	if n := run(); n != 2 || !f.plugins["hommy@hommy"].enabled || f.plugins["hommy@hommy"].config["instance"] != "test-unit" {
		t.Fatalf("repair: %d changes, %+v", n, *f.plugins["hommy@hommy"])
	}
}

// --seed-context --dry-run prints the agterm command and the first message, and runs nothing else.
func TestSeedContextDryRun(t *testing.T) {
	var out strings.Builder
	r := testRunner(t, "studio", Options{SeedContext: true, DryRun: true})
	r.Out = &out
	if err := r.seedContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"agtermctl session new --workspace-name hommy --create-workspace --cwd ", "--command ",
		"first message: Run the retainer interview", "Do not rewrite AGENTS.md"} {
		if !strings.Contains(s, want) {
			t.Errorf("dry run lacks %q:\n%s", want, s)
		}
	}
}
