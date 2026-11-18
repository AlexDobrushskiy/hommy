// Package browsers is hommy's side of hommy-browsers (master spec §3.1, §7):
// the engine's config.env for an instance, its ports, and the MCP probe.
package browsers

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/config"
)

// Engine defaults (the default instance uses them as they are).
const (
	DefaultPortBase  = 8931
	DefaultDebugBase = 9331
)

// NodeCandidates are where Homebrew's node lives (stable path first): the
// engine runs on the Node the installer brought, never one a version manager
// put on PATH.
var NodeCandidates = []string{"/opt/homebrew/opt/node/bin/node", "/opt/homebrew/bin/node"}

// Node is the first candidate that exists ("" = let the engine find node).
func Node() string {
	for _, p := range NodeCandidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// Home is the instance's engine install (HB_HOME).
func Home(c *config.Config) string { return c.GetPath("HB_HOME") }

// Ports are the lanes and identity MCP ports of the instance.
func Ports(c *config.Config) (lanes, identity int) {
	if c.Inst.Name == "" {
		return DefaultPortBase, DefaultPortBase + 1
	}
	return c.PortBase() + 6, c.PortBase() + 7
}

// ConfigEnv renders <HB_HOME>/config.env for the instance: the default
// instance keeps the engine's defaults; instance X gets its own ports, labels,
// bundle id, state dir and pool 0.
func ConfigEnv(c *config.Config, node string) []byte {
	var b strings.Builder
	b.WriteString("# hommy-browsers for hommy instance " + instName(c) + "; written by hommy setup (rewritten on every run)\n")
	if c.Inst.Name != "" {
		p := c.Inst.LabelPrefix()
		fmt.Fprintf(&b, "HB_PORT_BASE=%d\n", c.PortBase()+6)
		fmt.Fprintf(&b, "HB_DEBUG_BASE=%d\n", c.PortBase()+8)
		b.WriteString("HB_POOL=0\n")
		b.WriteString("HB_LABEL_PREFIX=" + p + ".browsers\n")
		b.WriteString("HB_BUNDLE_ID=" + p + ".agent-chrome\n")
		b.WriteString("HB_STATE_DIR=" + filepath.Join(c.Inst.Home, ".local", "state", c.Inst.Base()+"-browsers") + "\n")
		b.WriteString("HB_REAPER_SCOPE=own\n")
	}
	if node != "" {
		b.WriteString("HB_NODE=" + node + "\n")
	}
	return []byte(b.String())
}

func instName(c *config.Config) string {
	if c.Inst.Name == "" {
		return "default"
	}
	return c.Inst.Name
}

// WriteConfig writes config.env (0600) when its bytes differ.
func WriteConfig(c *config.Config, node string) (bool, error) {
	p := filepath.Join(Home(c), "config.env")
	data := ConfigEnv(c, node)
	if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	if err := os.WriteFile(p+".tmp", data, 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(p+".tmp", p)
}

// ParseEnv reads the KEY=VALUE lines of an engine config.env.
func ParseEnv(b []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out
}

// LanesPort is the lanes MCP port of an install's config.env (the instance's port without one).
func LanesPort(c *config.Config, env map[string]string) int {
	if n, err := strconv.Atoi(env["HB_PORT_BASE"]); err == nil && n > 0 {
		return n
	}
	lanes, _ := Ports(c)
	return lanes
}

// Initialize sends MCP initialize to http://127.0.0.1:port/mcp and checks for serverInfo.
func Initialize(ctx context.Context, port int) error {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"hommy-doctor","version":"1"}}}`
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/mcp",
		strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("lanes MCP :%d does not answer", port)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK || !bytes.Contains(b, []byte("serverInfo")) {
		return fmt.Errorf("lanes MCP :%d answered HTTP %d without serverInfo", port, resp.StatusCode)
	}
	return nil
}

// ChromeCandidates are where Google Chrome is looked for: /Applications, then ~/Applications.
func ChromeCandidates(home string) []string {
	return []string{"/Applications/Google Chrome.app", filepath.Join(home, "Applications", "Google Chrome.app")}
}
