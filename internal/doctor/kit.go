package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/browsers"
	"github.com/AlexDobrushskiy/hommy/internal/speech/stack"
)

// The kit's checks (phase 05): the local speech stack, the plugins, the browsers
// engine and the optional tools.

// KitProbes are the outside world of the kit checks (faked in tests).
type KitProbes struct {
	// MCPInit sends MCP initialize to the lanes server on 127.0.0.1:port.
	MCPInit func(ctx context.Context, port int) error
	// ModelOK reports whether path holds the pinned model.
	ModelOK func(path string, m stack.Model) (bool, error)
	// Transcribe/Synthesize are the --deep round trips.
	Transcribe func(ctx context.Context, baseURL string, audio []byte) (string, error)
	Synthesize func(ctx context.Context, baseURL, model, voice, lang, text string) ([]byte, error)
	// LookPath finds an optional tool.
	LookPath func(name string) (string, error)
}

// RealKitProbes talk to the Mac.
func RealKitProbes() KitProbes {
	return KitProbes{
		MCPInit: browsers.Initialize,
		ModelOK: stack.Verified,
		Transcribe: func(ctx context.Context, u string, a []byte) (string, error) {
			return stack.Transcribe(ctx, nil, u, a)
		},
		Synthesize: func(ctx context.Context, u, model, voice, lang, text string) ([]byte, error) {
			return stack.Synthesize(ctx, nil, u, model, voice, lang, text)
		},
		LookPath: func(name string) (string, error) {
			if p, err := exec.LookPath(name); err == nil {
				return p, nil
			}
			return exec.LookPath(filepath.Join("/opt/homebrew/bin", name))
		},
	}
}

// ServicePath is the PATH the speech service runs with (the service package's plist PATH).
func (d *Doctor) ServicePath() []string {
	return []string{"/opt/homebrew/bin", "/usr/bin", "/bin", filepath.Join(d.Cfg.Inst.Home, ".local", "bin")}
}

func (d *Doctor) speechStack() Result {
	c := d.Cfg
	var missing, fixes []string
	for _, t := range stack.Tools {
		if !d.executable(d.ServicePath(), t.Name) {
			missing = append(missing, t.Name)
			fixes = append(fixes, t.Fix)
		}
	}
	if len(missing) > 0 {
		return fail("speech-stack", strings.Join(missing, ", ")+" not found", strings.Join(append([]string{"install.sh (re-run)"}, fixes...), " · "))
	}
	m, err := stack.ModelFor(c.Get("WHISPER_MODEL"))
	if err != nil {
		return fail("speech-stack", err.Error(), "hommy setup --whisper-model large-v3-turbo")
	}
	p := stack.ModelPath(c.Inst.ShareDir(), m)
	ok2, err := d.K.ModelOK(p, m)
	if err != nil || !ok2 {
		return fail("speech-stack", p+" missing or not the pinned SHA-256", "hommy setup (downloads it again)")
	}
	return ok("speech-stack", "llama-swap, whisper-server, ffmpeg, mlx_audio.server; "+m.File()+" verified")
}

// executable: an executable file name is in one of dirs (stack.Find, through the Stat probe).
func (d *Doctor) executable(dirs []string, name string) bool {
	for _, dir := range dirs {
		if fi, err := d.P.Stat(filepath.Join(dir, name)); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

// speechDeep: the embedded "hello" through STT, "ok" through TTS as Ogg Opus.
func (d *Doctor) speechDeep(ctx context.Context) Result {
	c := d.Cfg
	fix := "check WHISPER_ENDPOINTS / TTS_URL"
	if c.Get("SPEECH") == "local" {
		fix = "hommy service restart speech; tail " + filepath.Join(c.Inst.LogDir("speech"), "err.log")
	}
	eps := parseWhisper(c.Get("WHISPER_ENDPOINTS"))
	tts := c.Get("TTS_URL")
	if len(eps) == 0 || strings.EqualFold(tts, "off") {
		return ok("speech-deep", "voice is off")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	txt, err := d.K.Transcribe(ctx, eps[0], stack.Sample)
	if err != nil {
		return fail("speech-deep", "transcription: "+err.Error(), fix)
	}
	if !strings.Contains(strings.ToLower(txt), "hello") {
		return fail("speech-deep", fmt.Sprintf("the sample came back as %q, not hello", txt), fix)
	}
	if _, err := d.K.Synthesize(ctx, tts, c.Get("TTS_MODEL"), c.Get("TTS_DEFAULT_VOICE"), "en", "ok"); err != nil {
		return fail("speech-deep", "synthesis: "+err.Error(), fix)
	}
	return ok("speech-deep", fmt.Sprintf("transcribed %q; synthesized Ogg Opus", txt))
}

type pluginRow struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	Version string `json:"version"`
}

// plugins lists the Claude user's plugins through CLAUDE_BIN.
func (d *Doctor) plugins(ctx context.Context) ([]pluginRow, error) {
	out, err := d.P.Run(ctx, d.Cfg.Get("CLAUDE_BIN"), "plugin", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("claude plugin list failed")
	}
	var rows []pluginRow
	if err := json.Unmarshal([]byte(jsonPart(out)), &rows); err != nil {
		return nil, fmt.Errorf("claude plugin list: not JSON")
	}
	return rows, nil
}

// jsonPart drops anything a CLI printed before its JSON.
func jsonPart(s string) string {
	if i := strings.IndexAny(s, "[{"); i > 0 {
		return s[i:]
	}
	return s
}

func findPlugin(rows []pluginRow, id string) *pluginRow {
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i]
		}
	}
	return nil
}

