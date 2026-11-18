package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/browsers"
	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/speech/stack"
)

// DefaultPluginSource is the hommy marketplace (the public repo).
const DefaultPluginSource = "AlexDobrushskiy/hommy"

// RetainerSource is retainer's own public marketplace.
const RetainerSource = "AlexDobrushskiy/retainer"

// --- 6. the local speech stack (SPEECH=local) ---

// LocalSpeech renders <config>/llama-swap.yaml and downloads the pinned whisper
// model; mlx-audio must already be installed (install.sh stage 4). It reports
// what it changed.
func LocalSpeech(ctx context.Context, c *config.Config, out io.Writer) (int, error) {
	dirs := []string{"/opt/homebrew/bin", "/usr/bin", "/bin", filepath.Join(c.Inst.Home, ".local", "bin")}
	paths := map[string]string{}
	var missing []string
	for _, t := range stack.Tools {
		p, ok := stack.Find(dirs, t.Name)
		if !ok {
			missing = append(missing, t.Name+" ("+t.Fix+")")
			continue
		}
		paths[t.Name] = p
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("speech stack incomplete: %s not found; run install.sh, or hommy setup --speech off", strings.Join(missing, ", "))
	}
	m, err := stack.ModelFor(c.Get("WHISPER_MODEL"))
	if err != nil {
		return 0, err
	}
	n := 0
	modelPath := stack.ModelPath(c.Inst.ShareDir(), m)
	fmt.Fprintf(out, "  whisper model %s (%d MB) …\n", m.Name, m.Size>>20)
	got, err := stack.Download(ctx, &http.Client{}, modelPath, m, out)
	if err != nil {
		return n, err
	}
	if got {
		n++
		fmt.Fprintf(out, "  downloaded %s (SHA-256 verified)\n", modelPath)
	}
	logDir := c.Inst.LogDir("speech")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return n, err
	}
	yaml := stack.Render(stack.Params{StartPort: c.PortBase() + 10000, WhisperBin: paths["whisper-server"],
		MlxAudioBin: paths["mlx_audio.server"], ModelFile: modelPath, LogDir: logDir})
	changed, err := stack.WriteIfChanged(filepath.Join(c.Inst.ConfigDir(), "llama-swap.yaml"), yaml)
	if err != nil {
		return n, err
	}
	if changed {
		n++
		fmt.Fprintf(out, "  wrote %s\n", filepath.Join(c.Inst.ConfigDir(), "llama-swap.yaml"))
	}
	return n, nil
}

// Warm makes the gateway load both models (whisper transcribes the sample,
// kokoro says "ok"), so the first voice note does not wait for a cold start.
func Warm(ctx context.Context, c *config.Config, out io.Writer) error {
	gw := "http://" + c.Get("SPEECH_LISTEN")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	// the gateway answers once launchd has started it
	for {
		if resp, err := http.Get(gw + "/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("speech gateway %s does not answer", gw)
		case <-time.After(500 * time.Millisecond):
		}
	}
	t0 := time.Now()
	txt, err := stack.Transcribe(ctx, nil, gw+"/upstream/whisper/v1", stack.Sample)
	if err != nil {
		return fmt.Errorf("warm whisper: %w", err)
	}
	fmt.Fprintf(out, "  whisper warm (%q, %s)\n", txt, time.Since(t0).Round(100*time.Millisecond))
	t0 = time.Now()
	if _, err := stack.Synthesize(ctx, nil, gw+"/upstream/kokoro/v1", c.Get("TTS_MODEL"), c.Get("TTS_DEFAULT_VOICE"), "en", "ok"); err != nil {
		return fmt.Errorf("warm kokoro: %w", err)
	}
	fmt.Fprintf(out, "  kokoro warm (%s)\n", time.Since(t0).Round(100*time.Millisecond))
	return nil
}

// --- 9. browsers and plugins ---

func (r *runner) kit(ctx context.Context) error {
	hb := browsers.Home(r.Cfg)
	hbOK := false
	if _, err := os.Stat(filepath.Join(hb, "install.sh")); err != nil {
		r.say("! browsers: %s is not installed (install.sh installs it); the browser lanes stay off", hb)
	} else {
		if err := r.browsers(ctx, hb); err != nil {
			return err
		}
		hbOK = true
	}
	if r.Exec == nil {
		return nil
	}
	return r.plugins(ctx, hb, hbOK)
}

