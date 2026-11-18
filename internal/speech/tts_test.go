package speech

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestExtractTTS(t *testing.T) {
	cases := []struct {
		name, in, text, spoken, lang string
	}{
		{"no block", "plain reply", "plain reply", "", ""},
		{"block at end", "the reply\n\n<tts>spoken version</tts>", "the reply", "spoken version", ""},
		{"text after close", "reply <tts>spoken</tts> trailing", "reply\ntrailing", "spoken", ""},
		{"unclosed tag", "reply <tts>oops", "reply <tts>oops", "", ""},
		{"multiple blocks uses last", "<tts>first</tts> mid <tts>second</tts>", "<tts>first</tts> mid", "second", ""},
		{"only block", "<tts>just voice</tts>", "", "just voice", ""},

		// Language attribute — routes to a per-language backend.
		{"lang attr", "reply\n\n<tts lang=\"ru\">по-русски</tts>", "reply", "по-русски", "ru"},
		{"lang single quotes", "reply <tts lang='ru'>текст</tts>", "reply", "текст", "ru"},
		{"lang unquoted", "reply <tts lang=ru>текст</tts>", "reply", "текст", "ru"},
		{"lang uppercase normalized", "reply <tts lang=\"RU\">текст</tts>", "reply", "текст", "ru"},
		{"lang with spaces", "reply <tts  lang = \"ru\" >текст</tts>", "reply", "текст", "ru"},
		{"lang en explicit", "reply <tts lang=\"en\">spoken</tts>", "reply", "spoken", "en"},
		{"last block's lang wins", "<tts lang=\"en\">a</tts> mid <tts lang=\"ru\">б</tts>", "<tts lang=\"en\">a</tts> mid", "б", "ru"},
		// A bare <tts> must still work — the instruction is optional and old
		// replies must not break.
		{"unknown attr ignored", "reply <tts foo=\"bar\">spoken</tts>", "reply", "spoken", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, spoken, lang := ExtractTTS(c.in)
			if text != c.text || spoken != c.spoken || lang != c.lang {
				t.Errorf("got text=%q spoken=%q lang=%q, want text=%q spoken=%q lang=%q",
					text, spoken, lang, c.text, c.spoken, c.lang)
			}
		})
	}
}

func TestParseTTSLangBackends(t *testing.T) {
	got := ParseTTSLangBackends("ru|http://127.0.0.1:8881/v1|silero|eugene,de|http://127.0.0.1:8882/v1|piper|thorsten")
	if len(got) != 2 {
		t.Fatalf("want 2 backends, got %d (%+v)", len(got), got)
	}
	ru := got["ru"]
	if ru.URL != "http://127.0.0.1:8881/v1" || ru.Model != "silero" || ru.Voice != "eugene" {
		t.Errorf("bad ru backend: %+v", ru)
	}
	if got["de"].Voice != "thorsten" {
		t.Errorf("bad de backend: %+v", got["de"])
	}
}

func TestParseTTSLangBackendsSkipsMalformed(t *testing.T) {
	// Trailing slash trimmed, wrong field count dropped, blanks ignored —
	// a typo in one entry must not take the whole map down.
	got := ParseTTSLangBackends("ru|http://127.0.0.1:8881/v1/|silero|eugene, ,broken|only|three,,xx||model|voice")
	if len(got) != 1 {
		t.Fatalf("want only the valid entry, got %d (%+v)", len(got), got)
	}
	if got["ru"].URL != "http://127.0.0.1:8881/v1" {
		t.Errorf("trailing slash not trimmed: %q", got["ru"].URL)
	}
}

func TestTTSConfigBackendFor(t *testing.T) {
	cfg := TTSConfig{
		URL: "http://127.0.0.1:8880/v1", Model: "kokoro", DefaultVoice: "af_heart",
		LangBackends: map[string]TTSBackend{"ru": {URL: "http://127.0.0.1:8881/v1", Model: "silero", Voice: "eugene"}},
	}
	for _, c := range []struct{ lang, wantURL, wantVoice string }{
		{"ru", "http://127.0.0.1:8881/v1", "eugene"},
		{"RU", "http://127.0.0.1:8881/v1", "eugene"},
		{"", "http://127.0.0.1:8880/v1", "af_heart"},
		{"ja", "http://127.0.0.1:8880/v1", "af_heart"},
	} {
		if b := cfg.BackendFor(c.lang); b.URL != c.wantURL || b.Voice != c.wantVoice {
			t.Errorf("lang %q -> %+v", c.lang, b)
		}
	}
	if got := cfg.ExtraLangs(); len(got) != 1 || got[0] != "ru" {
		t.Errorf("ExtraLangs = %v", got)
	}
}

func TestSynthesizeOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/speech" {
			t.Errorf("path=%s", r.URL.Path)
		}
		w.Write([]byte("OggSfakeaudio"))
	}))
	defer srv.Close()
	data, err := Synthesize(context.Background(), 5*time.Second, TTSBackend{URL: srv.URL, Model: "kokoro", Voice: "af_heart"}, "hello")
	if err != nil || string(data) != "OggSfakeaudio" {
		t.Fatalf("data=%q err=%v", data, err)
	}
}

// TestSynthesizeSendsLangCode: every speech request carries lang_code, the
// <tts> block's language (default en).
func TestSynthesizeSendsLangCode(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		lc, _ := body["lang_code"].(string)
		got = append(got, lc)
		w.Write([]byte("OggS"))
	}))
	defer srv.Close()
	cfg := TTSConfig{URL: srv.URL, Model: "m", DefaultVoice: "af_heart",
		LangBackends: ParseTTSLangBackends("es|" + srv.URL + "|m|ef_dora,fr|" + srv.URL + "|m|ff_siwis")}
	langs := []string{"", "EN", "es", "fr", "it", "pt", "hi"}
	for _, l := range langs {
		if _, err := Synthesize(context.Background(), 5*time.Second, cfg.BackendFor(l), "hello"); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"en", "en", "es", "fr", "it", "pt", "hi"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("lang_code sent %v, want %v", got, want)
	}
}

func TestLangCode(t *testing.T) {
	for in, want := range map[string]string{"": "en", " ": "en", "en": "en", "EN": "en", "es": "es", "fr": "fr",
		"it": "it", "pt": "pt", "hi": "hi"} {
		if got := LangCode(in); got != want {
			t.Errorf("LangCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSynthesizeErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-Mode") {
		case "500":
			http.Error(w, "boom", 500)
		default:
			w.Write([]byte("<html>not audio</html>"))
		}
	}))
	defer srv.Close()
	if _, err := Synthesize(context.Background(), 5*time.Second, TTSBackend{URL: srv.URL, Model: "kokoro", Voice: "v"}, "t"); err == nil || !strings.Contains(err.Error(), "non-ogg") {
		t.Errorf("want non-ogg error, got %v", err)
	}
}

func TestSynthesizeHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()
	if _, err := Synthesize(context.Background(), 5*time.Second, TTSBackend{URL: srv.URL, Model: "kokoro", Voice: "v"}, "t"); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("want HTTP 500 error, got %v", err)
	}
}

func TestListVoicesFiltersEnglish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/voices" {
			t.Errorf("path=%s", r.URL.Path)
		}
		w.Write([]byte(`{"voices":["af_heart",{"id":"am_michael","name":"am_michael"},"bf_emma","bm_george","jf_alpha","zf_xiaobei"]}`))
	}))
	defer srv.Close()
	got, err := ListVoices(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"af_heart", "am_michael", "bf_emma", "bm_george"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v", got)
		}
	}
}

func TestSynthesisTimeoutScalesWithLength(t *testing.T) {
	cases := []struct {
		name  string
		chars int
		want  time.Duration
	}{
		{"short reply", 500, 90 * time.Second},
		// The 4,313-char block that timed out against the old flat 180s deadline.
		{"long reply", 4313, 318780 * time.Millisecond},
		{"clamped to ceiling", 100000, 600 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SynthesisTimeout(600*time.Second, strings.Repeat("a", tc.chars))
			if got != tc.want {
				t.Fatalf("synthesisTimeout(%d chars) = %s, want %s", tc.chars, got, tc.want)
			}
		})
	}
}

