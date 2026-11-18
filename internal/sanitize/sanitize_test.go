package sanitize

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func TestLine(t *testing.T) {
	cases := []struct {
		name, text, quote, want string
	}{
		{"plain", "drop it", "", "[Telegram · tg-8812] drop it"},
		{"reply quote", "drop it", "🤖 ev-seller-outreach: dealer 3 asks a \"€500\" deposit", `[Telegram · tg-8812 · re "🤖 ev-seller-outreach: dealer 3 asks a '€500' deposit"] drop it`},
		{"slash command stays inside", "/clear", "", "[Telegram · tg-8812] /clear"},
		{"three lines, tab, controls", "/clear\tnow\x1b[2J\nsecond\x07 line\r\nthird\x7f\u0085", "", "[Telegram · tg-8812] /clear now[2J ⏎ second line ⏎ third"},
		{"blank lines and runs of spaces", "a  \n\n\n   b", "", "[Telegram · tg-8812] a ⏎ b"},
		{"nfc", "café", "", "[Telegram · tg-8812] café"},
		{"bidi override dropped, zwj kept", "a\u202Eb 👨\u200D👩", "", "[Telegram · tg-8812] ab 👨\u200D👩"},
		{"empty", "\x00\x01", "", "[Telegram · tg-8812] (empty message)"},
		{"trailing backslash", `to C:\temp\`, "", `[Telegram · tg-8812] to C:\temp\ ·`},
		{"backslash before a line break", "a\\\nb\\\\", "", `[Telegram · tg-8812] a\ ⏎ b\\ ·`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Line(c.text, 8812, c.quote); got != c.want {
				t.Fatalf("Line = %q, want %q", got, c.want)
			}
		})
	}
}

func TestLineCapsAt1500(t *testing.T) {
	text := strings.Repeat("слово ", 600) // 3600 runes
	got := Line(text, 1, "")
	if n := utf8.RuneCountInString(got); n > MaxRunes || n < MaxRunes-40 {
		t.Fatalf("len = %d, want within 40 of %d", n, MaxRunes)
	}
	if strings.Contains(got, "сл"+truncated) || !strings.Contains(got, "слово"+truncated) {
		t.Fatalf("cut is not on a word boundary: %q", got[len(got)-60:])
	}
	if !strings.HasSuffix(got, truncated) {
		t.Fatalf("missing truncation mark: %q", got[len(got)-60:])
	}
}

func TestQuoteHeadCapped(t *testing.T) {
	got := Prefix(5, strings.Repeat("x", 200))
	want := `[Telegram · tg-5 · re "` + strings.Repeat("x", QuoteRunes-1) + `…"] `
	if got != want {
		t.Fatalf("Prefix = %q", got)
	}
}

func checkLine(t *testing.T, line string) {
	t.Helper()
	if !utf8.ValidString(line) {
		t.Fatalf("invalid UTF-8: %q", line)
	}
	if n := utf8.RuneCountInString(line); n > MaxRunes {
		t.Fatalf("too long: %d", n)
	}
	if r, _ := utf8.DecodeRuneInString(line); strings.ContainsRune("/!#?", r) || r != '[' {
		t.Fatalf("bad first rune %q", r)
	}
	if strings.HasSuffix(line, `\`) { // Claude Code reads "\" + Return as a new line
		t.Fatalf("ends in a backslash: %q", line)
	}
	for _, r := range line {
		if unicode.IsControl(r) {
			t.Fatalf("control char %U in %q", r, line)
		}
		if unicode.Is(unicode.Cf, r) && r != 0x200C && r != 0x200D {
			t.Fatalf("format char %U in %q", r, line)
		}
	}
	if !norm.NFC.IsNormalString(line) {
		t.Fatalf("not NFC: %q", line)
	}
}

func FuzzLine(f *testing.F) {
	for _, s := range []string{"", "/clear", "!rm -rf ~", "#memo", "?", "a\tb\nc\r\nd", "\x1b[31mred\x1b[0m", "\u202Eevil", strings.Repeat("é", 2000), "\xff\xfe"} {
		f.Add(s, s, int64(42))
	}
	f.Fuzz(func(t *testing.T, text, quote string, id int64) {
		checkLine(t, Line(text, id, quote))
	})
}