var changedRe = regexp.MustCompile(`(?m)^changed: (\d+)\s*$`)

func (r *runner) browsers(ctx context.Context, hb string) error {
	changed, err := browsers.WriteConfig(r.Cfg, browsers.Node())
	if err != nil {
		return err
	}
	lanes, _ := browsers.Ports(r.Cfg)
	if changed {
		r.changes++
	}
	if !changed && browsers.Initialize(ctx, lanes) == nil {
		r.say("✓ browsers %s (lanes MCP :%d)", hb, lanes)
		return nil
	}
	if r.Exec == nil {
		return nil
	}
	out, err := r.Exec(ctx, "", filepath.Join(hb, "install.sh"))
	if err != nil {
		return fmt.Errorf("hommy-browsers install.sh failed: %s", tail(out, 6))
	}
	if m := changedRe.FindStringSubmatch(out); m != nil {
		n, _ := strconv.Atoi(m[1])
		r.changes += n
	}
	r.say("✓ browsers %s configured for instance %s (lanes MCP :%d)", hb, instName(r.Cfg), lanes)
	return nil
}

type pluginRow struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	Version string `json:"version"`
}

func (r *runner) claude(ctx context.Context, stdin string, args ...string) (string, error) {
	out, err := r.Exec(ctx, stdin, r.Cfg.Get("CLAUDE_BIN"), args...)
	if err != nil {
		return out, fmt.Errorf("claude %s: %s", strings.Join(args, " "), tail(out, 3))
	}
	return out, nil
}

func (r *runner) pluginList(ctx context.Context) (map[string]pluginRow, error) {
	out, err := r.claude(ctx, "", "plugin", "list", "--json")
	if err != nil {
		return nil, err
	}
	var rows []pluginRow
	if err := json.Unmarshal([]byte(jsonPart(out)), &rows); err != nil {
		return nil, fmt.Errorf("claude plugin list: not JSON")
	}
	m := map[string]pluginRow{}
	for _, x := range rows {
		m[x.ID] = x
	}
	return m, nil
}

func (r *runner) marketplaces(ctx context.Context) (map[string]bool, error) {
	out, err := r.claude(ctx, "", "plugin", "marketplace", "list", "--json")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(jsonPart(out)), &rows); err != nil {
		return nil, fmt.Errorf("claude plugin marketplace list: not JSON")
	}
	m := map[string]bool{}
	for _, x := range rows {
		m[x.Name] = true
	}
	return m, nil
}

type wantPlugin struct {
	id, market, source string
	config             map[string]string // userConfig values (strings as the CLI takes them)
}

func (r *runner) plugins(ctx context.Context, hb string, hbOK bool) error {
	src := r.o.PluginSource
	if src == "" {
		src = DefaultPluginSource
	}
	if !strings.Contains(src, "://") && !strings.HasPrefix(src, "/") && strings.Count(src, "/") != 1 {
		if abs, err := filepath.Abs(src); err == nil {
			src = abs
		}
	}
	want := []wantPlugin{{id: "hommy@hommy", market: "hommy", source: src, config: map[string]string{"instance": r.Cfg.Inst.Name}}}
	if hbOK {
		lanes, identity := browsers.Ports(r.Cfg)
		pool := "4"
		if r.Cfg.Inst.Name != "" {
			pool = "0"
		}
		want = append(want, wantPlugin{id: "hommy-browsers@hommy-browsers", market: "hommy-browsers", source: hb,
			config: map[string]string{"lanes_port": strconv.Itoa(lanes), "identity_port": strconv.Itoa(identity), "pool": pool}})
	}
	want = append(want, wantPlugin{id: "retainer@retainer", market: "retainer", source: RetainerSource})

	markets, err := r.marketplaces(ctx)
	if err != nil {
		return err
	}
	before, err := r.pluginList(ctx)
	if err != nil {
		return err
	}
	for _, w := range want {
		if !markets[w.market] {
			if _, err := r.claude(ctx, "", "plugin", "marketplace", "add", w.source); err != nil {
				return err
			}
			r.changes++
		} else if _, err := r.claude(ctx, "", "plugin", "marketplace", "update", w.market); err != nil {
			r.say("! marketplace %s not refreshed: %v", w.market, err)
		}
		cur, installed := before[w.id]
		switch {
		case !installed:
			args := []string{"plugin", "install", w.id}
			for _, k := range sortedKeys(w.config) {
				args = append(args, "--config", k+"="+w.config[k])
			}
			if _, err := r.claude(ctx, "", args...); err != nil {
				return err
			}
			r.changes++
			r.say("✓ plugin %s installed", w.id)
			continue
		case !cur.Enabled:
			if _, err := r.claude(ctx, "", "plugin", "enable", w.id); err != nil {
				return err
			}
			r.changes++
		}
		if _, err := r.claude(ctx, "", "plugin", "update", w.id); err != nil {
			r.say("! plugin %s not updated: %v", w.id, err)
		}
		if len(w.config) > 0 {
			if err := r.pluginConfig(ctx, w); err != nil {
				return err
			}
		}
	}
	after, err := r.pluginList(ctx)
	if err != nil {
		return err
	}
	for _, w := range want {
		b, a := before[w.id], after[w.id]
		if b.ID != "" && a.Version != b.Version {
			r.changes++
			r.say("✓ plugin %s upgraded %s → %s", w.id, b.Version, a.Version)
		} else if b.ID != "" {
			r.say("✓ plugin %s %s", w.id, a.Version)
		}
		if !a.Enabled {
			return fmt.Errorf("plugin %s is not enabled after setup", w.id)
		}
	}
	return nil
}

