package config

import (
	"path/filepath"
	"strings"
)

// Key is one setting: its default (derived from the instance and other keys),
// its validation and the services a change restarts.
type Key struct {
	Name     string
	Default  func(c *Config) string
	Check    func(c *Config, v string) error
	Services []string // hub, host, speech
	Path     bool     // a path: ~ is expanded on export
	Doc      string
}

func lit(v string) func(*Config) string { return func(*Config) string { return v } }

const (
	hub    = "hub"
	host   = "host"
	speech = "speech"
)

// DefaultAllowedUpdates is the update types the hub asks Telegram for.
const DefaultAllowedUpdates = "message,edited_message,message_reaction,callback_query,stopped_message_generation"

// DefaultTTSLangs are the non-English Kokoro voices of the default install.
var DefaultTTSLangs = [][2]string{{"es", "ef_dora"}, {"fr", "ff_siwis"}, {"it", "if_sara"}, {"pt", "pf_dora"}, {"hi", "hf_alpha"}}

// Keys is the settings table (master spec §3.2).
var Keys = []Key{
	{Name: "PORT_BASE", Default: lit("8470"), Check: checkInt(1024, 65000), Services: []string{hub, host, speech},
		Doc: "derives the ports below when they are unset"},
	{Name: "HUB_LISTEN", Default: func(c *Config) string { return "127.0.0.1:" + c.Port(0) }, Check: checkListen,
		Services: []string{hub}},
	{Name: "HUB_URL", Default: func(c *Config) string { return "ws://127.0.0.1:" + c.Port(0) + "/ws" },
		Check: checkURL("ws", "wss"), Services: []string{host}},
	{Name: "HUB_DB", Default: func(c *Config) string { return filepath.Join(c.Inst.StateDir(), "hub", "hub.sqlite3") },
		Check: checkAbsPath, Services: []string{hub}, Path: true},
	{Name: "TELEGRAM_API_URL", Default: lit("https://api.telegram.org"), Check: checkURL("http", "https"),
		Services: []string{hub}},
	{Name: "ALLOWED_UPDATES", Default: lit(DefaultAllowedUpdates), Services: []string{hub}},
	{Name: "DEFAULT_TARGET", Default: func(c *Config) string {
		return "new@" + c.Get("HOST_NAME") + ":" + c.Get("VAULT_DIR") + ":interactive"
	}, Check: checkTarget, Services: []string{hub}},
	{Name: "PAIRING_TTL", Default: lit("30m"), Check: checkDuration, Services: []string{hub}},
	{Name: "OWNER_CHAT_ID", Default: lit(""), Check: checkChatID},
	{Name: "HOST_SILENT_AFTER", Default: lit("2m"), Check: checkDuration, Services: []string{hub}},
	{Name: "QUEUED_NOTICE_AFTER", Default: lit("2m"), Check: checkDuration, Services: []string{hub}},
	{Name: "DRAFT_EVERY", Default: lit("1100ms"), Check: checkDuration, Services: []string{hub}},
	{Name: "DRAFT_KEEPALIVE", Default: lit("4s"), Check: checkDuration, Services: []string{hub}},
	{Name: "NEW_LINK_WAIT", Default: lit("15s"), Check: checkDuration, Services: []string{hub}},
	{Name: "MINIAPP_LISTEN", Default: lit(""), Check: checkOptionalListen, Services: []string{hub}},
	{Name: "MINIAPP_URL", Default: lit(""), Check: checkMiniAppURL, Services: []string{hub}},
	{Name: "LOG_UPDATES", Default: lit("0"), Check: checkOneOf("0", "1"), Services: []string{hub}},
	{Name: "WHISPER_ENDPOINTS", Default: func(c *Config) string {
		return "local|http://127.0.0.1:" + c.Port(4) + "/upstream/whisper/v1|whisper"
	}, Services: []string{hub}},
	{Name: "GEMINI_MODEL", Default: lit("gemini-2.5-flash"), Services: []string{hub}},
	{Name: "TTS_URL", Default: func(c *Config) string { return "http://127.0.0.1:" + c.Port(4) + "/upstream/kokoro/v1" },
		Check: checkTTSURL, Services: []string{hub}},
	{Name: "TTS_MODEL", Default: lit("mlx-community/Kokoro-82M-bf16"), Services: []string{hub}},
	{Name: "TTS_DEFAULT_VOICE", Default: lit("af_heart"), Services: []string{hub}},
	{Name: "TTS_LANG_BACKENDS", Default: func(c *Config) string {
		url, model := c.Get("TTS_URL"), c.Get("TTS_MODEL")
		if strings.EqualFold(url, "off") {
			return ""
		}
		var parts []string
		for _, l := range DefaultTTSLangs {
			parts = append(parts, l[0]+"|"+url+"|"+model+"|"+l[1])
		}
		return strings.Join(parts, ",")
	}, Services: []string{hub}},
	{Name: "TTS_MIN_CHARS", Default: lit("400"), Check: checkInt(0, 1<<20), Services: []string{hub}},
	{Name: "TTS_TIMEOUT", Default: lit("600s"), Check: checkDuration, Services: []string{hub}},
	{Name: "HOST_NAME", Default: func(c *Config) string { return c.HostNameFn() }, Check: checkHostName,
		Services: []string{hub, host}},
	{Name: "HOST_DATA_DIR", Default: func(c *Config) string { return filepath.Join(c.Inst.StateDir(), "host") },
		Check: checkAbsPath, Services: []string{host}, Path: true},
	{Name: "MEDIA_DIR", Default: func(c *Config) string { return filepath.Join(c.GetPath("HOST_DATA_DIR"), "media") },
		Check: checkAbsPath, Services: []string{host}, Path: true},
	{Name: "SPOOL_DIR", Default: func(c *Config) string { return filepath.Join(c.Inst.StateDir(), "spool") },
		Check: checkAbsPath, Services: []string{host}, Path: true},
	{Name: "HOOK_SOCKET", Default: func(c *Config) string { return filepath.Join(c.GetPath("HOST_DATA_DIR"), "hook.sock") },
		Check: checkAbsPath, Services: []string{host}, Path: true},
	{Name: "VAULT_DIR", Default: func(c *Config) string { return filepath.Join(c.Inst.Home, "hommy-vault") },
		Check: checkAbsPath, Services: []string{hub, host}, Path: true},
	{Name: "AGENT_BRIEFS_DIR", Default: func(c *Config) string { return filepath.Join(c.Inst.ShareDir(), "briefs") },
		Check: checkAbsPath, Services: []string{host}, Path: true},
	{Name: "NEW_SESSION_ROOTS", Default: lit("~/Projects,~/src"), Services: []string{host}},
	{Name: "NEW_SESSION_PINNED", Default: func(c *Config) string { return c.Get("VAULT_DIR") }, Services: []string{host}},
	{Name: "CLAUDE_BIN", Default: lit("claude"), Services: []string{host}},
	{Name: "AGENT_PERMISSIONS", Default: lit("skip"), Check: checkOneOf("skip", "ask"), Services: []string{host}},
	{Name: "AGENT_COMMAND", Default: func(c *Config) string { return c.Get("CLAUDE_BIN") }, Services: []string{host}},
	{Name: "AGTERM_WORKSPACE", Default: lit("hommy"), Services: []string{host}},
	{Name: "CLAUDE_TIMEOUT", Default: lit("30m"), Check: checkDuration, Services: []string{host}},
	{Name: "HOST_PROGRESS_EVERY", Default: lit("500ms"), Check: checkDuration, Services: []string{host}},
	{Name: "CLAUDE_KILL_GRACE", Default: lit("5s"), Check: checkDuration, Services: []string{host}},
	{Name: "SPEECH", Default: lit("local"), Check: checkOneOf("local", "external", "off"), Services: []string{speech}},
	{Name: "SPEECH_LISTEN", Default: func(c *Config) string { return "127.0.0.1:" + c.Port(4) }, Check: checkListen,
		Services: []string{speech}},
	{Name: "WHISPER_MODEL", Default: lit("large-v3-turbo"), Check: checkOneOf("large-v3-turbo", "small", "medium"),
		Services: []string{speech}},
	{Name: "HB_HOME", Default: func(c *Config) string {
		if c.Inst.Name == "" {
			return filepath.Join(c.Inst.Home, ".local", "share", "hommy-browsers")
		}
		return filepath.Join(c.Inst.Home, ".local", "share", c.Inst.Base()+"-browsers")
	}, Check: checkOptionalAbsPath, Path: true},
}

var keyIndex map[string]Key

func init() {
	keyIndex = map[string]Key{}
	for _, k := range Keys {
		keyIndex[k.Name] = k
	}
}

// Known reports whether key is a setting.
func Known(key string) bool { _, ok := keyIndex[key]; return ok }
