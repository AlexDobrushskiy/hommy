package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

var errNoLlamaSwap = errors.New("llama-swap not found on PATH (install.sh installs it)")

// pathDirs is the PATH every service runs with.
func (m *Manager) pathDirs() []string {
	return []string{"/opt/homebrew/bin", "/usr/bin", "/bin", filepath.Join(m.inst().Home, ".local", "bin")}
}

// findTool resolves a tool to an absolute path: the services' PATH first, then this process's.
func (m *Manager) findTool(name string) (string, error) {
	if m.lookPath != nil {
		return m.lookPath(name)
	}
	for _, d := range m.pathDirs() {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// args is the service's ProgramArguments.
func (m *Manager) args(svc string) ([]string, error) {
	inst := m.inst()
	if svc == Speech {
		bin, err := m.findTool("llama-swap")
		if err != nil {
			return nil, errNoLlamaSwap
		}
		return []string{bin, "--config", filepath.Join(inst.ConfigDir(), "llama-swap.yaml"),
			"--listen", m.Cfg.Get("SPEECH_LISTEN"), "--watch-config"}, nil
	}
	a := []string{inst.BinPath(), svc}
	if inst.Name != "" {
		a = append(a, "--instance", inst.Name)
	}
	return a, nil
}

// env is the service's EnvironmentVariables: never a secret (I4).
func (m *Manager) env() map[string]string {
	inst := m.inst()
	e := map[string]string{
		"PATH": strings.Join(m.pathDirs(), ":"),
		"HOME": inst.Home,
	}
	if inst.Name != "" {
		e["HOMMY_INSTANCE"] = inst.Name
	}
	if m.Cfg.Path != "" && m.Cfg.Path != inst.ConfigFile() {
		e["HOMMY_CONFIG"] = m.Cfg.Path
	}
	return e
}

// plist renders the service's launchd plist.
func (m *Manager) plist(svc string) ([]byte, error) {
	args, err := m.args(svc)
	if err != nil {
		return nil, err
	}
	inst := m.inst()
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	str := func(indent, k, v string) {
		b.WriteString(indent + "<key>" + esc(k) + "</key>\n" + indent + "<string>" + esc(v) + "</string>\n")
	}
	str("\t", "Label", inst.Label(svc))
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range args {
		b.WriteString("\t\t<string>" + esc(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>KeepAlive</key>\n\t<true/>\n")
	b.WriteString("\t<key>ThrottleInterval</key>\n\t<integer>10</integer>\n")
	str("\t", "StandardOutPath", filepath.Join(inst.LogDir(svc), "out.log"))
	str("\t", "StandardErrorPath", filepath.Join(inst.LogDir(svc), "err.log"))
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	env := m.env()
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		str("\t\t", k, env[k])
	}
	b.WriteString("\t</dict>\n</dict>\n</plist>\n")
	return b.Bytes(), nil
}

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
