// Package setup is `hommy setup`: preflight, instance config, bot token, vault,
// permissions, speech URLs, keys, services, pairing and a final doctor
// (master spec §4.6; phase 02 builds steps 1–8, 10 and 12).
package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/doctor"
	"github.com/AlexDobrushskiy/hommy/internal/secret"
)

// Options are setup's flags.
type Options struct {
	NonInteractive bool
	PortBase       string
	HostName       string
	TokenStdin     bool
	TelegramAPI    string
	Vault          string
	Permissions    string // skip | ask | "" (keep)
	Speech         string // local | external | off | "" (keep)
	STTURL, TTSURL string
	WhisperModel   string
	ClaudeBin      string
	NoPairWait     bool
	// --join: host-only install for another Mac.
	Join      string // the hub's ws:// URL
	Name      string // this host's name
	KeyStdin  bool
	DryRun    bool
	SkipPrefl bool // tests: skip the platform/agterm/claude preflight
}

// ErrUsage is a missing required value under --non-interactive (exit 2).
var ErrUsage = errors.New("usage")

// Env is what setup needs from outside (faked in tests).
type Env struct {
	Cfg    *config.Config
	Store  secret.Store
	In     io.Reader
	Out    io.Writer
	Probes doctor.Probes
	// Install is `hommy service install`; restart also restarts a loaded hub/host
	// whose plist is unchanged (setup changed what they read at start).
	Install func(restart bool, names ...string) error
	Pair    func(ctx context.Context) error
	// Ready waits for the hub and host to come up after install.
	Ready func(ctx context.Context, hubRole bool) error
	// Doctor runs the final checks; it returns the results.
	Doctor func(ctx context.Context) []doctor.Result
}

type runner struct {
	Env
	o       Options
	in      *bufio.Reader
	botName string
	// what the daemons read at start: the config file as it was, and whether a secret changed
	cfgBefore      []byte
	secretsChanged bool
}

