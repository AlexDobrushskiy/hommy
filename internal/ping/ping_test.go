package ping

import (
	"strings"
	"testing"
)

func TestChunkLikeSendPy(t *testing.T) {
	// send.py: cut at the last newline before the limit (else hard), drop the newlines at the cut
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"short", []string{"short"}},
		{"aaaa\n\nbb", []string{"aaaa\n", "bb"}},
		{"abcdefghij", []string{"abcdef", "ghij"}},
		{"\nabcdefgh", []string{"\nabcde", "fgh"}}, // a newline at 0 doesn't count (cut > 0)
		{"жжжжжжжж", []string{"жжжжжж", "жж"}},
	} {
		if got := Chunk(c.in, 6); strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("Chunk(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestReactionLine(t *testing.T) {
	for _, c := range []struct {
		old, new []string
		want     string
	}{
		{nil, []string{"👍"}, "👍 yes / continue"},
		{nil, []string{"❤️"}, "❤ 👍 + good work"},
		{[]string{"👍"}, []string{"🔥"}, "🔥 urgent"},
		{[]string{"👍"}, nil, "retracted 👍"},
		{nil, []string{"🎃"}, "🎃"},
		{[]string{"👍"}, []string{"👍"}, ""},
		{[]string{"👍", "🤔"}, []string{"👍"}, "retracted 🤔"},
	} {
		if got := ReactionLine(c.old, c.new); got != c.want {
			t.Errorf("%v → %v: %q, want %q", c.old, c.new, got, c.want)
		}
	}
}

func TestButtonsAndCallbacks(t *testing.T) {
	got, err := ParseButtons(" Keep | Drop |Answer…")
	if err != nil || strings.Join(got, ",") != "Keep,Drop,Answer…" || !IsAnswer(got[2]) || IsAnswer(got[0]) {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"A||B", strings.Repeat("x", 41), "1|2|3|4|5|6|7|8|9"} {
		if _, err := ParseButtons(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	kb := Keyboard(42, []string{"a", "b", "c", "d"})
	if len(kb) != 2 || kb[1][0].Data != "ag:42:3" {
		t.Errorf("keyboard %+v", kb)
	}
	if id, i, ok := ParseCallback("ag:42:3"); !ok || id != 42 || i != 3 {
		t.Errorf("parse %d %d %v", id, i, ok)
	}
	for _, bad := range []string{"cb_B", "ag:0:1", "ag:x:1", "ag:1:-1", "ag:1"} {
		if _, _, ok := ParseCallback(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
	if w := WithAnswer([]string{"A"}); len(w) != 2 || w[1] != "Answer…" {
		t.Errorf("WithAnswer %q", w)
	}
}

func TestHeader(t *testing.T) {
	for in, want := range map[string]string{
		Text("ev-seller", "green", "", "hi"): "ev-seller green",
		"🤖 ev-test@black: x":                 "ev-test black",
		"🤖 nobody":                           "",
	} {
		g := HeaderRe.FindStringSubmatch(in)
		got := ""
		if g != nil {
			got = g[1] + " " + g[2]
		}
		if got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
