package hub

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/envfile"
	"github.com/AlexDobrushskiy/hommy/internal/secret"
	"github.com/AlexDobrushskiy/hommy/internal/speech"
)

// Config is the hub's setup: settings from the env (config.Export puts the
// instance's resolved values there), secrets from the Keychain.
type Config struct {
	TelegramToken  string
	TelegramAPI    string // TELEGRAM_API_URL: the Bot API base (tests: the fake)
	Listen         string // HUB_LISTEN
	DBPath         string
	HostKeys       map[string]string // host name → key
	DefaultTarget  string
	AllowedUpdates []string
	PairingTTL     time.Duration // PAIRING_TTL: how long a pairing code is valid
	LogUpdates     bool          // log every update's JSON (dev: lets probes read what Telegram sends)
	SilentAfter    time.Duration // host-silent alert
	QueuedNotice   time.Duration // "<host> is offline; queued" text
	DraftEvery     time.Duration // DRAFT_EVERY: min gap between sendMessageDraft calls of one stream
	DraftKeepalive time.Duration // DRAFT_KEEPALIVE: re-send an unchanged draft so the client keeps showing it
	MiniAppListen  string        // MINIAPP_LISTEN: the Mini App + its API only ("" = off)
	MiniAppURL     string        // MINIAPP_URL: its public https:// base ("" = 🎯 is plain text)
	NewLinkWait    time.Duration // NEW_LINK_WAIT: how long the 🆕 line waits for the new session's claude.ai link (hosts with links only)
	Transcribe     speech.TranscribeConfig
	TTS            speech.TTSConfig
}

// Secrets is where the hub's secrets come from (the instance's Keychain
// service; a map in tests).
type Secrets interface {
	Get(item string) (string, error)
}

// LoadConfig reads the hub's settings from the env and its secrets from sec.
func LoadConfig(sec Secrets) (Config, error) {
	cfg := Config{
		TelegramAPI:   strings.TrimSuffix(envfile.Get("TELEGRAM_API_URL", "https://api.telegram.org"), "/"),
		Listen:        envfile.Get("HUB_LISTEN", "127.0.0.1:8470"),
		DBPath:        envfile.Get("HUB_DB", ""),
		DefaultTarget: os.Getenv("DEFAULT_TARGET"),
		AllowedUpdates: strings.Split(envfile.Get("ALLOWED_UPDATES",
			"message,edited_message,message_reaction,callback_query,stopped_message_generation"), ","),
		Transcribe: speech.TranscribeConfig{
			Endpoints:   speech.ParseWhisperEndpoints(envfile.Get("WHISPER_ENDPOINTS", speech.DefaultWhisperEndpoints)),
			GeminiModel: envfile.Get("GEMINI_MODEL", "gemini-2.5-flash"),
		},
		TTS: speech.TTSConfig{
			URL:          strings.TrimSuffix(envfile.Get("TTS_URL", speech.DefaultTTSURL), "/"),
			Model:        envfile.Get("TTS_MODEL", speech.DefaultTTSModel),
			DefaultVoice: envfile.Get("TTS_DEFAULT_VOICE", "af_heart"),
			LangBackends: speech.ParseTTSLangBackends(envfile.Get("TTS_LANG_BACKENDS", speech.DefaultTTSLangBackends)),
		},
	}
	if cfg.DBPath == "" {
		return cfg, fmt.Errorf("HUB_DB is required")
	}
	cfg.LogUpdates = os.Getenv("LOG_UPDATES") == "1"
	cfg.MiniAppListen = os.Getenv("MINIAPP_LISTEN")
	cfg.MiniAppURL = strings.TrimSuffix(os.Getenv("MINIAPP_URL"), "/")
	if cfg.MiniAppURL != "" && !strings.HasPrefix(cfg.MiniAppURL, "https://") {
		return cfg, fmt.Errorf("MINIAPP_URL must be https:// (Telegram requires it)")
	}
	if strings.EqualFold(cfg.TTS.URL, "off") {
		cfg.TTS.URL = ""
	}
	if cfg.DefaultTarget == "" {
		return cfg, fmt.Errorf("DEFAULT_TARGET is required")
	}
	if _, err := ParseTarget(cfg.DefaultTarget); err != nil {
		return cfg, fmt.Errorf("DEFAULT_TARGET: %w", err)
	}
	for _, d := range []struct {
		dst      *time.Duration
		key, def string
	}{{&cfg.SilentAfter, "HOST_SILENT_AFTER", "2m"}, {&cfg.QueuedNotice, "QUEUED_NOTICE_AFTER", "2m"},
		{&cfg.TTS.Timeout, "TTS_TIMEOUT", "600s"}, {&cfg.DraftEvery, "DRAFT_EVERY", "1100ms"},
		{&cfg.DraftKeepalive, "DRAFT_KEEPALIVE", "4s"}, {&cfg.NewLinkWait, "NEW_LINK_WAIT", "15s"},
		{&cfg.PairingTTL, "PAIRING_TTL", "30m"}} {
		v, err := time.ParseDuration(envfile.Get(d.key, d.def))
		if err != nil {
			return cfg, fmt.Errorf("invalid %s: %w", d.key, err)
		}
		*d.dst = v
	}
	mc, err := strconv.Atoi(envfile.Get("TTS_MIN_CHARS", "400"))
	if err != nil {
		return cfg, fmt.Errorf("invalid TTS_MIN_CHARS: %w", err)
	}
	cfg.TTS.MinChars = mc

	tok, err := sec.Get(secret.TelegramToken)
	if err != nil {
		return cfg, fmt.Errorf("keychain %s: %w (hommy setup stores it)", secret.TelegramToken, err)
	}
	if cfg.TelegramToken = strings.TrimSpace(tok); cfg.TelegramToken == "" {
		return cfg, fmt.Errorf("keychain %s is empty", secret.TelegramToken)
	}
	keys, err := sec.Get(secret.HubHostKeys)
	if err != nil {
		return cfg, fmt.Errorf("keychain %s: %w (hommy setup stores it)", secret.HubHostKeys, err)
	}
	if cfg.HostKeys, err = parseHostKeys(keys); err != nil {
		return cfg, err
	}
	// WHISPER_ENDPOINTS=off: voice in is off (no fallback either).
	if strings.EqualFold(strings.TrimSpace(os.Getenv("WHISPER_ENDPOINTS")), "off") {
		cfg.Transcribe.Endpoints = nil
		return cfg, nil
	}
	// Optional: absent means no Gemini fallback (and no call to it).
	switch g, err := sec.Get(secret.GeminiKey); {
	case err == nil:
		cfg.Transcribe.GoogleAPIKey = strings.TrimSpace(g)
	case !errors.Is(err, secret.ErrNotFound):
		log.Printf("keychain %s: %v; transcription runs without it", secret.GeminiKey, err)
	}
	return cfg, nil
}

// parseHostKeys checks the hub-host-keys item (`name key` entries).
func parseHostKeys(v string) (map[string]string, error) {
	keys := secret.HostKeys(v)
	for name, key := range keys {
		if !hostRe.MatchString(name) || len(key) < 32 {
			return nil, fmt.Errorf("keychain %s: bad entry for %q (want `name key`, key >= 32 chars)", secret.HubHostKeys, name)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("keychain %s: no hosts", secret.HubHostKeys)
	}
	return keys, nil
}

// checkKey compares in constant time.
func (c Config) checkKey(host, key string) bool {
	want, ok := c.HostKeys[host]
	if !ok {
		want = strings.Repeat("x", 64) // same work for unknown hosts
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(key)) == 1 && ok
}
