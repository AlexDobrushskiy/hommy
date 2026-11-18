// Package speech is voice in and out: the whisper → Gemini transcription chain
// and OpenAI-compatible TTS with per-language backends.
package speech

import (
	"sort"
	"strings"
	"time"
)

// TTSBackend is one language's OpenAI-compatible synthesis endpoint. Keeping
// url+model+voice together is what lets a language be served by a different
// engine (a language Kokoro lacks goes to another server).
type TTSBackend struct {
	URL   string
	Model string
	Voice string
	Lang  string // lang_code sent with every request ("" = DefaultLang)
}

// WhisperEndpoint is one entry in the ordered whisper transcription chain.
type WhisperEndpoint struct {
	Name  string
	URL   string
	Model string
}

// Defaults when the env has none (the config package normally provides the
// instance's, derived from PORT_BASE): the local speech gateway on loopback.
const (
	DefaultTTSURL           = "http://127.0.0.1:8474/upstream/kokoro/v1"
	DefaultTTSModel         = "mlx-community/Kokoro-82M-bf16"
	DefaultTTSLangBackends  = ""
	DefaultWhisperEndpoints = "local|http://127.0.0.1:8474/upstream/whisper/v1|whisper"
)

// DefaultLang is the lang_code of a <tts> block without a lang attribute.
const DefaultLang = "en"

// LangCode is the lang_code a <tts lang="…"> block's language is sent with:
// the language itself, lowercased; DefaultLang when unset.
func LangCode(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" {
		return DefaultLang
	}
	return lang
}

// ParseTTSLangBackends parses TTS_LANG_BACKENDS in the form
// "lang|url|model|voice,lang|url|model|voice". Malformed or partially empty
// entries are skipped rather than failing startup — a typo in one language
// must not take voice replies down for the others.
func ParseTTSLangBackends(s string) map[string]TTSBackend {
	out := map[string]TTSBackend{}
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		f := strings.Split(entry, "|")
		if len(f) != 4 {
			continue
		}
		lang := strings.ToLower(strings.TrimSpace(f[0]))
		url := strings.TrimSuffix(strings.TrimSpace(f[1]), "/")
		model, voice := strings.TrimSpace(f[2]), strings.TrimSpace(f[3])
		if lang == "" || url == "" || model == "" || voice == "" {
			continue
		}
		out[lang] = TTSBackend{URL: url, Model: model, Voice: voice, Lang: lang}
	}
	return out
}

// ParseWhisperEndpoints parses WHISPER_ENDPOINTS in the form
// "name|url|model,name|url|model". Malformed entries (wrong field count,
// or any field blank after trimming) are skipped.
func ParseWhisperEndpoints(raw string) []WhisperEndpoint {
	var endpoints []WhisperEndpoint
	for _, entry := range strings.Split(raw, ",") {
		parts := strings.Split(entry, "|")
		if len(parts) != 3 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		url := strings.TrimSpace(parts[1])
		model := strings.TrimSpace(parts[2])
		if name == "" || url == "" || model == "" {
			continue
		}
		endpoints = append(endpoints, WhisperEndpoint{Name: name, URL: url, Model: model})
	}
	return endpoints
}

// TranscribeConfig is the transcription chain: whisper endpoints in order,
// then Gemini only when an API key is set.
type TranscribeConfig struct {
	Endpoints    []WhisperEndpoint
	GoogleAPIKey string
	GeminiModel  string
}

// Enabled reports whether there is anything to transcribe with
// (WHISPER_ENDPOINTS=off and no Gemini key = voice in is off).
func (c TranscribeConfig) Enabled() bool { return len(c.Endpoints) > 0 || c.GoogleAPIKey != "" }

// TTSConfig is the voice-reply setup. URL "" disables voice replies.
type TTSConfig struct {
	URL          string
	Model        string
	DefaultVoice string
	MinChars     int
	Timeout      time.Duration // ceiling; see SynthesisTimeout
	LangBackends map[string]TTSBackend
}

// BackendFor picks the backend for a <tts> block's language, falling back to
// the default (English) backend. Lang is the block's language either way.
func (c TTSConfig) BackendFor(lang string) TTSBackend {
	if b, ok := c.LangBackends[strings.ToLower(lang)]; ok {
		b.Lang = LangCode(lang)
		return b
	}
	return TTSBackend{URL: c.URL, Model: c.Model, Voice: c.DefaultVoice, Lang: LangCode(lang)}
}

// ExtraLangs lists configured languages other than the default backend's,
// sorted so the prompt built from them is deterministic.
func (c TTSConfig) ExtraLangs() []string {
	var codes []string
	for code := range c.LangBackends {
		if code != "en" {
			codes = append(codes, code)
		}
	}
	sort.Strings(codes)
	return codes
}