// Run is `hommy setup`.
func Run(ctx context.Context, e Env, o Options) error {
	r := &runner{Env: e, o: o, in: bufio.NewReader(e.In)}
	r.cfgBefore, _ = os.ReadFile(e.Cfg.Path)
	join := o.Join != ""
	if join && (o.Name == "" || !o.KeyStdin) {
		return fmt.Errorf("%w: --join needs --name and --key-stdin", ErrUsage)
	}
	steps := []struct {
		name string
		fn   func(context.Context) error
		join bool
	}{
		{"preflight", r.preflight, true},
		{"config", r.configStep, true},
		{"token", r.token, false},
		{"vault", r.vault, true},
		{"permissions", r.permissions, true},
		{"speech", r.speech, false},
		{"keys", r.keys, true},
		{"services", r.services, true},
		{"pairing", r.pairing, false},
	}
	for _, s := range steps {
		if join && !s.join {
			continue
		}
		if err := s.fn(ctx); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return r.finish(ctx)
}

func (r *runner) say(format string, a ...any) { fmt.Fprintf(r.Out, format+"\n", a...) }

// ask prompts for a value (interactive) or fails with ErrUsage.
func (r *runner) ask(prompt, flag, def string) (string, error) {
	if r.o.NonInteractive {
		if def != "" {
			return def, nil
		}
		return "", fmt.Errorf("%w: %s is required with --non-interactive", ErrUsage, flag)
	}
	if def != "" {
		fmt.Fprintf(r.Out, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(r.Out, "%s: ", prompt)
	}
	line, err := r.in.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil && line == "" {
		if def != "" {
			return def, nil
		}
		return "", fmt.Errorf("%w: no answer for %s", ErrUsage, flag)
	}
	if line == "" {
		return def, nil
	}
	return line, nil
}

// --- 1. preflight ---

func (r *runner) preflight(ctx context.Context) error {
	if r.o.SkipPrefl {
		return nil
	}
	d := doctor.Doctor{Cfg: r.Cfg, P: r.Probes}
	for _, res := range d.Run(ctx)[:1] { // platform
		if res.Mark == doctor.Fail {
			return fmt.Errorf("%s: %s", res.Detail, res.Fix)
		}
	}
	if err := r.agtermUp(ctx); err != nil {
		return err
	}
	return r.claudeReady(ctx)
}

func (r *runner) agtermUp(ctx context.Context) error {
	p, err := agterm.FindAgtermctl()
	if err != nil {
		return fmt.Errorf("agterm is not installed: brew install --cask umputun/apps/agterm")
	}
	if _, err := r.Probes.Run(ctx, p, "version"); err == nil {
		return nil
	}
	r.say("starting agterm …")
	r.Probes.Run(ctx, "/usr/bin/open", "-a", "agterm")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := r.Probes.Run(ctx, p, "version"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("agterm did not come up within 60 s: open -a agterm, then rerun hommy setup")
}

// claudeBin resolves claude to an absolute path.
func (r *runner) claudeBin() (string, error) {
	cand := r.o.ClaudeBin
	if cand == "" {
		if v, ok := r.Cfg.FileValue("CLAUDE_BIN"); ok && v != "" {
			cand = v
		}
	}
	if cand == "" {
		cand = "claude"
	}
	p, err := exec.LookPath(config.ExpandHome(cand, r.Cfg.Inst.Home))
	if err != nil {
		for _, c := range []string{filepath.Join(r.Cfg.Inst.Home, ".local/bin/claude"), "/opt/homebrew/bin/claude"} {
			if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
				p = c
				break
			}
		}
		if p == "" {
			return "", fmt.Errorf("claude not found: curl -fsSL https://claude.ai/install.sh | bash (or --claude-bin PATH)")
		}
	}
	return filepath.Abs(p)
}

func (r *runner) claudeReady(ctx context.Context) error {
	bin, err := r.claudeBin()
	if err != nil {
		return err
	}
	if err := r.Cfg.Set("CLAUDE_BIN", bin); err != nil {
		return err
	}
	signedIn := func() bool {
		_, err := r.Probes.Run(ctx, bin, "auth", "status")
		return err == nil && doctor.Onboarded(r.Probes.ReadFile, r.claudeJSON())
	}
	if signedIn() {
		return nil
	}
	if r.o.NonInteractive {
		return fmt.Errorf("claude is not signed in (or its first-run screens are not done): run claude once, then rerun hommy setup")
	}
	r.say("Claude Code is not signed in yet: an agterm session opens with claude; sign in there and finish its first screens.")
	ctl, err := agterm.NewExecCtl()
	if err != nil {
		return err
	}
	cmd := agterm.AgentCommandLine(bin, r.Cfg.Inst.Home, os.Getenv("CLAUDE_CONFIG_DIR"), false)
	if _, err := ctl.NewSession(ctx, agterm.NewSessionOpts{Workspace: r.Cfg.Get("AGTERM_WORKSPACE"), Cwd: r.Cfg.Inst.Home,
		Name: "claude-sign-in", Command: cmd}); err != nil {
		return fmt.Errorf("open the sign-in session: %w", err)
	}
	r.say("waiting for the sign-in …")
	for !signedIn() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	r.say("✓ claude signed in")
	return nil
}

func (r *runner) claudeJSON() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	return filepath.Join(r.Cfg.Inst.Home, ".claude.json")
}

// --- 2. instance dirs and config ---

func (r *runner) configStep(context.Context) error {
	c := r.Cfg
	for _, d := range []string{c.Inst.ConfigDir(), c.Inst.StateDir(), c.Inst.ShareDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	set := map[string]string{"PORT_BASE": r.o.PortBase, "HOST_NAME": r.o.HostName, "TELEGRAM_API_URL": r.o.TelegramAPI}
	if r.o.Join != "" {
		set["HUB_URL"] = r.o.Join
		set["HOST_NAME"] = r.o.Name
	}
	if _, ok := c.FileValue("HOST_NAME"); !ok && set["HOST_NAME"] == "" {
		// kept, so a renamed Mac keeps the name its host key is stored under
		set["HOST_NAME"] = c.Get("HOST_NAME")
	}
	for _, k := range []string{"PORT_BASE", "HUB_URL", "HOST_NAME", "TELEGRAM_API_URL"} {
		if v := set[k]; v != "" {
			if err := c.Set(k, v); err != nil {
				return err
			}
		}
	}
	if !c.Exists() {
		if err := c.Save(); err != nil {
			return err
		}
	}
	if err := os.Chmod(c.Path, 0o600); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	r.say("✓ config %s (instance %s, host %s)", c.Path, instName(c), c.Get("HOST_NAME"))
	return nil
}

func instName(c *config.Config) string {
	if c.Inst.Name == "" {
		return "default"
	}
	return c.Inst.Name
}

// --- 3. bot token ---

func (r *runner) token(ctx context.Context) error {
	api := r.Cfg.Get("TELEGRAM_API_URL")
	if !r.o.TokenStdin {
		if tok, err := r.Store.Get(secret.TelegramToken); err == nil {
			name, unauth, err := doctor.GetMe(ctx, r.Probes.HTTPGet, api, tok)
			switch {
			case err == nil:
				r.botName = name
				r.say("✓ bot @%s (token kept)", name)
				return nil
			case !unauth:
				r.say("! token kept: Telegram did not answer (%v)", err)
				return nil
			}
			r.say("the stored bot token is not accepted any more")
		}
	}
	var tok string
	switch {
	case r.o.TokenStdin:
		line, err := r.in.ReadString('\n')
		tok = strings.TrimSpace(line)
		if tok == "" && err != nil {
			return fmt.Errorf("%w: --token-stdin got no token", ErrUsage)
		}
	case r.o.NonInteractive:
		return fmt.Errorf("%w: no bot token: pass it with --token-stdin", ErrUsage)
	default:
		r.say("Make a bot: in Telegram, message @BotFather, send /newbot, and paste the token it gives you.")
		var err error
		if tok, err = r.ask("bot token", "--token-stdin", ""); err != nil {
			return err
		}
	}
	name, unauth, err := doctor.GetMe(ctx, r.Probes.HTTPGet, api, tok)
	if err != nil {
		if unauth {
			return fmt.Errorf("token rejected by Telegram: make a new one with @BotFather")
		}
		return err
	}
	if err := r.Store.Set(secret.TelegramToken, tok); err != nil {
		return err
	}
	r.secretsChanged = true
	r.botName = name
	r.say("✓ bot @%s", name)
	return nil
}

// --- 4. vault ---

func (r *runner) vault(context.Context) error {
	c := r.Cfg
	def := c.Get("VAULT_DIR")
	path := r.o.Vault
	if path == "" {
		var err error
		if path, err = r.ask("vault folder (your notes and recipes)", "--vault", def); err != nil {
			return err
		}
	}
	path = config.ExpandHome(path, c.Inst.Home)
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
		return fmt.Errorf("%s is a file, not a folder", abs)
	}
	created := false
	if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return err
		}
		created = true
	}
	if err := c.Set("VAULT_DIR", abs); err != nil {
		return err
	}
	if created {
		r.say("✓ vault %s (created)", abs)
	} else {
		r.say("✓ vault %s (kept)", abs)
	}
	if note := TCCNote(abs, c.Inst.Home, c.Inst.BinPath()); note != "" {
		r.say("%s", note)
	}
	return nil
}