func (d *Doctor) plugin(ctx context.Context, rows []pluginRow, listErr error) Result {
	inst := d.Cfg.Inst.Name
	install := "claude plugin install hommy@hommy"
	if inst != "" {
		install += " --config instance=" + inst
	}
	if listErr != nil {
		return fail("plugin", listErr.Error(), install)
	}
	p := findPlugin(rows, "hommy@hommy")
	switch {
	case p == nil:
		return fail("plugin", "hommy@hommy not installed", install)
	case !p.Enabled:
		return fail("plugin", "hommy@hommy disabled (answers would not reach Telegram)", "claude plugin enable hommy@hommy")
	}
	out, err := d.P.Run(ctx, d.Cfg.Get("CLAUDE_BIN"), "plugin", "configure", "hommy@hommy", "--json")
	var v struct {
		Inputs map[string]any `json:"inputs"`
	}
	if err != nil || json.Unmarshal([]byte(jsonPart(out)), &v) != nil {
		return fail("plugin", "hommy@hommy: its instance setting cannot be read", install)
	}
	got, _ := v.Inputs["instance"].(string)
	if got != inst {
		fixVal := inst
		if fixVal == "" {
			fixVal = `""`
		}
		return fail("plugin", fmt.Sprintf("hommy@hommy sends hooks to instance %q, not %q", got, inst),
			fmt.Sprintf(`echo '{"instance":%s}' | claude plugin configure hommy@hommy --values-stdin`, jsonString(inst))+
				" (instance "+fixVal+")")
	}
	return ok("plugin", "hommy@hommy "+p.Version+" enabled (instance "+displayInst(inst)+")")
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func displayInst(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

func (d *Doctor) retainer(rows []pluginRow, listErr error) Result {
	const fix = "claude plugin marketplace add AlexDobrushskiy/retainer; claude plugin install retainer@retainer"
	if listErr != nil {
		return fail("retainer", listErr.Error(), fix)
	}
	if p := findPlugin(rows, "retainer@retainer"); p != nil {
		if !p.Enabled {
			return fail("retainer", "retainer@retainer disabled", "claude plugin enable retainer@retainer")
		}
		return ok("retainer", "retainer@retainer "+p.Version)
	}
	return fail("retainer", "retainer@retainer not installed", fix)
}

func (d *Doctor) browsersCheck(ctx context.Context) Result {
	c := d.Cfg
	chrome := false
	for _, p := range browsers.ChromeCandidates(c.Inst.Home) {
		if _, err := d.P.Stat(p); err == nil {
			chrome = true
			break
		}
	}
	if !chrome {
		return fail("browsers", "Google Chrome not found", "brew install --cask google-chrome")
	}
	hb := browsers.Home(c)
	if _, err := d.P.Stat(filepath.Join(hb, "install.sh")); err != nil {
		return fail("browsers", hb+" missing", "install.sh (re-run), or hommy setup")
	}
	env, _ := d.P.ReadFile(filepath.Join(hb, "config.env"))
	port := browsers.LanesPort(c, browsers.ParseEnv(env))
	if err := d.K.MCPInit(ctx, port); err != nil {
		return fail("browsers", err.Error(), hb+"/install.sh")
	}
	return ok("browsers", fmt.Sprintf("Chrome, %s, lanes MCP :%d", hb, port))
}

func (d *Doctor) optionalTool(id, tool, fix string) Result {
	if p, err := d.K.LookPath(tool); err == nil {
		return ok(id, p)
	}
	return Result{ID: id, Mark: Optional, Detail: tool + " not installed (optional: " + fix + ")"}
}

// whatsapp: wacli is optional; without shfmt the WhatsApp guard can't parse commands, so it asks before every wacli one.
func (d *Doctor) whatsapp() Result {
	r := d.optionalTool("whatsapp", "wacli", "install.sh --with-whatsapp")
	if _, err := d.K.LookPath("shfmt"); r.Mark == OK && err != nil {
		r.Mark = Optional
		r.Detail += "; shfmt not installed, so the WhatsApp guard asks before every wacli command (optional: brew install shfmt)"
	}
	return r
}

func parseWhisper(raw string) []string {
	var out []string
	for _, e := range strings.Split(raw, ",") {
		f := strings.Split(e, "|")
		if len(f) == 3 && strings.TrimSpace(f[1]) != "" {
			out = append(out, strings.TrimSpace(f[1]))
		}
	}
	return out
}
