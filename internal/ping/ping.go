// Package ping is what an agent's message to the owner looks like (header, chunks,
// inline buttons) and what his reactions mean; shared by the hub and the
// `hommy send` / `inbox` CLIs.
package ping

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Header is the first line of an agent's message: `🤖 <slug> · <host>`.
func Header(slug, host string) string { return "🤖 " + slug + " · " + host }

// HeaderRe reads a header: `🤖 slug · host`, or the older `🤖 slug@host`.
var HeaderRe = regexp.MustCompile(`^🤖 ([^\s@·]+)(?: · |@)([a-z0-9][a-z0-9-]{0,31})`)

// SlugRe is a valid agent slug.
var SlugRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// LinkPrefix + a bridge session id opens the session in the Claude app.
const LinkPrefix = "https://claude.ai/code/"

// Text is the full message: header, body, then the session link when known.
func Text(slug, host, link, body string) string {
	s := Header(slug, host) + "\n" + body
	if link != "" {
		s += "\n" + link
	}
	return s
}

// Quote is the short form a typed line quotes: `🤖 slug: body`.
func Quote(slug, body string) string { return "🤖 " + slug + ": " + body }

// Limits for one Telegram message: send.py's (plain sends keep its splits)
// and a margin for agent messages, whose emoji header counts double in UTF-16.
const (
	PlainLimit = 4096
	AgentLimit = 4000
)

// Chunk splits text as send.py does: at the last newline before limit runes
// (else hard at limit), dropping the newlines at the cut.
func Chunk(text string, limit int) []string {
	var out []string
	for utf8.RuneCountInString(text) > limit {
		r := []rune(text)
		cut := strings.LastIndex(string(r[:limit]), "\n")
		if cut <= 0 {
			cut = len(string(r[:limit]))
		}
		out = append(out, text[:cut])
		text = strings.TrimLeft(text[cut:], "\n")
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// MaxButtons and MaxLabelRunes bound --buttons.
const (
	MaxButtons    = 8
	MaxLabelRunes = 40
)

// ParseButtons reads "A|B|Answer…".
func ParseButtons(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []string
	for _, l := range strings.Split(s, "|") {
		l = strings.TrimSpace(l)
		if l == "" {
			return nil, fmt.Errorf("--buttons: empty label in %q", s)
		}
		if utf8.RuneCountInString(l) > MaxLabelRunes {
			return nil, fmt.Errorf("--buttons: label %q is longer than %d characters", l, MaxLabelRunes)
		}
		out = append(out, l)
	}
	if len(out) > MaxButtons {
		return nil, fmt.Errorf("--buttons: at most %d buttons", MaxButtons)
	}
	return out, nil
}

// IsAnswer reports an "answer in text" label (ends with … or ...).
func IsAnswer(label string) bool {
	return strings.HasSuffix(label, "…") || strings.HasSuffix(label, "...")
}

// WithAnswer appends an "Answer…" button when none of labels is one.
func WithAnswer(labels []string) []string {
	for _, l := range labels {
		if IsAnswer(l) {
			return labels
		}
	}
	return append(labels, "Answer…")
}

// Button is one inline button.
type Button struct {
	Text   string  `json:"text"`
	Data   string  `json:"callback_data,omitempty"`
	WebApp *WebApp `json:"web_app,omitempty"` // a Mini App button (the console's "Open") instead of a callback
}

// WebApp is a web_app button's page.
type WebApp struct {
	URL string `json:"url"`
}

// OpenLabel is the text of the button that opens a session's console.
const OpenLabel = "Open"

// OpenRow is the row with the console's "Open" button (none when url is "").
func OpenRow(url string) [][]Button {
	if url == "" {
		return nil
	}
	return [][]Button{{{Text: OpenLabel, WebApp: &WebApp{URL: url}}}}
}

// CallbackData is a button's `ag:<msgid>:<i>`.
func CallbackData(msgID, i int) string { return fmt.Sprintf("ag:%d:%d", msgID, i) }

// ParseCallback reads `ag:<msgid>:<i>`.
func ParseCallback(data string) (msgID, i int, ok bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "ag" {
		return 0, 0, false
	}
	id, err := strconv.Atoi(parts[1])
	if err != nil || id <= 0 {
		return 0, 0, false
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil || n < 0 {
		return 0, 0, false
	}
	return id, n, true
}

// Keyboard lays labels out three to a row, each pointing at msgID.
func Keyboard(msgID int, labels []string) [][]Button {
	var rows [][]Button
	for i, l := range labels {
		if i%3 == 0 {
			rows = append(rows, nil)
		}
		rows[len(rows)-1] = append(rows[len(rows)-1], Button{Text: l, Data: CallbackData(msgID, i)})
	}
	return rows
}

// Pressed is the line a pressed button leaves on its message.
func Pressed(text, label string) string { return text + "\n→ " + label + " ✓" }

// meanings are what the owner's reactions say (typed after the emoji).
var meanings = map[string]string{
	"👍": "yes / continue",
	"❤": "👍 + good work",
	"👎": "no / drop",
	"🤔": "explain / options",
	"👀": "seen, nothing to do",
	"🔥": "urgent",
}

// bare drops variation selectors, so ❤️ and ❤ read the same.
func bare(emoji string) string { return strings.ReplaceAll(emoji, "️", "") }

// Meaning is a reaction as typed: the emoji with its meaning ("👍 yes / continue").
func Meaning(emoji string) string {
	e := bare(emoji)
	if m, ok := meanings[e]; ok {
		return e + " " + m
	}
	return e
}

// ReactionLine says what changed between the owner's old and new reactions on a
// message: added ones with their meaning, else "retracted …"; "" when nothing
// he can see changed.
func ReactionLine(old, new []string) string {
	has := func(set []string, e string) bool {
		for _, s := range set {
			if bare(s) == bare(e) {
				return true
			}
		}
		return false
	}
	var added, removed []string
	for _, e := range new {
		if !has(old, e) {
			added = append(added, Meaning(e))
		}
	}
	for _, e := range old {
		if !has(new, e) {
			removed = append(removed, bare(e))
		}
	}
	switch {
	case len(added) > 0:
		return strings.Join(added, " · ")
	case len(removed) > 0:
		return "retracted " + strings.Join(removed, " ")
	}
	return ""
}