// pluginConfig sets the plugin's userConfig values that differ.
func (r *runner) pluginConfig(ctx context.Context, w wantPlugin) error {
	out, err := r.claude(ctx, "", "plugin", "configure", w.id, "--json")
	if err != nil {
		return err
	}
	var v struct {
		Inputs map[string]any `json:"inputs"`
	}
	if err := json.Unmarshal([]byte(jsonPart(out)), &v); err != nil {
		return fmt.Errorf("claude plugin configure %s: not JSON", w.id)
	}
	diff := map[string]string{}
	for k, want := range w.config {
		if fmt.Sprint(v.Inputs[k]) != want && !(want == "" && v.Inputs[k] == nil) {
			diff[k] = want
		}
	}
	if len(diff) == 0 {
		return nil
	}
	in, _ := json.Marshal(diff)
	if _, err := r.claude(ctx, string(in), "plugin", "configure", w.id, "--values-stdin"); err != nil {
		return err
	}
	r.changes++
	r.say("✓ plugin %s configured (%s)", w.id, strings.Join(sortedKeys(diff), ", "))
	return nil
}

func sortedKeys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0 && ks[j] < ks[j-1]; j-- {
			ks[j], ks[j-1] = ks[j-1], ks[j]
		}
	}
	return ks
}

func jsonPart(s string) string {
	if i := strings.IndexAny(s, "[{"); i > 0 {
		return s[i:]
	}
	return s
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// --- 11. context ---

// ContextMessage is the first message of the context session: the retainer
// interview into context/ and active/Dashboard.md, the kernel untouched.
const ContextMessage = "Run the retainer interview (the retainer plugin) to fill the context/ files of this vault and " +
	"active/Dashboard.md from my answers: one question at a time, short answers are fine. Do not rewrite AGENTS.md: it is " +
	"the hommy kernel (routing table and conventions); write to context/ and active/ only, and keep index.md current."

func (r *runner) seedContext(ctx context.Context) error {
	if !r.o.SeedContext {
		if r.o.NonInteractive || r.o.Join != "" {
			return nil
		}
		a, err := r.ask("seed your vault's context now with the retainer interview? (y/N)", "--seed-context", "n")
		if err != nil || !strings.HasPrefix(strings.ToLower(a), "y") {
			return err
		}
	}
	c := r.Cfg
	cmd := agterm.AgentCommandLineArgs(c.Get("AGENT_COMMAND"), c.Inst.Home, os.Getenv("CLAUDE_CONFIG_DIR"),
		c.Get("AGENT_PERMISSIONS") == "skip", ContextMessage)
	o := agterm.NewSessionOpts{Workspace: c.Get("AGTERM_WORKSPACE"), Cwd: c.GetPath("VAULT_DIR"), Name: "hommy-context", Command: cmd}
	if r.o.DryRun {
		r.say("%s", agterm.NewSessionCommand(o))
		r.say("first message: %s", ContextMessage)
		return nil
	}
	if r.NewSession == nil {
		return nil
	}
	id, err := r.NewSession(ctx, o)
	if err != nil {
		return fmt.Errorf("open the context session: %w", err)
	}
	r.say("✓ context: agterm session %s in workspace %s runs the retainer interview", id, o.Workspace)
	return nil
}
