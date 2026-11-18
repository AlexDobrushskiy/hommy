package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/secret"
)

type mapSecrets map[string]string

func (m mapSecrets) Get(item string) (string, error) {
	if v, ok := m[item]; ok {
		return v, nil
	}
	return "", secret.ErrNotFound
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("HUB_DB", "/tmp/hub.sqlite3")
	t.Setenv("DEFAULT_TARGET", "new@mac1:~/hommy-vault:interactive")
	t.Setenv("TELEGRAM_API_URL", "http://127.0.0.1:4711/")
	t.Setenv("PAIRING_TTL", "5m")
	sec := mapSecrets{secret.TelegramToken: "123:abc\n", secret.HubHostKeys: "mac1 " + hostKey + ",mac2 " + hostKey + "2"}
	cfg, err := LoadConfig(sec)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TelegramToken != "123:abc" || len(cfg.HostKeys) != 2 || cfg.HostKeys["mac2"] != hostKey+"2" ||
		cfg.TelegramAPI != "http://127.0.0.1:4711" || cfg.PairingTTL != 5*time.Minute || cfg.Listen != "127.0.0.1:8470" {
		t.Errorf("cfg %+v", cfg)
	}
	if cfg.Transcribe.GoogleAPIKey != "" {
		t.Error("a Gemini key from nowhere")
	}
	for _, u := range append([]string{cfg.TTS.URL}, cfg.Transcribe.Endpoints[0].URL) {
		if !strings.HasPrefix(u, "http://127.0.0.1:") {
			t.Errorf("default %q is not loopback", u)
		}
	}
	sec[secret.GeminiKey] = "g"
	if cfg, _ = LoadConfig(sec); cfg.Transcribe.GoogleAPIKey != "g" {
		t.Error("Gemini key not read")
	}

	for name, s := range map[string]mapSecrets{
		"no token":     {secret.HubHostKeys: "mac1 " + hostKey},
		"no host keys": {secret.TelegramToken: "1:a"},
		"short key":    {secret.TelegramToken: "1:a", secret.HubHostKeys: "mac1 short"},
	} {
		if _, err := LoadConfig(s); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	t.Setenv("WHISPER_ENDPOINTS", "OFF")
	if cfg, err = LoadConfig(sec); err != nil || cfg.Transcribe.Enabled() {
		t.Errorf("WHISPER_ENDPOINTS=off: voice in still on (%+v, %v)", cfg.Transcribe, err)
	}
	t.Setenv("WHISPER_ENDPOINTS", "")
	t.Setenv("DEFAULT_TARGET", "")
	if _, err := LoadConfig(sec); err == nil || !strings.Contains(err.Error(), "DEFAULT_TARGET") {
		t.Errorf("no default target: %v", err)
	}
}
