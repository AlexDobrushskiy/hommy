// Package doctor is `hommy doctor`: one line per check, `✓ <id> <detail>` or
// `✗ <id> <detail>` followed by `  fix: <what to do>`; `!` marks an optional
// piece that is absent. Exit 0 when nothing is red.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/pairing"
	"github.com/AlexDobrushskiy/hommy/internal/secret"
	"github.com/AlexDobrushskiy/hommy/internal/speech"
	"github.com/AlexDobrushskiy/hommy/internal/version"
)

// Mark is a check's outcome.
type Mark string

const (
	OK       Mark = "✓"
	Fail     Mark = "✗"
	Optional Mark = "!"
)

// Result is one check's line.
type Result struct {
	ID     string
	Mark   Mark
	Detail string
	Fix    string
}

// Probes are the outside world (faked in tests).
type Probes struct {
	Run       func(ctx context.Context, name string, args ...string) (string, error) // a command's combined output
	HTTPGet   func(ctx context.Context, url string, hdr map[string]string) (int, []byte, error)
	Dial      func(network, addr string) error
	Secret    func(item string) (string, error)
	SecretHas func(item string) bool
	Stat      func(path string) (os.FileInfo, error)
	ReadFile  func(path string) ([]byte, error)
	Service   func(name string) (loaded, running bool) // launchd state of hub|host|speech
	Installed func(name string) bool                   // the plist exists
	// Pairing and APIStatus ask the hub (GET /api/pairing, /api/status) with this Mac's host key.
	Pairing   func(ctx context.Context) (pairing.State, error)
	APIStatus func(ctx context.Context) (pairing.Status, error)
	GOOS      string
	GOARCH    string
	Version   string // this hommy's version
}

// Doctor runs the checks for one instance.
type Doctor struct {
	Cfg *config.Config
	P   Probes
}

// RealProbes talk to the Mac.
func RealProbes(c *config.Config, service func(string) (bool, bool), installed func(string) bool) Probes {
	st := secret.Store{Service: c.Inst.KeychainService()}
	return Probes{
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stdin = nil
			out, err := cmd.CombinedOutput()
			return string(out), err
		},
		HTTPGet: func(ctx context.Context, url string, hdr map[string]string) (int, []byte, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return 0, nil, err
			}
			for k, v := range hdr {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return 0, nil, err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			return resp.StatusCode, b, nil
		},
		Dial: func(network, addr string) error {
			c, err := net.DialTimeout(network, addr, 3*time.Second)
			if err == nil {
				c.Close()
			}
			return err
		},
		Secret:    st.Get,
		SecretHas: st.Exists,
		Pairing: func(ctx context.Context) (pairing.State, error) {
			cl, err := client(c, st.Get)
			if err != nil {
				return pairing.State{}, err
			}
			return cl.Status(ctx)
		},
		APIStatus: func(ctx context.Context) (pairing.Status, error) {
			cl, err := client(c, st.Get)
			if err != nil {
				return pairing.Status{}, err
			}
			return cl.APIStatus(ctx)
		},
		Stat:      os.Stat,
		ReadFile:  os.ReadFile,
		Service:   service,
		Installed: installed,
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
		Version:   version.String(),
	}
}

// HubRole: this Mac runs the hub (a joined host does not): the hub plist or the bot token exists.
func (d *Doctor) HubRole() bool {
	return d.P.Installed("hub") || d.P.SecretHas(secret.TelegramToken)
}

// Run runs every 02 check in order.
func (d *Doctor) Run(ctx context.Context) []Result {
	c := d.Cfg
	var out []Result
	add := func(r Result) { out = append(out, r) }
	hubRole := d.HubRole()

	add(d.platform(ctx))
	add(d.agtermCheck(ctx))
	claudeOK := d.claude(ctx, add)
	_ = claudeOK
	add(d.config())
	add(d.secrets(hubRole))
	if hubRole {
		add(d.telegram(ctx))
	}
	add(d.binary(ctx))
	add(d.services(hubRole))
	var status *pairing.Status
	_, clErr := client(c, d.P.Secret)
	if hubRole {
		add(d.hub(ctx))
		add(d.paired(ctx, clErr))
	}
	if clErr == nil {
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if s, err := d.P.APIStatus(sctx); err == nil {
			status = &s
		}
		cancel()
	}
	add(d.host(status, clErr))
	add(d.vault(status))
	add(d.hooks())
	if hubRole {
		add(d.speech(ctx))
	}
	_ = c
	return out
}

