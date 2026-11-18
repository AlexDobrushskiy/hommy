// Package textx holds small text helpers shared by the bot, hub and host.
package textx

import "strings"

// Tail returns the last n runes of s, trimmed, prefixed with "…" if truncated.
func Tail(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

// Chunk splits s into pieces of at most limit runes, preferring to
// break at a newline within the final 500 runes of a chunk.
func Chunk(s string, limit int) []string {
	var out []string
	r := []rune(s)
	for len(r) > 0 {
		if len(r) <= limit {
			out = append(out, string(r))
			break
		}
		cut := limit
		for i := limit; i > limit-500 && i > 0; i-- {
			if r[i-1] == '\n' {
				cut = i
				break
			}
		}
		out = append(out, string(r[:cut]))
		r = r[cut:]
	}
	return out
}

// Head returns the first n runes of s, suffixed with "…" if truncated.
func Head(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
