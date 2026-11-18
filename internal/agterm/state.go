package agterm

import (
	"context"
	"regexp"
	"strings"
)

// State is what a session's composer allows right now.
type State string

const (
	Ready   State = "ready"   // empty composer at an idle prompt: safe to type
	Draft   State = "draft"   // someone's unsent text is in the composer
	Menu    State = "menu"    // a permission / selection dialog holds the input
	Working State = "working" // the agent is mid-turn
	Unknown State = "unknown" // no recognisable Claude Code / Codex composer
)

// ScreenLines is how much of the pane a state reading takes: Claude's renderer can
// leave many blank rows between the transcript and its bottom-anchored composer.
const ScreenLines = 200

// Composer is one classified reading of a session's primary pane.
type Composer struct {
	State   State  `json:"state"`
	Agent   Agent  `json:"agent,omitempty"`
	Content string `json:"content,omitempty"`
	Cursor  int    `json:"cursor"`
	Reason  string `json:"reason,omitempty"`
}

// Inputs are the raw observations a classification needs.
type Inputs struct {
	Foreground []string
	Title      string
	Status     string
	Screen     string
	Cursor     int
}

var (
	claudeTokenRE = rx(`(?:^|[/{WS};&|'"(])(?:claude)(?:$|[{WS};&|'")])`)
	codexTokenRE  = rx(`(?:^|[/{WS};&|'"(])(?:codex|cdx)(?:$|[{WS};&|'")])`)
	codexScriptRE = regexp.MustCompile(`/codex(?:\.[cm]?js)?$`)

	// a highlighted choice row: Claude `❯ 1. Yes`, Codex `› 1. Trust and continue`.
	menuChoiceRE = rx(`^[{WS}\x{a0}│]*[❯›][{WS}\x{a0}]*\p{Nd}+\.[{WS}\x{a0}]`)
	// an un-numbered highlighted option (Claude's folder-trust dialog `❯ No, exit`).
	menuCursorRE = rx(`^[{WS}\x{a0}│]*❯[{WS}\x{a0}]+[^{WS}]`)
	menuHintRE   = regexp.MustCompile(`(?i)(esc to cancel|enter to confirm|enter to select|enter continue|esc back)`)

	// Claude's spinner row while a turn runs: `✶ Mulling… (1m 33s · ↓ 8.1k tokens)`.
	// A finished turn's row (`✻ Churned for 23s · done 6:42 PM`) has no ellipsis.
	claudeSpinnerRE = rx(`^[·✢✳✶✻✽*][{WS}\x{a0}]+[^{WS}][^…]*…`)
	// Codex's status row: `• Working (10s • esc to interrupt)`.
	codexWorkingRE = rx(`^•[{WS}\x{a0}]+Working\b|esc to interrupt`)
)

// DetectAgent names the TUI a foreground command line runs; "" when it names
// neither or both.
func DetectAgent(foreground []string) Agent {
	claude, codex := false, false
	for _, part := range foreground {
		if claudeTokenRE.MatchString(part) {
			claude = true
		}
		if codexTokenRE.MatchString(part) || codexScriptRE.MatchString(part) {
			codex = true
		}
	}
	if LeadingCommand(foreground) == "codex" {
		codex = true
	}
	switch {
	case claude && !codex:
		return Claude
	case codex && !claude:
		return Codex
	}
	return ""
}

// claudeBoxed: a Claude composer with its full frame (rule above the prompt row,
// closing rule below). A shell prompt drawn as `❯` has no such frame.
func claudeBoxed(screen string) bool {
	lines := lastN(splitLines(screen), BoxLines)
	for i := len(lines) - 1; i >= 0; i-- {
		if !claudePromptRE.MatchString(lines[i]) {
			continue
		}
		if i == 0 || !ruleRE.MatchString(lines[i-1]) {
			return false
		}
		for _, l := range lines[i+1:] {
			if ruleRE.MatchString(l) {
				_, ok := ClaudeLivePromptText(screen)
				return ok
			}
		}
		return false
	}
	return false
}

// codexFramed: a Codex composer followed by its footer rows.
func codexFramed(screen string) bool {
	lines := splitLines(screen)
	for len(lines) > 0 && CodexRowIsBlank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 || !codexFooterRE.MatchString(lines[len(lines)-1]) {
		return false
	}
	_, ok := CodexLivePromptText(screen)
	return ok
}

// screenAgent identifies the TUI from the screen alone, used when the foreground
// is a wrapper (a launcher script) that names neither agent. It demands the full
// frame so a shell prompt is never taken for a composer.
func screenAgent(screen string) Agent {
	claude, codex := claudeBoxed(screen), codexFramed(screen)
	switch {
	case claude && !codex:
		return Claude
	case codex && !claude:
		return Codex
	}
	return ""
}

func nonBlankTail(screen string, n int) []string {
	lines := splitLines(screen)
	var out []string
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if strip(lines[i]) != "" {
			out = append([]string{lines[i]}, out...)
		}
	}
	return out
}