// TCCNote is the macOS privacy note for a vault in a protected folder ("" otherwise).
func TCCNote(path, home, bin string) string {
	for _, p := range []string{"Documents", "Desktop", "Downloads", "Library/Mobile Documents"} {
		dir := filepath.Join(home, p)
		if path == dir || strings.HasPrefix(path, dir+"/") {
			return "note: " + path + " is in a folder macOS protects. Give hommy access once: System Settings › " +
				"Privacy & Security › Full Disk Access › add " + bin + " (or choose a vault outside ~/" + p + ")."
		}
	}
	return ""
}

// --- 5. permissions ---

func (r *runner) permissions(context.Context) error {
	c := r.Cfg
	mode := r.o.Permissions
	if mode == "" {
		mode = c.Get("AGENT_PERMISSIONS")
	}
	if err := c.Set("AGENT_PERMISSIONS", mode); err != nil {
		return err
	}
	if mode == "skip" {
		dir := os.Getenv("CLAUDE_CONFIG_DIR")
		if dir == "" {
			dir = filepath.Join(c.Inst.Home, ".claude")
		}
		changed, err := MergeSkipPrompt(filepath.Join(dir, "settings.json"), time.Now())
		if err != nil {
			return err
		}
		if changed {
			r.say("✓ permissions skip (Claude's skip-permissions prompt turned off in %s)", filepath.Join(dir, "settings.json"))
			return nil
		}
	}
	r.say("✓ permissions %s", mode)
	return nil
}

