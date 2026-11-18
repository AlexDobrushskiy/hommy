package agterm

import (
	"regexp"
	"strings"
	"unicode"
)

// Helpers that reproduce the Python string semantics peer-chat's parser relies on
// (str.isspace, str.strip, str.splitlines, re's Unicode \s), so the ported rules
// read screens exactly like peer-chat.py does.

// wsClass is Python's Unicode whitespace as a regexp class body.
const wsClass = `\t\n\v\f\r \x{1c}-\x{1f}\x{85}\p{Zs}\x{2028}\x{2029}`

// rx compiles a pattern after expanding `{WS}` to the body of Python's Unicode
// whitespace class, so `[{WS}]` is Python's \s and `[^{WS}]` its \S.
func rx(pattern string) *regexp.Regexp {
	return regexp.MustCompile(strings.ReplaceAll(pattern, "{WS}", wsClass))
}

func isSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0x2028, 0x2029:
		return true
	}
	return unicode.Is(unicode.Zs, r)
}

func strip(s string) string  { return strings.TrimFunc(s, isSpace) }
func rstrip(s string) string { return strings.TrimRightFunc(s, isSpace) }

// splitLines is Python's str.splitlines(): no trailing empty line, "" → nil.
func splitLines(s string) []string {
	var out []string
	var cur strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\r':
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			fallthrough
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(rs[i])
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// rowsOf is `text.splitlines() or [text]`.
func rowsOf(s string) []string {
	if rows := splitLines(s); len(rows) > 0 {
		return rows
	}
	return []string{s}
}

func lastN(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

func firstRune(s string) (rune, bool) {
	for _, r := range s {
		return r, true
	}
	return 0, false
}

func lastRune(s string) (rune, bool) {
	rs := []rune(s)
	if len(rs) == 0 {
		return 0, false
	}
	return rs[len(rs)-1], true
}
