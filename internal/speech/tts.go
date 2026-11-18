package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/textx"
)

const ttsClose = "</tts>"

// Punctuation some TTS backends read aloud as a word instead of as a pause.
//
// Measured against Silero (a Russian backend), baseline
// "Слово слово." = 1.01s:
//
//	"Слово: слово."      1.48s  (+0.47s — it says "двоеточие")
//	"Слово; слово."      1.01s  (silent)
//	"Слово — слово."     1.06s  (silent)
//	"Слово (слово)."     0.98s  (silent)
//	"Темп пять / шесть." 1.32s  == "Темп пять слэш шесть." to the millisecond
//
// Times are hit too: "Встреча в 14:30 сегодня." = 2.48s vs 1.87s for "14 30".
//
// Kokoro (English) voices neither character — "Total: two eggs today." matches
// its colon-less baseline. The substitutions below are pause-equivalent in
// English as well, so this runs for every backend rather than being made
// conditional on which one was selected; a colon should never be *spoken* in
// either language.
var (
	// 14:30 -> "14 30", so the digits are read as a time.
	digitColonRe  = regexp.MustCompile(`([0-9])\s*:\s*([0-9])`)
	slashRe       = regexp.MustCompile(`\s*/\s*`)
	repeatedComma = regexp.MustCompile(`,(\s*,)+`)
	runOfSpaces   = regexp.MustCompile(`[ \t]{2,}`)
)

// SanitizeForSpeech rewrites characters that would otherwise be pronounced.
// It only touches punctuation, never words, and deliberately leaves newlines
// alone — they carry sentence pacing.
func SanitizeForSpeech(s string) string {
	// Looped because a chained time like "10:30:15" needs a second pass: the
	// middle digit is consumed as the right-hand side of the first match.
	for {
		next := digitColonRe.ReplaceAllString(s, "$1 $2")
		if next == s {
			break
		}
		s = next
	}
	// Any colon left is prose ("Что под наблюдением: левое колено"). A comma
	// reproduces the pause without being pronounced.
	s = strings.ReplaceAll(s, ":", ",")
	s = slashRe.ReplaceAllString(s, " ")
	// Tidy artefacts the substitutions can create (", ,", double spaces).
	s = repeatedComma.ReplaceAllString(s, ",")
	s = runOfSpaces.ReplaceAllString(s, " ")
	return s
}

// ttsOpenRe matches the opening marker with an optional language attribute:
// <tts>, <tts lang="ru">, <tts lang='ru'>, <tts lang=ru>. The language selects
// a per-language backend (see TTSConfig.BackendFor); absent means the default.
var ttsOpenRe = regexp.MustCompile(`<tts(?:\s+lang\s*=\s*"([a-zA-Z-]+)"|\s+lang\s*=\s*'([a-zA-Z-]+)'|\s+lang\s*=\s*([a-zA-Z-]+))?[^>]*>`)

// englishVoicePrefixes are the Kokoro voice families we expose: American and
// British, female and male. Other languages are filtered out.
var englishVoicePrefixes = []string{"af_", "am_", "bf_", "bm_"}

// ExtractTTS splits a Claude reply into the text to display, the spoken version
// from the trailing <tts>...</tts> block, and that block's language ("" when
// unspecified). spoken is "" when the block is absent or unclosed; the block is
// always removed from text when found.
func ExtractTTS(reply string) (text, spoken, lang string) {
	locs := ttsOpenRe.FindAllStringSubmatchIndex(reply, -1)
	if len(locs) == 0 {
		return strings.TrimSpace(reply), "", ""
	}
	m := locs[len(locs)-1] // last block wins, as before
	rest := reply[m[1]:]
	end := strings.LastIndex(rest, ttsClose)
	if end < 0 {
		return strings.TrimSpace(reply), "", ""
	}
	// Groups 1-3 are the quoted / single-quoted / bare spellings of lang.
	for _, g := range []int{1, 2, 3} {
		if m[2*g] >= 0 {
			lang = strings.ToLower(reply[m[2*g]:m[2*g+1]])
			break
		}
	}
	spoken = strings.TrimSpace(rest[:end])
	before := strings.TrimSpace(reply[:m[0]])
	after := strings.TrimSpace(rest[end+len(ttsClose):])
	if after != "" {
		before = strings.TrimSpace(before + "\n" + after)
	}
	return before, spoken, lang
}

// SynthesisTimeout scales the deadline with input length, bounded by
// ceiling. Kokoro on the Mac GPU runs anywhere from ~50 to ~500 chars/s
// for the same text depending on how much of Metal's kernel cache is warm, so a
// flat deadline either starves long blocks or leaves short ones hanging. A
// 4,000-char reply measured 20s warm and 84s cold on a freshly restarted server.
func SynthesisTimeout(ceiling time.Duration, text string) time.Duration {
	d := 60*time.Second + time.Duration(len([]rune(text)))*60*time.Millisecond
	if d > ceiling {
		return ceiling
	}
	return d
}

// Synthesize renders text as an ogg/opus voice payload via the
// OpenAI-compatible speech endpoint on the given backend; ceiling bounds the
// length-scaled deadline (see SynthesisTimeout).
func Synthesize(ctx context.Context, ceiling time.Duration, backend TTSBackend, text string) ([]byte, error) {
	// Single choke point for every backend, so no caller can bypass it.
	text = SanitizeForSpeech(text)
	body, err := json.Marshal(map[string]any{
		"model":           backend.Model,
		"voice":           backend.Voice,
		"input":           text,
		"response_format": "opus",
		"lang_code":       LangCode(backend.Lang),
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, SynthesisTimeout(ceiling, text))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, backend.URL+"/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tts HTTP %d: %s", resp.StatusCode, textx.Tail(string(data), 200))
	}
	if !bytes.HasPrefix(data, []byte("OggS")) {
		return nil, fmt.Errorf("tts returned non-ogg payload (%d bytes)", len(data))
	}
	return data, nil
}

// ListVoices returns the English voices offered by the TTS server. The
// deadline covers a cold start: llama-swap loads Kokoro on demand (~25s).
func ListVoices(ctx context.Context, ttsURL string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ttsURL+"/audio/voices", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("voices HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Voices []json.RawMessage `json:"voices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	var out []string
	for _, raw := range payload.Voices {
		name := voiceID(raw)
		for _, p := range englishVoicePrefixes {
			if strings.HasPrefix(name, p) {
				out = append(out, name)
				break
			}
		}
	}
	return out, nil
}

// voiceID accepts both voice-list shapes seen across TTS servers:
// a plain string or an object with an "id" field.
func voiceID(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.ID
	}
	return ""
}