// MergeSkipPrompt sets skipDangerousModePermissionPrompt: true in a Claude
// settings file, keeping every other key; a backup goes beside it when it changes.
func MergeSkipPrompt(path string, now time.Time) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	doc := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return false, fmt.Errorf("%s is not valid JSON: %w", path, err)
		}
	}
	if string(doc["skipDangerousModePermissionPrompt"]) == "true" {
		return false, nil
	}
	if len(b) > 0 {
		if err := os.WriteFile(path+".bak-"+now.Format("20060102-150405"), b, 0o600); err != nil {
			return false, err
		}
	}
	doc["skipDangerousModePermissionPrompt"] = json.RawMessage("true")
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// --- 6. speech (URLs only in 02) ---

func (r *runner) speech(context.Context) error {
	c := r.Cfg
	mode := r.o.Speech
	if mode == "" {
		mode = c.Get("SPEECH")
	}
	prev := c.Get("SPEECH")
	if err := c.Set("SPEECH", mode); err != nil {
		return err
	}
	// a value left by another mode (off, or external URLs) would override this one
	stale := func(k string) bool {
		v, ok := c.FileValue(k)
		return ok && (prev != mode || strings.EqualFold(v, "off"))
	}
	switch mode {
	case "off":
		if err := c.Set("TTS_URL", "off"); err != nil {
			return err
		}
		if err := c.Set("WHISPER_ENDPOINTS", "off"); err != nil {
			return err
		}
	case "external":
		if r.o.STTURL != "" {
			if err := c.Set("WHISPER_ENDPOINTS", "external|"+r.o.STTURL+"|whisper"); err != nil {
				return err
			}
		}
		if r.o.TTSURL != "" {
			if err := c.Set("TTS_URL", r.o.TTSURL); err != nil {
				return err
			}
		}
		for _, u := range []struct{ key, flag, prompt, prefix, suffix string }{
			{"WHISPER_ENDPOINTS", "--stt-url", "speech-to-text base URL", "external|", "|whisper"},
			{"TTS_URL", "--tts-url", "text-to-speech base URL", "", ""},
		} {
			if v, ok := c.FileValue(u.key); ok && !strings.EqualFold(v, "off") {
				continue
			}
			if r.o.NonInteractive {
				return fmt.Errorf("%w: --speech external needs --stt-url and --tts-url", ErrUsage)
			}
			v, err := r.ask(u.prompt, u.flag, "")
			if err != nil {
				return err
			}
			if err := c.Set(u.key, u.prefix+v+u.suffix); err != nil {
				return err
			}
		}
		if stale("TTS_LANG_BACKENDS") {
			if err := c.Unset("TTS_LANG_BACKENDS"); err != nil {
				return err
			}
		}
	case "local":
		for _, k := range []string{"WHISPER_ENDPOINTS", "TTS_URL", "TTS_LANG_BACKENDS"} {
			if stale(k) {
				if err := c.Unset(k); err != nil {
					return err
				}
			}
		}
		if r.o.WhisperModel != "" {
			if err := c.Set("WHISPER_MODEL", r.o.WhisperModel); err != nil {
				return err
			}
		}
	}
	r.say("✓ speech %s", mode)
	return nil
}