func TestSynthesisTimeoutCountsRunesNotBytes(t *testing.T) {
	// Cyrillic is 2 bytes/rune; byte-counting would double the deadline.
	if got, want := SynthesisTimeout(600*time.Second, strings.Repeat("я", 500)), 90*time.Second; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// TestIntegrationLiveBackends exercises the configured TTS servers (TTS_URL,
// TTS_LANG_BACKENDS; else the loopback defaults). Opt-in (HOMMY_INTEGRATION=1)
// so the normal suite stays hermetic. It checks the thing unit tests
// structurally cannot: that each backend is reachable and actually returns
// ogg/opus that Telegram will accept.
func TestIntegrationLiveBackends(t *testing.T) {
	if os.Getenv("HOMMY_INTEGRATION") != "1" {
		t.Skip("set HOMMY_INTEGRATION=1 to run against live TTS servers")
	}
	cfg := TTSConfig{
		URL:   strings.TrimSuffix(envOr("TTS_URL", DefaultTTSURL), "/"),
		Model: envOr("TTS_MODEL", DefaultTTSModel), DefaultVoice: "af_heart",
		Timeout:      600 * time.Second,
		LangBackends: ParseTTSLangBackends(envOr("TTS_LANG_BACKENDS", DefaultTTSLangBackends)),
	}
	cases := []struct{ name, lang, text string }{
		{"english (default backend)", "", "Your ride was sixty-nine kilometres with thirteen hundred metres of climbing."},
	}
	for _, lang := range cfg.ExtraLangs() {
		cases = append(cases, struct{ name, lang, text string }{lang, lang, "Hola, uno dos tres."})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := cfg.BackendFor(c.lang)
			start := time.Now()
			data, err := Synthesize(context.Background(), cfg.Timeout, b, c.text)
			if err != nil {
				t.Fatalf("%s backend %s: %v", c.lang, b.URL, err)
			}
			if !strings.HasPrefix(string(data), "OggS") {
				t.Fatalf("backend %s returned non-ogg (%d bytes)", b.URL, len(data))
			}
			t.Logf("%s -> %s voice=%s: %d bytes in %s", c.lang, b.URL, b.Voice, len(data), time.Since(start).Round(time.Millisecond))
		})
	}
}

func TestSanitizeForSpeech(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"prose colon becomes a comma",
			"Что под наблюдением: левое колено.",
			"Что под наблюдением, левое колено."},
		{"time keeps its digits, loses the colon",
			"Встреча в 14:30 сегодня.",
			"Встреча в 14 30 сегодня."},
		{"chained time needs the second pass",
			"Отсечка 10:30:15 ровно.",
			"Отсечка 10 30 15 ровно."},
		{"slash is spoken as слэш, so drop it",
			"Темп пять / шесть.",
			"Темп пять шесть."},
		{"slash without surrounding spaces",
			"Скорость км/ч.",
			"Скорость км ч."},
		{"colon right after a comma does not double up",
			"Итак, вот что: три бега.",
			"Итак, вот что, три бега."},
		{"punctuation Silero already ignores is left alone",
			"Слово — слово; слово (слово).",
			"Слово — слово; слово (слово)."},
		{"newlines are preserved — they carry pacing",
			"Первое: раз.\nВторое: два.",
			"Первое, раз.\nВторое, два."},
		{"text with nothing to fix is untouched",
			"Обычное предложение без всего.",
			"Обычное предложение без всего."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SanitizeForSpeech(c.in); got != c.want {
				t.Errorf("SanitizeForSpeech(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// The lang_code values hommy sends for its default languages are ones mlx-audio's Kokoro accepts: its pipeline aliases
// these ISO codes to Kokoro's own (en→a, es→e, fr→f, it→i, pt→p, hi→h), checked against the real server in 05.
func TestLangCodeKokoroAliases(t *testing.T) {
	for in, want := range map[string]string{"": "en", "EN": "en", "es": "es", "fr": "fr", "it": "it", "pt": "pt", " hi ": "hi"} {
		if got := LangCode(in); got != want {
			t.Errorf("LangCode(%q) = %q, want %q", in, got, want)
		}
	}
}