func menuShown(screen string) bool {
	tail := nonBlankTail(screen, 30)
	for _, l := range tail {
		if menuChoiceRE.MatchString(l) {
			return true
		}
	}
	cursor := false
	for _, l := range tail {
		if menuCursorRE.MatchString(l) {
			cursor = true
		}
	}
	if !cursor {
		return false
	}
	for _, l := range nonBlankTail(screen, 4) {
		if menuHintRE.MatchString(l) {
			return true
		}
	}
	return false
}

// TrustDialog reports Claude's folder-trust dialog and whether "Yes" is selected.
func TrustDialog(screen string) (shown, yesSelected bool) {
	tail := nonBlankTail(screen, 15)
	for _, l := range tail {
		if strings.Contains(l, "Yes, I trust this folder") {
			shown = true
			if menuCursorRE.MatchString(l) {
				yesSelected = true
			}
		}
	}
	return shown, yesSelected
}

// rows just above the composer: Claude's top rule, Codex's prompt row.
func rowsAboveComposer(agent Agent, screen string, n int) []string {
	lines := lastN(splitLines(screen), ScreenLines)
	anchor := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if agent == Claude && claudePromptRE.MatchString(lines[i]) {
			anchor = i
			if i > 0 && ruleRE.MatchString(lines[i-1]) {
				anchor = i - 1
			}
			break
		}
		if agent == Codex && codexPromptRE.MatchString(lines[i]) {
			anchor = i
			break
		}
	}
	var out []string
	for i := anchor - 1; i >= 0 && len(out) < n; i-- {
		l := strip(lines[i])
		if l == "" || strings.HasPrefix(l, "⎿") {
			continue
		}
		out = append(out, lines[i])
	}
	return out
}

// claudeTitleWorking reads the terminal title Claude Code sets itself: `✳ name`
// when idle, a spinner glyph while a turn runs. ok=false when it is neither.
func claudeTitleWorking(title string) (working, ok bool) {
	r, has := firstRune(strings.TrimSpace(title))
	if !has {
		return false, false
	}
	switch {
	case r == '✳':
		return false, true
	case strings.ContainsRune("◐◑◒◓·✢✶✻✽", r) || isParticle(r):
		return true, true
	}
	return false, false
}

func working(agent Agent, in Inputs) bool {
	above := rowsAboveComposer(agent, in.Screen, 3)
	if agent == Codex {
		for _, l := range above {
			if codexWorkingRE.MatchString(strip(l)) {
				return true
			}
		}
		return false
	}
	for _, l := range above {
		if claudeSpinnerRE.MatchString(strip(l)) {
			return true
		}
	}
	if w, ok := claudeTitleWorking(in.Title); ok {
		return w
	}
	// agterm's agent-status hooks: `active` while a turn runs.
	return in.Status == "active"
}

// Classify decides the composer state from one set of observations.
func Classify(in Inputs) Composer {
	if len(in.Foreground) == 0 {
		return Composer{State: Unknown, Cursor: in.Cursor, Reason: "no program in the foreground (shell prompt)"}
	}
	agent := DetectAgent(in.Foreground)
	if agent == "" {
		agent = screenAgent(in.Screen)
	}
	if agent == "" {
		if menuShown(in.Screen) {
			// a dialog hides the composer frame; the title Claude sets still names it.
			c := Composer{State: Menu, Cursor: in.Cursor, Reason: "a dialog holds the input"}
			if _, ok := claudeTitleWorking(in.Title); ok {
				c.Agent = Claude
			}
			return c
		}
		return Composer{State: Unknown, Cursor: in.Cursor, Reason: "no Claude Code or Codex composer on screen"}
	}
	c := Composer{Agent: agent, Cursor: in.Cursor}
	content, ok := LivePromptText(agent, in.Screen)
	if !ok {
		if menuShown(in.Screen) {
			c.State, c.Reason = Menu, "a dialog holds the input"
			return c
		}
		c.State, c.Reason = Unknown, "composer not recognisable"
		return c
	}
	c.Content = content
	empty := ComposerIsEmpty(agent, content)
	switch {
	case in.Cursor != EmptyCursorColumn:
		c.State, c.Reason = Draft, "cursor past the prompt"
	case agent == Codex && !empty:
		c.State, c.Reason = Draft, "text in the composer"
	case agent == Claude && claudeHasBlankRow(in.Screen):
		c.State, c.Reason = Draft, "a line break in the composer"
	case working(agent, in):
		c.State, c.Reason = Working, "a turn is running"
	default:
		// Claude draws free-form suggestions as plain text at column 2; they are not drafts.
		c.State = Ready
	}
	return c
}

// Observe reads a session and classifies it.
func Observe(ctx context.Context, ctl Ctl, s Session) (Composer, string, error) {
	screen, err := ctl.Text(ctx, s, ScreenLines)
	if err != nil {
		return Composer{}, "", err
	}
	cursor, err := ctl.Cursor(ctx, s)
	if err != nil {
		return Composer{}, "", err
	}
	return Classify(Inputs{Foreground: s.Foreground, Title: s.Title, Status: s.Status, Screen: screen, Cursor: cursor}), screen, nil
}