// --- 7. keys ---

func (r *runner) keys(context.Context) error {
	st := r.Store
	host := r.Cfg.Get("HOST_NAME")
	if r.o.Join != "" {
		line, err := r.in.ReadString('\n')
		key := strings.TrimSpace(line)
		if len(key) < 32 {
			if err == nil {
				err = errors.New("shorter than 32 chars")
			}
			return fmt.Errorf("%w: --key-stdin: %v", ErrUsage, err)
		}
		if old, err := st.Get(secret.HostKey); err == nil && old == key {
			r.say("✓ host key for %s kept", host)
			return nil
		}
		if err := st.Set(secret.HostKey, key); err != nil {
			return err
		}
		r.secretsChanged = true
		r.say("✓ host key for %s stored", host)
		return nil
	}
	key, err := st.Get(secret.HostKey)
	if errors.Is(err, secret.ErrNotFound) || (err == nil && len(key) < 32) {
		if key, err = secret.NewKey(); err != nil {
			return err
		}
		if err := st.Set(secret.HostKey, key); err != nil {
			return err
		}
		r.secretsChanged = true
	} else if err != nil {
		return err
	}
	cur, err := st.Get(secret.HubHostKeys)
	if err != nil && !errors.Is(err, secret.ErrNotFound) {
		return err
	}
	keys := secret.HostKeys(cur)
	if keys[host] != key {
		keys[host] = key
		if err := st.Set(secret.HubHostKeys, secret.FormatHostKeys(keys)); err != nil {
			return err
		}
		r.secretsChanged = true
	}
	r.say("✓ keys (host %s)", host)
	return nil
}

// --- 8. services ---

func (r *runner) services(ctx context.Context) error {
	names := []string{"all"}
	if r.o.Join != "" {
		names = []string{"host"}
	}
	now, _ := os.ReadFile(r.Cfg.Path)
	if err := r.Install(r.secretsChanged || string(now) != string(r.cfgBefore), names...); err != nil {
		return err
	}
	if r.Ready != nil {
		if err := r.Ready(ctx, r.o.Join == ""); err != nil {
			r.say("services: %v", err)
		}
	}
	return nil
}

// --- 10. pairing ---

func (r *runner) pairing(ctx context.Context) error {
	if r.o.NoPairWait || r.Pair == nil {
		return nil
	}
	return r.Pair(ctx)
}

// --- 12. doctor + summary ---

func (r *runner) finish(ctx context.Context) error {
	var res []doctor.Result
	if r.Doctor != nil {
		res = r.Doctor(ctx)
	}
	r.say("")
	doctor.Print(r.Out, res)
	red := false
	paired := false
	for _, x := range res {
		if x.ID == "paired" {
			paired = x.Mark == doctor.OK
			continue
		}
		if x.Mark == doctor.Fail {
			red = true
		}
	}
	c := r.Cfg
	owner := "not paired"
	if paired {
		owner = "paired"
	}
	svcs := "hub, host"
	if r.o.Join != "" {
		svcs = "host"
		r.say("\nhost %s joined %s · vault %s · services %s", c.Get("HOST_NAME"), c.Get("HUB_URL"), c.Get("VAULT_DIR"), svcs)
	} else {
		if c.Get("SPEECH") == "local" {
			svcs += ", speech"
		}
		bot := "@" + r.botName
		if r.botName == "" {
			bot = "(not checked)"
		}
		r.say("\nbot %s · vault %s · owner %s · services %s", bot, c.Get("VAULT_DIR"), owner, svcs)
		if paired {
			r.say("Send your bot a text or a voice note, e.g. \"what can you do?\"")
		} else {
			r.say("Next: hommy pair (then send your bot a text or a voice note)")
		}
	}
	if red {
		return errors.New("doctor found problems (see the fix lines above)")
	}
	return nil
}