// Print writes the lines; it reports whether anything is red.
func Print(w io.Writer, rs []Result) (red bool) {
	for _, r := range rs {
		fmt.Fprintf(w, "%s %s %s\n", r.Mark, r.ID, r.Detail)
		if r.Mark == Fail {
			red = true
			if r.Fix != "" {
				fmt.Fprintf(w, "  fix: %s\n", r.Fix)
			}
		}
	}
	return red
}

func ok(id, detail string) Result        { return Result{ID: id, Mark: OK, Detail: detail} }
func fail(id, detail, fix string) Result { return Result{ID: id, Mark: Fail, Detail: detail, Fix: fix} }

func (d *Doctor) platform(ctx context.Context) Result {
	const fix = "hommy needs macOS 14+ on Apple Silicon"
	if d.P.GOOS != "darwin" || d.P.GOARCH != "arm64" {
		return fail("platform", d.P.GOOS+"/"+d.P.GOARCH, fix)
	}
	v, err := d.P.Run(ctx, "/usr/bin/sw_vers", "-productVersion")
	v = strings.TrimSpace(v)
	major, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	if err != nil || major < 14 {
		return fail("platform", "macOS "+v, fix)
	}
	return ok("platform", "macOS "+v+" arm64")
}

func (d *Doctor) agtermCheck(ctx context.Context) Result {
	const fix = "brew install --cask umputun/apps/agterm, then open -a agterm"
	p, err := agterm.FindAgtermctl()
	if err != nil {
		return fail("agterm", "agtermctl not found", fix)
	}
	out, err := d.P.Run(ctx, p, "version")
	if err != nil {
		return fail("agterm", "agterm does not answer", "open -a agterm")
	}
	v, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return ok("agterm", strings.TrimSpace(v))
}

// claudeJSON is where Claude Code keeps its onboarding state.
func (d *Doctor) claudeJSON() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	return filepath.Join(d.Cfg.Inst.Home, ".claude.json")
}

// Onboarded reports Claude Code's hasCompletedOnboarding.
func Onboarded(read func(string) ([]byte, error), path string) bool {
	b, err := read(path)
	if err != nil {
		return false
	}
	var v struct {
		Done bool `json:"hasCompletedOnboarding"`
	}
	return json.Unmarshal(b, &v) == nil && v.Done
}

func (d *Doctor) claude(ctx context.Context, add func(Result)) bool {
	bin := d.Cfg.Get("CLAUDE_BIN")
	out, err := d.P.Run(ctx, bin, "--version")
	if err != nil {
		add(fail("claude", bin+" does not run", "curl -fsSL https://claude.ai/install.sh | bash"))
		add(fail("claude-auth", "not checked (no claude)", "claude auth login, then run claude once"))
		return false
	}
	v, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	add(ok("claude", v))
	if _, err := d.P.Run(ctx, bin, "auth", "status"); err != nil {
		add(fail("claude-auth", "not signed in", "claude auth login, then run claude once"))
		return true
	}
	if !Onboarded(d.P.ReadFile, d.claudeJSON()) {
		add(fail("claude-auth", "first-run onboarding not finished", "run claude once and finish its first screens"))
		return true
	}
	add(ok("claude-auth", "signed in"))
	return true
}

func (d *Doctor) config() Result {
	c := d.Cfg
	fi, err := d.P.Stat(c.Path)
	if err != nil {
		return fail("config", c.Path+" missing", "hommy setup")
	}
	if fi.Mode().Perm() != 0o600 {
		return fail("config", fmt.Sprintf("%s mode %o (want 600)", c.Path, fi.Mode().Perm()), "chmod 600 "+c.Path)
	}
	if err := c.Validate(); err != nil {
		return fail("config", err.Error(), "hommy config set <KEY> <valid value>, or hommy setup")
	}
	detail := c.Path
	if u := c.Unknown(); len(u) > 0 {
		detail += " (unknown settings ignored: " + strings.Join(u, ", ") + ")"
	}
	return ok("config", detail)
}

func (d *Doctor) secrets(hubRole bool) Result {
	need := []string{secret.HostKey}
	if hubRole {
		need = append(need, secret.TelegramToken, secret.HubHostKeys)
	}
	var missing []string
	for _, it := range need {
		if !d.P.SecretHas(it) {
			missing = append(missing, it)
		}
	}
	if len(missing) > 0 {
		return fail("secrets", "missing in the Keychain: "+strings.Join(missing, ", "), "hommy setup")
	}
	return ok("secrets", strings.Join(need, ", "))
}

