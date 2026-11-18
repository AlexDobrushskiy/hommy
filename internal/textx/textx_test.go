package textx

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTailRuneSafe(t *testing.T) {
	s := strings.Repeat("ошибка ", 600)
	got := Tail(s, 500)
	if !utf8.ValidString(got) {
		t.Fatalf("Tail produced invalid UTF-8: %q", got)
	}
	r := []rune(got)
	if len(r) != 501 || r[0] != '…' {
		t.Errorf("want ellipsis + 500 runes, got %d runes starting %q", len(r), r[0])
	}
	if Tail("  short  ", 10) != "short" {
		t.Errorf("short input should be trimmed, untouched otherwise")
	}
}

func TestChunk(t *testing.T) {
	if got := Chunk("hello", 10); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("short: %q", got)
	}
	s := strings.Repeat("a", 3700) + "\n" + strings.Repeat("b", 3000)
	got := Chunk(s, 4000)
	if len(got) != 2 || !strings.HasSuffix(got[0], "\n") || got[1] != strings.Repeat("b", 3000) {
		t.Fatalf("want a break at the newline, got lens %d", len(got))
	}
	if got := Chunk(strings.Repeat("я", 9000), 4000); len(got) != 3 || utf8.RuneCountInString(got[0]) != 4000 {
		t.Fatalf("hard cut: %d chunks", len(got))
	}
}
