package agterm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type screenMeta struct {
	Foreground []string `json:"foreground"`
	Title      string   `json:"title"`
	Status     string   `json:"status"`
	Cursor     int      `json:"cursor"`
	Expected   State    `json:"expected"`
}

// Screens captured from real agterm sessions (agtermctl session text --lines 200,
// surface cursor, tree title/status/foreground).
func TestClassifyCapturedScreens(t *testing.T) {
	metas, _ := filepath.Glob("testdata/screens/*.json")
	if len(metas) == 0 {
		t.Fatal("no fixtures")
	}
	for _, mp := range metas {
		name := strings.TrimSuffix(filepath.Base(mp), ".json")
		t.Run(name, func(t *testing.T) {
			var m screenMeta
			raw, err := os.ReadFile(mp)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			screen, err := os.ReadFile(strings.TrimSuffix(mp, ".json") + ".txt")
			if err != nil {
				t.Fatal(err)
			}
			got := Classify(Inputs{Foreground: m.Foreground, Title: m.Title, Status: m.Status, Screen: string(screen), Cursor: m.Cursor})
			if got.State != m.Expected {
				t.Fatalf("state = %s (%s), want %s", got.State, got.Reason, m.Expected)
			}
		})
	}
}

const claudeBox = "\n\n" + rule + "\n❯ \n" + rule + "\n  ~/x | Opus 5.5\n  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"

func TestClassifyEdges(t *testing.T) {
	launcher := []string{"/bin/zsh", "-l", "/Users/tester/agent-briefs/x/steps/04/implement-return.launch.zsh"}
	streaming := "⏺ The Last Light\n  For thirty-one years he climbed the steps." + claudeBox
	cases := []struct {
		name string
		in   Inputs
		want State
	}{
		{"launcher wrapper with a framed composer is claude", Inputs{Foreground: launcher, Screen: claudeBox, Cursor: 2, Title: "✳ x"}, Ready},
		{"shell ❯ prompt under a launcher is not a composer", Inputs{Foreground: launcher, Screen: "out\n❯ ", Cursor: 2}, Unknown},
		{"shell ❯ prompt with no foreground", Inputs{Screen: claudeBox, Cursor: 2}, Unknown},
		{"streaming without spinner row: title spinner says working", Inputs{Foreground: []string{"claude"}, Screen: streaming, Cursor: 2, Title: "◐ story"}, Working},
		{"braille title spinner", Inputs{Foreground: []string{"claude"}, Screen: streaming, Cursor: 2, Title: "⠂ story"}, Working},
		{"idle title wins over a stale active status", Inputs{Foreground: []string{"claude"}, Screen: streaming, Cursor: 2, Title: "✳ story", Status: "active"}, Ready},
		{"no title: hook status active", Inputs{Foreground: []string{"claude"}, Screen: streaming, Cursor: 2, Status: "active"}, Working},
		{"spinner row above the box", Inputs{Foreground: []string{"claude"}, Screen: "✶ Mulling… (1m 33s · ↓ 8.1k tokens)\n  ⎿  Tip: x" + claudeBox, Cursor: 2, Title: "x"}, Working},
		{"finished-turn row is not a spinner", Inputs{Foreground: []string{"claude"}, Screen: "✻ Churned for 23s · done 6:42 PM" + claudeBox, Cursor: 2}, Ready},
		{"claude wrapper command line", Inputs{Foreground: []string{"/bin/zsh", "-l", "-c", "env HOME=/Users/tester claude; exec /bin/zsh -l"}, Screen: claudeBox, Cursor: 2}, Ready},
		{"draft: cursor past the prompt", Inputs{Foreground: []string{"claude"}, Screen: strings.Replace(claudeBox, "❯ ", "❯ hello", 1), Cursor: 7}, Draft},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.in); got.State != c.want {
				t.Fatalf("state = %s (%s), want %s", got.State, got.Reason, c.want)
			}
		})
	}
}

func TestDetectAgent(t *testing.T) {
	cases := []struct {
		fg   []string
		want Agent
	}{
		{[]string{"claude", "--dangerously-skip-permissions"}, Claude},
		{[]string{"node", "/usr/lib/node_modules/@openai/codex/bin/codex.js"}, Codex},
		{[]string{"/bin/zsh", "-l", "-c", "codex; exec /bin/zsh -l"}, Codex},
		{[]string{"claude", "ask codex for a review"}, ""},
		{[]string{"vim", "notes.md"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := DetectAgent(c.fg); got != c.want {
			t.Errorf("DetectAgent(%q) = %q, want %q", c.fg, got, c.want)
		}
	}
}

func TestTrustDialog(t *testing.T) {
	no := " Quick safety check\n\n ❯ No, exit\n   Yes, I trust this folder\n\n Enter to confirm · Esc to cancel"
	yes := " Quick safety check\n\n   No, exit\n ❯ Yes, I trust this folder\n\n Enter to confirm · Esc to cancel"
	if shown, sel := TrustDialog(no); !shown || sel {
		t.Fatalf("no selected: shown=%v yes=%v", shown, sel)
	}
	if shown, sel := TrustDialog(yes); !shown || !sel {
		t.Fatalf("yes selected: shown=%v yes=%v", shown, sel)
	}
	if shown, _ := TrustDialog(claudeBox); shown {
		t.Fatal("composer is not a trust dialog")
	}
}