// GetMe asks the Bot API who the token belongs to; unauthorized = the token is rejected.
func GetMe(ctx context.Context, get func(ctx context.Context, url string, hdr map[string]string) (int, []byte, error),
	apiURL, token string) (username string, unauthorized bool, err error) {
	code, body, err := get(ctx, strings.TrimSuffix(apiURL, "/")+"/bot"+token+"/getMe", nil)
	if err != nil {
		return "", false, errors.New("Bot API unreachable") // the URL holds the token: never echo err
	}
	if code == 401 || code == 404 {
		return "", true, errors.New("token rejected")
	}
	var v struct {
		OK     bool `json:"ok"`
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	if code != 200 || json.Unmarshal(body, &v) != nil || !v.OK {
		return "", false, fmt.Errorf("getMe: HTTP %d", code)
	}
	return v.Result.Username, false, nil
}

func (d *Doctor) telegram(ctx context.Context) Result {
	const fix = "token rejected: make a new one with @BotFather, then hommy setup --token-stdin"
	tok, err := d.P.Secret(secret.TelegramToken)
	if err != nil {
		return fail("telegram", "no token", "hommy setup --token-stdin")
	}
	name, unauth, err := GetMe(ctx, d.P.HTTPGet, d.Cfg.Get("TELEGRAM_API_URL"), tok)
	switch {
	case unauth:
		return fail("telegram", "getMe: token rejected", fix)
	case err != nil:
		return fail("telegram", err.Error(), "check the network / TELEGRAM_API_URL")
	}
	return ok("telegram", "@"+name)
}

func (d *Doctor) binary(ctx context.Context) Result {
	const fix = "hommy service install"
	p := d.Cfg.Inst.BinPath()
	if _, err := d.P.Stat(p); err != nil {
		return fail("binary", p+" missing", fix)
	}
	out, err := d.P.Run(ctx, p, "version")
	got := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "hommy "))
	if err != nil || got != d.P.Version {
		return fail("binary", fmt.Sprintf("%s is %q, this hommy is %q", p, got, d.P.Version), fix)
	}
	req, err := d.P.Run(ctx, "/usr/bin/codesign", "-d", "-r-", p)
	if err != nil || !strings.Contains(req, "certificate leaf") || strings.Contains(strings.ToLower(req), "cdhash") ||
		!strings.Contains(req, `identifier "`+config.SigningIdentifier+`"`) {
		return fail("binary", "not signed with the stable identity (TCC grants would not survive)", fix)
	}
	return ok("binary", p+" "+got+" signed")
}

func (d *Doctor) serviceNames(hubRole bool) []string {
	var names []string
	if hubRole {
		names = append(names, "hub")
	}
	names = append(names, "host")
	if d.Cfg.Get("SPEECH") == "local" {
		names = append(names, "speech")
	}
	return names
}

func (d *Doctor) services(hubRole bool) Result {
	var down []string
	names := d.serviceNames(hubRole)
	for _, n := range names {
		loaded, running := d.P.Service(n)
		if !loaded {
			return fail("services", d.Cfg.Inst.Label(n)+" not loaded", "hommy service install")
		}
		if !running {
			down = append(down, n)
		}
	}
	if len(down) > 0 {
		return fail("services", strings.Join(down, ", ")+" not running", "hommy service restart "+strings.Join(down, " "))
	}
	return ok("services", strings.Join(names, ", ")+" running")
}

func client(c *config.Config, get func(string) (string, error)) (pairing.Client, error) {
	base, err := pairing.BaseURL(c.Get("HUB_URL"))
	if err != nil {
		return pairing.Client{}, err
	}
	key, err := get(secret.HostKey)
	if err != nil {
		return pairing.Client{}, errors.New("no host key in the Keychain")
	}
	return pairing.Client{BaseURL: base, HostName: c.Get("HOST_NAME"), Key: key}, nil
}

func (d *Doctor) hub(ctx context.Context) Result {
	fix := "hommy service restart hub; tail " + filepath.Join(d.Cfg.Inst.LogDir("hub"), "err.log")
	base, err := pairing.BaseURL(d.Cfg.Get("HUB_URL"))
	if err != nil {
		return fail("hub", err.Error(), "hommy config set HUB_URL ws://127.0.0.1:"+d.Cfg.Port(0)+"/ws")
	}
	code, _, err := d.P.HTTPGet(ctx, base+"/healthz", nil)
	if err != nil {
		return fail("hub", base+" does not answer", fix)
	}
	if code != 200 {
		return fail("hub", fmt.Sprintf("/healthz %d (database or Bot API down)", code), fix)
	}
	return ok("hub", base+" healthy")
}

