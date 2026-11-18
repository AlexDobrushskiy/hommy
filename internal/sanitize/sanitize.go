// Package sanitize turns a Telegram message into the one line typed into an agent
// session: NFC, no control characters, newlines shown as ⏎, capped, and always
// prefixed so the line can never start a TUI command (/ ! # ?).
package sanitize

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	// MaxRunes caps the typed line; the full text is in the session's inbox.
	MaxRunes = 1500
	// QuoteRunes caps the quoted head of the replied-to message.
	QuoteRunes = 60
	newline    = " ⏎ "
	truncated  = " … [cut; full text in inbox]"
	// backslashEnd follows a trailing backslash; a trailing space would not show
	// on screen, so the typed line could not be verified.
	backslashEnd = " ·"
)

// Line is the full typed line: prefix + sanitised body, at most MaxRunes runes.
func Line(text string, msgID int64, quote string) string {
	return LineID(text, "tg-"+strconv.FormatInt(msgID, 10), quote)
}

// LineID is Line with the inbox item id spelled out (reactions and buttons
// have ids of their own, e.g. tg-8812-r41).
func LineID(text, id, quote string) string {
	prefix := PrefixID(id, quote)
	body := Body(text)
	if body == "" {
		body = "(empty message)"
	}
	// Claude Code reads "\" + Return as a new line instead of a submit.
	if strings.HasSuffix(body, `\`) {
		body += backslashEnd
	}
	room := MaxRunes - utf8.RuneCountInString(prefix)
	if utf8.RuneCountInString(body) > room {
		keep := room - utf8.RuneCountInString(truncated)
		cut := string([]rune(body)[:keep])
		// end on a word boundary when one is near
		if i := strings.LastIndexByte(cut, ' '); i >= 0 && utf8.RuneCountInString(cut[i:]) <= 40 {
			cut = cut[:i]
		}
		body = strings.TrimRight(cut, " ⏎") + truncated
	}
	return prefix + body
}

// Prefix is `[Telegram · tg-<id> · re "<quoted head>"] `; the re part only
// when the message replies to something.
func Prefix(msgID int64, quote string) string {
	return PrefixID("tg-"+strconv.FormatInt(msgID, 10), quote)
}

// PrefixID is Prefix for an inbox item id.
func PrefixID(id, quote string) string {
	var b strings.Builder
	b.WriteString("[Telegram · ")
	b.WriteString(Body(id))
	if q := quoteHead(quote); q != "" {
		b.WriteString(` · re "`)
		b.WriteString(q)
		b.WriteString(`"`)
	}
	b.WriteString("] ")
	return b.String()
}

func quoteHead(quote string) string {
	q := strings.NewReplacer(`"`, "'", "[", "(", "]", ")").Replace(Body(quote))
	if rs := []rune(q); len(rs) > QuoteRunes {
		q = strings.TrimRight(string(rs[:QuoteRunes-1]), " ⏎") + "…"
	}
	return q
}

// Body normalises text to one line: NFC; C0/C1 controls, ESC, DEL and format
// characters (bidi overrides, BOM) removed; tabs and whitespace runs become one
// space; each line break becomes ` ⏎ `.
func Body(text string) string {
	text = norm.NFC.String(strings.ToValidUTF8(text, "�"))
	var lines []string
	var cur strings.Builder
	flush := func() {
		if l := strings.Join(strings.Fields(cur.String()), " "); l != "" {
			lines = append(lines, l)
		}
		cur.Reset()
	}
	for _, r := range text {
		switch {
		case r == '\n' || r == '\r' || r == '\v' || r == '\f' || r == 0x85 || r == 0x2028 || r == 0x2029:
			flush()
		case r == '\t':
			cur.WriteRune(' ')
		case unicode.IsControl(r):
		case unicode.Is(unicode.Cf, r) && r != 0x200C && r != 0x200D:
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	out := strings.Join(lines, newline)
	return norm.NFC.String(out)
}