func (d *Doctor) paired(ctx context.Context, clErr error) Result {
	if clErr != nil {
		return fail("paired", clErr.Error(), "hommy setup")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := d.P.Pairing(ctx)
	if err != nil {
		return fail("paired", "hub: "+err.Error(), "hommy service restart hub")
	}
	if !st.Paired {
		return fail("paired", "no owner yet", "hommy pair")
	}
	return ok("paired", "owner paired")
}

func (d *Doctor) thisHost(s *pairing.Status) *pairing.Host {
	if s == nil {
		return nil
	}
	name := d.Cfg.Get("HOST_NAME")
	for i := range s.Hosts {
		if s.Hosts[i].Name == name {
			return &s.Hosts[i]
		}
	}
	return nil
}

func (d *Doctor) host(s *pairing.Status, clErr error) Result {
	const fix = "hommy service restart host"
	name := d.Cfg.Get("HOST_NAME")
	if clErr != nil {
		return fail("host", clErr.Error(), "hommy setup")
	}
	h := d.thisHost(s)
	switch {
	case s == nil:
		return fail("host", "the hub's /api/status does not answer", fix)
	case h == nil:
		return fail("host", name+" unknown to the hub (no key in hub-host-keys?)", "hommy setup")
	case !h.Online:
		return fail("host", name+" offline", fix+"; tail "+filepath.Join(d.Cfg.Inst.LogDir("host"), "err.log"))
	case h.LastSeen != nil && time.Since(*h.LastSeen) > 30*time.Second:
		return fail("host", fmt.Sprintf("%s: last heartbeat %s ago", name, time.Since(*h.LastSeen).Round(time.Second)), fix)
	}
	return ok("host", name+" online")
}

func (d *Doctor) vault(s *pairing.Status) Result {
	dir := d.Cfg.GetPath("VAULT_DIR")
	h := d.thisHost(s)
	state := ""
	if h != nil {
		state = h.Checks["vault"]
	}
	switch state {
	case "ok":
		return ok("vault", dir)
	case "denied":
		return fail("vault", dir+": the host may not read it",
			"System Settings › Privacy & Security › Full Disk Access › add "+d.Cfg.Inst.BinPath())
	case "missing":
		return fail("vault", dir+" missing or without CLAUDE.md", "hommy setup --vault PATH")
	}
	return fail("vault", dir+": no report from the host yet", "hommy service restart host")
}

func (d *Doctor) hooks() Result {
	sock := d.Cfg.GetPath("HOOK_SOCKET")
	if err := d.P.Dial("unix", sock); err != nil {
		return fail("hooks", sock+" does not accept", "hommy service restart host")
	}
	return ok("hooks", sock)
}

// healthURL is the gateway /health of an OpenAI-style endpoint
// (http://h:p/upstream/<model>/v1 or http://h:p/v1 → http://h:p/health).
func healthURL(u string) string {
	u = strings.TrimSuffix(u, "/")
	if i := strings.Index(u, "/upstream/"); i >= 0 {
		return u[:i] + "/health"
	}
	return strings.TrimSuffix(u, "/v1") + "/health"
}

func (d *Doctor) speech(ctx context.Context) Result {
	c := d.Cfg
	fix := "check WHISPER_ENDPOINTS / TTS_URL"
	if c.Get("SPEECH") == "local" {
		fix = "hommy service restart speech"
	}
	if c.Get("SPEECH") == "off" {
		return ok("speech", "off")
	}
	var urls []string
	for _, e := range speech.ParseWhisperEndpoints(c.Get("WHISPER_ENDPOINTS")) {
		urls = append(urls, healthURL(e.URL))
	}
	if t := c.Get("TTS_URL"); !strings.EqualFold(t, "off") {
		urls = append(urls, healthURL(t))
	}
	seen := map[string]bool{}
	for _, u := range urls {
		if seen[u] {
			continue
		}
		seen[u] = true
		code, _, err := d.P.HTTPGet(ctx, u, nil)
		if err != nil || code != 200 {
			return fail("speech", u+" does not answer", fix)
		}
	}
	if len(seen) == 0 {
		return ok("speech", "off")
	}
	return ok("speech", "STT and TTS answer")
}
