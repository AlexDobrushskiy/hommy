package agterm

// Ports of the typing-transaction cases in ~/.claude/skills/peer-chat/test_peer_chat.py
// (send, type_body, the composer waiters, clear_composer). Each subtest is named
// after the Python test method it reproduces.
//
// Python patches peer-chat's module globals (type_body, wait_for_composer_change,
// clear_composer, …); the Go methods cannot be patched, so a fake Pane produces
// the readings those mocks returned and the real methods run on them.
// Deliberate differences from peer-chat, asserted as the Go behaviour:
//   - the submit key is "\r" (Python "\n"), and there is no queue ("\t") mode;
//   - after an unaccepted Return, Send repeats Return (SubmitAttempts in all)
//     while the composer still holds exactly the line, else returns ErrAmbiguous;
//   - there is no normalize()/profile label: lines arrive pre-sanitised, so the
//     tests type "Chat from …: " lines directly;
//   - the preflight is Classify/Observe (state.go), not part of Send;
//   - KeyboardInterrupt maps to context cancellation only where the meaning
//     carries over (the submit phase, a cleanup that is cut short).

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

const del = "\x7f"

func cr(content string, cursor int) *ComposerReading {
	return &ComposerReading{Content: content, Cursor: cursor}
}

// ---- clock ----

// fakeClock never sleeps. stepping: each Now() is 110ms after the previous one
// (Python's stepping_time / ComposerSimulation.monotonic). script: Now() returns
// the listed times (Python's Mock(monotonic=Mock(side_effect=[…]))). sleeping:
// Now() is fixed and Sleep advances it.
type fakeClock struct {
	t             *testing.T
	now           time.Duration
	step          time.Duration
	script        []time.Duration
	scripted      bool
	sleepAdvances bool
	nows, sleeps  int
}

var clockBase = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func steppingTime(t *testing.T) *fakeClock { return &fakeClock{t: t, step: 110 * time.Millisecond} }

func sleepingClock(t *testing.T) *fakeClock { return &fakeClock{t: t, sleepAdvances: true} }

func monotonicScript(t *testing.T, seconds ...float64) *fakeClock {
	c := &fakeClock{t: t, scripted: true}
	for _, s := range seconds {
		c.script = append(c.script, time.Duration(math.Round(s*float64(time.Second))))
	}
	return c
}

func (c *fakeClock) Now() time.Time {
	c.nows++
	if c.scripted {
		if len(c.script) == 0 {
			c.t.Errorf("monotonic() call %d: scripted times exhausted", c.nows)
			return clockBase.Add(time.Hour)
		}
		c.now, c.script = c.script[0], c.script[1:]
		return clockBase.Add(c.now)
	}
	v := c.now
	c.now += c.step
	return clockBase.Add(v)
}

func (c *fakeClock) Sleep(d time.Duration) {
	c.sleeps++
	if c.sleepAdvances {
		c.now += d
	}
}

// ---- scripted pane (Python's mocked composer_state / type_text) ----

type scriptPane struct {
	t      *testing.T
	state  func(call int) (*ComposerReading, error)
	onType func(text string) error
	calls  int
	typed  []string
	// statesAfter[i] counts State calls made after the i-th TypeText (0 = before any).
	statesAfter []int
}

func (p *scriptPane) State(context.Context) (*ComposerReading, error) {
	p.calls++
	if len(p.statesAfter) == 0 {
		p.statesAfter = []int{0}
	}
	p.statesAfter[len(p.statesAfter)-1]++
	return p.state(p.calls)
}

func (p *scriptPane) TypeText(_ context.Context, text string) error {
	p.typed = append(p.typed, text)
	if len(p.statesAfter) == 0 {
		p.statesAfter = []int{0}
	}
	p.statesAfter = append(p.statesAfter, 0)
	if p.onType != nil {
		return p.onType(text)
	}
	return nil
}

// sideEffect is Mock(side_effect=[…]): one reading per call; nil is None.
func sideEffect(t *testing.T, readings ...*ComposerReading) func(int) (*ComposerReading, error) {
	return func(call int) (*ComposerReading, error) {
		if call > len(readings) {
			t.Errorf("composer_state call %d: only %d readings scripted", call, len(readings))
			return nil, errors.New("composer_state side effects exhausted")
		}
		return readings[call-1], nil
	}
}

// returnValue is Mock(return_value=…).
func returnValue(r *ComposerReading) func(int) (*ComposerReading, error) {
	return func(int) (*ComposerReading, error) { return r, nil }
}

func repeatReading(r *ComposerReading, n int) []*ComposerReading {
	out := make([]*ComposerReading, n)
	for i := range out {
		out[i] = r
	}
	return out
}

// afterTypes shows readings[k] once k TypeText calls have happened: Python's
// mocked wait_for_composer_change returning one reading per typed event.
func afterTypes(p *scriptPane, readings ...*ComposerReading) func(int) (*ComposerReading, error) {
	return func(int) (*ComposerReading, error) {
		return readings[min(len(p.typed), len(readings)-1)], nil
	}
}

// ---- composer simulation (Python's ComposerSimulation, extended with hooks) ----

const simRows = 8

// composerSim is an in-memory editor: printable runes append, "\x7f" deletes
// the last rune, "\r" submits. width > 0 wraps the body like textwrap.wrap and
// shows the last 8 rows, the first marked "↑" when rows are hidden; clip > 0
// shows only the body's last clip runes on one row; otherwise the whole body
// is one row. An empty body shows idle.
type composerSim struct {
	t     *testing.T
	clock *fakeClock
	width int
	clip  func(body string) int
	idle  string
	// afterSubmit replaces idle once a Return has submitted (Claude drops its suggestion).
	afterSubmit *string

	body      string
	submitted []string
	scrolled  bool
	typed     []string
	returned  bool // a Return has been typed (submitted or not)
	// statesAfterReturn counts State calls after the first Return.
	statesAfterReturn int

	// onType decides what of event n (1-based) reaches the editor; an error
	// means nothing did.
	onType func(n int, text string) (string, error)
	// onState rewrites a reading; immediate = no Sleep or TypeText since the
	// previous State call (a re-read right after a waiter returned).
	onState func(r *ComposerReading, immediate bool) (*ComposerReading, error)

	lastWasState  bool
	sleepsAtState int
}

func (s *composerSim) reading() *ComposerReading {
	if s.body == "" {
		return cr(s.idle, EmptyCursorColumn)
	}
	var visible []string
	switch {
	case s.width > 0:
		rows := pyWrap(s.body, s.width)
		if len(rows) == 0 {
			s.t.Errorf("body %q wraps to no rows", s.body)
			return nil
		}
		visible = slices.Clone(rows[max(0, len(rows)-simRows):])
		if len(rows) > len(visible) {
			s.scrolled = true
			visible[0] = ljust(visible[0], s.width) + "↑"
		}
	case s.clip != nil && s.clip(s.body) > 0:
		rs := []rune(s.body)
		visible = []string{string(rs[max(0, len(rs)-s.clip(s.body)):])}
	default:
		visible = []string{s.body}
	}
	return cr(strings.Join(visible, "\n"), runeLen(visible[len(visible)-1])+2)
}

func (s *composerSim) State(context.Context) (*ComposerReading, error) {
	immediate := s.lastWasState && s.clock.sleeps == s.sleepsAtState
	s.lastWasState, s.sleepsAtState = true, s.clock.sleeps
	if s.returned {
		s.statesAfterReturn++
	}
	r := s.reading()
	if s.onState != nil {
		return s.onState(r, immediate)
	}
	return r, nil
}

func (s *composerSim) TypeText(_ context.Context, text string) error {
	s.lastWasState = false
	s.typed = append(s.typed, text)
	if text == "\r" {
		s.returned = true
	}
	applied := text
	if s.onType != nil {
		var err error
		if applied, err = s.onType(len(s.typed), text); err != nil {
			return err
		}
	}
	for _, r := range applied {
		switch r {
		case '\r':
			s.submitted = append(s.submitted, s.body)
			s.body = ""
			if s.afterSubmit != nil {
				s.idle = *s.afterSubmit
			}
		case 0x7f:
			if rs := []rune(s.body); len(rs) > 0 {
				s.body = string(rs[:len(rs)-1])
			}
		default:
			s.body += string(r)
		}
	}
	return nil
}

func (s *composerSim) returns() int {
	n := 0
	for _, text := range s.typed {
		n += strings.Count(text, "\r")
	}
	return n
}

func ljust(s string, width int) string {
	if n := runeLen(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

var wrapChunkRE = regexp.MustCompile(` +|[^ ]+`)

// pyWrap is textwrap.wrap(text, width, break_on_hyphens=False) for text whose
// only whitespace is spaces.
func pyWrap(text string, width int) []string {
	chunks := wrapChunkRE.FindAllString(text, -1)
	var lines []string
	for len(chunks) > 0 {
		var cur []string
		curLen := 0
		if strings.TrimSpace(chunks[0]) == "" && len(lines) > 0 {
			chunks = chunks[1:]
		}
		for len(chunks) > 0 {
			if l := runeLen(chunks[0]); curLen+l <= width {
				cur = append(cur, chunks[0])
				curLen += l
				chunks = chunks[1:]
			} else {
				break
			}
		}
		if len(chunks) > 0 && runeLen(chunks[0]) > width {
			rs := []rune(chunks[0])
			spaceLeft := max(width-curLen, 0)
			cur = append(cur, string(rs[:spaceLeft]))
			chunks[0] = string(rs[spaceLeft:])
		}
		if len(cur) > 0 && strings.TrimSpace(cur[len(cur)-1]) == "" {
			cur = cur[:len(cur)-1]
		}
		if len(cur) > 0 {
			lines = append(lines, strings.Join(cur, ""))
		}
	}
	return lines
}

// ---- assertions ----

func assertErrMatches(t *testing.T, err error, pattern string) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want one matching %q", pattern)
	}
	if !regexp.MustCompile(pattern).MatchString(err.Error()) {
		t.Fatalf("err = %q, want one matching %q", err, pattern)
	}
}

func assertDirty(t *testing.T, err error, pattern string) *ErrDirty {
	t.Helper()
	var d *ErrDirty
	if !errors.As(err, &d) {
		t.Fatalf("err = %v (%T), want *ErrDirty", err, err)
	}
	assertErrMatches(t, err, pattern)
	return d
}

func assertReading(t *testing.T, got, want *ComposerReading) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reading = %s, want %s", fmtReading(got), fmtReading(want))
	}
}

func fmtReading(r *ComposerReading) string {
	if r == nil {
		return "None"
	}
	return fmt.Sprintf("(%q, %d)", r.Content, r.Cursor)
}

func assertTyped(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("type_text calls = %q, want %q", got, want)
	}
}

// deleteSizes is [len(call.args[2]) for call in type_text.call_args_list] for
// calls that must all be backspace batches.
func deleteSizes(t *testing.T, typed []string) []int {
	t.Helper()
	var sizes []int
	for _, text := range typed {
		if strings.Trim(text, del) != "" {
			t.Fatalf("type_text(%q) is not a backspace batch", text)
		}
		sizes = append(sizes, runeLen(text))
	}
	return sizes
}

func assertInts(t *testing.T, what string, got []int, want ...int) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func assertNoReturn(t *testing.T, typed []string) {
	t.Helper()
	for _, text := range typed {
		if strings.Contains(text, "\r") || strings.Contains(text, "\n") {
			t.Fatalf("a submit key was typed: %q", typed)
		}
	}
}

func claudeScreen(content string) string { return rule + "\n❯ " + content + "\n" + rule }

func codexScreen(content string) string { return content + "\n" + codexFooter }

// ---- SendPreflightTests ----

// Not ported: test_control_character_is_rejected_before_any_pane_write (no
// normalize; lines arrive sanitised); the queued "\t" half of
// test_codex_default_and_queued_submit_keys (no queue mode); and the body-phase
// interrupts (test_interrupt_after_a_confirmed_chunk_cleans_composer,
// test_interrupt_during_marked_tail_check_cleans_marker,
// test_interrupt_during_unmarked_tail_check_cleans_body,
// test_raw_body_transaction_interrupt_uses_guarded_cleanup): Python cleans the
// composer after a KeyboardInterrupt, while a cancelled ctx also cancels Go's
// cleanup, and the tail checks have no cancellation point.
func TestSendPreflight(t *testing.T) {
	ctx := context.Background()
	classify := func(t *testing.T, foreground, screen string, cursor int, want State, reason string) Composer {
		t.Helper()
		c := Classify(Inputs{Foreground: []string{foreground}, Screen: screen, Cursor: cursor})
		if c.State != want || (reason != "" && c.Reason != reason) {
			t.Fatalf("Classify = %+v, want state %s reason %q", c, want, reason)
		}
		return c
	}
	initialClaude := ComposerReading{Content: "", Cursor: EmptyCursorColumn}
	initialCodex := ComposerReading{Content: CodexEmptyPrompt, Cursor: EmptyCursorColumn}
	newSim := func(t *testing.T, agent Agent) (*composerSim, *Transaction) {
		clock := steppingTime(t)
		sim := &composerSim{t: t, clock: clock}
		if agent == Codex {
			sim.idle = CodexEmptyPrompt
		}
		return sim, &Transaction{Pane: sim, Agent: agent, Clock: clock}
	}
	// burstLoss: the TUI drops the tail of the first body event (the marker's
	// last bytes), so the chunk never confirms but what arrived is owned text.
	burstLoss := func(sim *composerSim, lost int) {
		sim.onType = func(n int, text string) (string, error) {
			if n == 1 {
				rs := []rune(text)
				return string(rs[:len(rs)-lost]), nil
			}
			return text, nil
		}
	}

	runCases(t, []namedCase{
		{"test_prompt_is_checked_before_cursor", func(t *testing.T) {
			// A shell-mode row is no composer whatever the cursor says.
			for _, cursor := range []int{2, 3} {
				classify(t, "codex", codexScreen("! ls -la"), cursor, Unknown, "composer not recognisable")
			}
		}},
		{"test_cursor_refusal_writes_nothing", func(t *testing.T) {
			classify(t, "codex", codexScreen("» Ask Codex to do anything"), 3, Draft, "cursor past the prompt")
		}},
		{"test_full_width_codex_draft_at_column_two_is_refused", func(t *testing.T) {
			classify(t, "codex", codexScreen("» "+strings.Repeat("x", 83)), 2, Draft, "text in the composer")
		}},
		{"test_claude_suggestion_at_column_two_allows_send", func(t *testing.T) {
			line := "Chat from Codex: ping"
			marker := ComposerProbeMarker(line)
			for i, suggestion := range []string{
				"Add a regression test for the parser",
				"Add a regression test\nfor the parser",
			} {
				t.Run(fmt.Sprint(i), func(t *testing.T) {
					c := classify(t, "claude", claudeScreen(suggestion), 2, Ready, "")
					sim, tx := newSim(t, Claude)
					sim.idle = c.Content
					cleared := ""
					sim.afterSubmit = &cleared
					if err := tx.Send(ctx, line, ComposerReading{Content: c.Content, Cursor: c.Cursor}); err != nil {
						t.Fatalf("Send: %v", err)
					}
					assertTyped(t, sim.typed, line+marker, strings.Repeat(del, runeLen(marker)), "\r")
				})
			}
		}},
		{"test_claude_text_past_column_two_is_refused", func(t *testing.T) {
			classify(t, "claude", claudeScreen("Add a regression test for the parser"), 3, Draft, "cursor past the prompt")
		}},
		{"test_codex_free_form_text_at_column_two_is_refused", func(t *testing.T) {
			classify(t, "codex", codexScreen("» Add a regression test for the parser"), 2, Draft, "text in the composer")
		}},
		{"test_preflight_transport_failure_says_nothing_was_typed", func(t *testing.T) {
			ctl := &preflightCtl{textErr: errors.New("malformed JSON")}
			_, _, err := Observe(ctx, ctl, Session{ID: "session-id", Foreground: []string{"claude"}})
			assertErrMatches(t, err, "malformed JSON")
			if ctl.cursorCalls != 0 || ctl.typed != 0 {
				t.Fatalf("cursor reads %d, writes %d after a failed screen read; want none", ctl.cursorCalls, ctl.typed)
			}
		}},
		{"test_long_message_then_return_are_separate_writes", func(t *testing.T) {
			message := "opening " + strings.Repeat("body ", 4_000) + "closing"
			line := "Chat from Codex: " + strings.Join(strings.Fields(message), " ")
			sim, tx := newSim(t, Claude)
			if err := tx.Send(ctx, line, initialClaude); err != nil {
				t.Fatalf("Send: %v", err)
			}
			last := len(sim.typed) - 1
			assertNoReturn(t, sim.typed[:last])
			if sim.typed[last] != "\r" {
				t.Fatalf("last write = %q, want a lone Return", sim.typed[last])
			}
			if !slices.Equal(sim.submitted, []string{line}) {
				t.Fatalf("submitted %d messages, want the line once", len(sim.submitted))
			}
		}},
		{"test_codex_default_and_queued_submit_keys", func(t *testing.T) {
			// Default key only: Go submits with "\r" and has no queue ("\t") mode.
			line := "Chat from Claude: steer"
			sim, tx := newSim(t, Codex)
			if err := tx.Send(ctx, line, initialCodex); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if got := sim.typed[len(sim.typed)-1]; got != "\r" {
				t.Fatalf("submit key = %q, want \"\\r\"", got)
			}
			if !slices.Equal(sim.submitted, []string{line}) {
				t.Fatalf("submitted = %q, want [%q]", sim.submitted, line)
			}
		}},
		{"test_interrupt_during_submit_reports_ambiguous_delivery", func(t *testing.T) {
			// KeyboardInterrupt → the context is cancelled while Return is written.
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			sim, tx := newSim(t, Claude)
			sim.onType = func(_ int, text string) (string, error) {
				if text == "\r" {
					cancel()
					return "", ctx.Err()
				}
				return text, nil
			}
			err := tx.Send(ctx, "Chat from Codex: body", initialClaude)
			assertErrMatches(t, err, "submit request.*delivery is ambiguous.*do not resend")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want it to wrap context.Canceled", err)
			}
			if sim.statesAfterReturn != 0 {
				t.Fatalf("wait_for_accepted ran (%d reads) after a failed Return", sim.statesAfterReturn)
			}
		}},
		{"test_submit_value_error_reports_ambiguous_delivery", func(t *testing.T) {
			sim, tx := newSim(t, Claude)
			sim.onType = func(_ int, text string) (string, error) {
				if text == "\r" {
					return "", errors.New("malformed JSON")
				}
				return text, nil
			}
			err := tx.Send(ctx, "Chat from Codex: body", initialClaude)
			assertErrMatches(t, err, "submit request failed.*delivery is ambiguous.*do not resend.*malformed JSON")
			if sim.statesAfterReturn != 0 {
				t.Fatalf("wait_for_accepted ran (%d reads) after a failed Return", sim.statesAfterReturn)
			}
		}},
		{"test_interrupt_during_acceptance_reports_ambiguous_delivery", func(t *testing.T) {
			// KeyboardInterrupt → the context is cancelled while acceptance is awaited.
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			sim, tx := newSim(t, Claude)
			sim.onState = func(r *ComposerReading, _ bool) (*ComposerReading, error) {
				if sim.returned {
					cancel()
					return nil, ctx.Err()
				}
				return r, nil
			}
			err := tx.Send(ctx, "Chat from Codex: body", initialClaude)
			assertErrMatches(t, err, "submission confirmation.*delivery is ambiguous.*do not resend")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want it to wrap context.Canceled", err)
			}
		}},
		{"test_acceptance_probe_failure_reports_ambiguous_delivery", func(t *testing.T) {
			sim, tx := newSim(t, Claude)
			sim.onState = func(r *ComposerReading, _ bool) (*ComposerReading, error) {
				if sim.returned {
					return nil, errors.New("malformed JSON")
				}
				return r, nil
			}
			err := tx.Send(ctx, "Chat from Codex: body", initialClaude)
			assertErrMatches(t, err, "submission confirmation failed.*delivery is ambiguous.*do not resend.*malformed JSON")
		}},
		{"test_unconfirmed_chunk_withholds_submit", func(t *testing.T) {
			line := "Chat from Codex: body"
			marked := line + ComposerProbeMarker(line)
			sim, tx := newSim(t, Claude)
			burstLoss(sim, 6) // "Chat from Codex: body [peer-ch" arrives
			err := tx.Send(ctx, line, initialClaude)
			d := assertDirty(t, err, "submit withheld; composer cleared$")
			if d.Owned != marked || !d.Cleared {
				t.Fatalf("ErrDirty owned %q cleared %v, want owned %q cleared", d.Owned, d.Cleared, marked)
			}
			assertNoReturn(t, sim.typed)
			assertInts(t, "cleanup batches", deleteSizes(t, sim.typed[1:]), runeLen(marked)-6)
			if sim.body != "" {
				t.Fatalf("composer left holding %q", sim.body)
			}
		}},
		{"test_transport_failure_after_a_confirmed_chunk_cleans_composer", func(t *testing.T) {
			message := strings.Repeat("a", TypeChunkBytes*2)
			line := "Chat from Codex: " + message
			marker := ComposerProbeMarker(line)
			chunks := TextChunks(line, TypeChunkBytes-len(marker))
			sim, tx := newSim(t, Claude)
			sim.onType = func(n int, text string) (string, error) {
				if n == 3 {
					return "", errors.New("transport failed")
				}
				return text, nil
			}
			err := tx.Send(ctx, line, initialClaude)
			d := assertDirty(t, err, "transport failed.*composer cleared")
			if want := chunks[0] + chunks[1] + marker; d.Owned != want {
				t.Fatalf("owned = %q, want %q", d.Owned, want)
			}
			// type_text.call_count == 3 in Python, where clear_composer is mocked;
			// here the backspaces of the real cleanup follow.
			assertTyped(t, sim.typed[:3], chunks[0]+marker, strings.Repeat(del, runeLen(marker)), chunks[1]+marker)
			assertInts(t, "cleanup batches", deleteSizes(t, sim.typed[3:]), runeLen(chunks[0]))
			if sim.body != "" {
				t.Fatalf("composer left holding %q", sim.body)
			}
		}},
		{"test_interrupt_during_cleanup_preserves_partial_body_warning", func(t *testing.T) {
			// KeyboardInterrupt in clear_composer → the context is cancelled as the
			// cleanup writes its first backspace batch. Go has no separate
			// "interrupted" wording: the warning keeps the dirty message and
			// reports the cleanup as failed.
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			line := "Chat from Codex: partial body"
			sim, tx := newSim(t, Claude)
			sim.onType = func(n int, text string) (string, error) {
				if n == 1 {
					return text[:len(text)-6], nil
				}
				cancel()
				return "", ctx.Err()
			}
			err := tx.Send(ctx, line, initialClaude)
			d := assertDirty(t, err, "^message chunk 1/1 .*; submit withheld; composer cleanup failed$")
			if d.Cleared || d.Owned != line+ComposerProbeMarker(line) {
				t.Fatalf("ErrDirty owned %q cleared %v", d.Owned, d.Cleared)
			}
			assertNoReturn(t, sim.typed)
		}},
		{"test_cleanup_value_error_reports_failure_without_submitting", func(t *testing.T) {
			sim, tx := newSim(t, Claude)
			sim.onType = func(n int, text string) (string, error) {
				if n == 1 {
					return text[:len(text)-6], nil
				}
				return "", errors.New("malformed JSON")
			}
			err := tx.Send(ctx, "Chat from Codex: partial body", initialClaude)
			_ = assertDirty(t, err, "submit withheld; composer cleanup failed$")
			assertNoReturn(t, sim.typed)
		}},
		{"test_missing_middle_withholds_submit", func(t *testing.T) {
			// Python mocks type_body raising "chunk 2/3 is incomplete"; here the
			// TUI loses the second of three body events. A real waiter reports it
			// as unconfirmed (Python's real wait_for_composer_change does the same).
			var words []string
			for i := range 45 {
				words = append(words, fmt.Sprintf("word%03d", i))
			}
			line := "Chat from Codex: opening " + strings.Join(words, " ") + " closing"
			chunks := TextChunks(line, TypeChunkBytes-len(ComposerProbeMarker(line)))
			if len(chunks) != 3 {
				t.Fatalf("fixture has %d chunks, want 3", len(chunks))
			}
			sim, tx := newSim(t, Claude)
			sim.onType = func(n int, text string) (string, error) {
				if n == 3 {
					return "", nil
				}
				return text, nil
			}
			err := tx.Send(ctx, line, initialClaude)
			_ = assertDirty(t, err, "^message chunk 2/3 .*; submit withheld; composer cleared$")
			assertNoReturn(t, sim.typed)
			if sim.body != "" {
				t.Fatalf("composer left holding %q", sim.body)
			}
		}},
		{"test_unaccepted_submit_is_reported", func(t *testing.T) {
			line := "Chat from Codex: body"
			// The composer keeps the line after Return (read as a newline): Go
			// repeats Return SubmitAttempts times in all, then reports ambiguity.
			sim, tx := newSim(t, Claude)
			sim.onType = func(_ int, text string) (string, error) {
				if text == "\r" {
					return "", nil
				}
				return text, nil
			}
			err := tx.Send(ctx, line, initialClaude)
			if !errors.Is(err, ErrAmbiguous) {
				t.Fatalf("err = %v, want ErrAmbiguous", err)
			}
			assertErrMatches(t, err, "did not confirm submission.*delivery is ambiguous.*do not resend")
			if got := sim.returns(); got != SubmitAttempts {
				t.Fatalf("Return typed %d times, want %d", got, SubmitAttempts)
			}
			// Anything but the line after Return: no second Return.
			sim, tx = newSim(t, Claude)
			sim.onState = func(r *ComposerReading, _ bool) (*ComposerReading, error) {
				if sim.returned {
					return nil, nil
				}
				return r, nil
			}
			err = tx.Send(ctx, line, initialClaude)
			if !errors.Is(err, ErrAmbiguous) {
				t.Fatalf("err = %v, want ErrAmbiguous", err)
			}
			if got := sim.returns(); got != 1 {
				t.Fatalf("Return typed %d times, want 1", got)
			}
		}},
		{"test_clipped_composer_before_submit_still_submits", func(t *testing.T) {
			var words []string
			for i := range 120 {
				words = append(words, fmt.Sprintf("word%d", i))
			}
			line := "Chat from Codex: " + strings.Join(words, " ")
			sim, tx := newSim(t, Claude)
			sim.clip = func(string) int { return 300 }
			if err := tx.Send(ctx, line, initialClaude); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if sim.returns() != 1 || !slices.Equal(sim.submitted, []string{line}) {
				t.Fatalf("Return typed %d times, submitted %d messages; want the line once", sim.returns(), len(sim.submitted))
			}
		}},
		{"test_clipped_cyrillic_composer_needs_only_the_final_chunk", func(t *testing.T) {
			var words []string
			for i := range 90 {
				words = append(words, fmt.Sprintf("слово%d", i))
			}
			line := "Chat from Claude: " + strings.Join(words, " ")
			// Python mocks type_body and composer_state to return line[-150:]. A
			// marked chunk needs more than 150 runes to verify, so the editor shows
			// the whole body while typing and clips to the last 150 runes once it
			// holds exactly the line: the final unmarked check and the pre-submit
			// check then see Python's reading.
			sim, tx := newSim(t, Codex)
			sim.clip = func(body string) int {
				if body == line {
					return 150
				}
				return 0
			}
			if err := tx.Send(ctx, line, initialCodex); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if sim.returns() != 1 || !slices.Equal(sim.submitted, []string{line}) {
				t.Fatalf("Return typed %d times, submitted %d messages; want the line once", sim.returns(), len(sim.submitted))
			}
		}},
		{"test_wrong_character_revealed_before_submit_withholds_return", func(t *testing.T) {
			// A particle covers the "i" while the body is typed; the re-read before
			// Return shows "pong" under it.
			line := "Chat from Claude: ping"
			sim, tx := newSim(t, Codex)
			revealed := false
			sim.onState = func(r *ComposerReading, immediate bool) (*ComposerReading, error) {
				if !strings.Contains(r.Content, "ping") {
					return r, nil
				}
				if immediate && sim.body == line {
					revealed = true
				}
				if revealed {
					return cr(strings.Replace(r.Content, "ping", "pong", 1), r.Cursor), nil
				}
				return cr(strings.Replace(r.Content, "ping", "p\u2801ng", 1), r.Cursor), nil
			}
			err := tx.Send(ctx, line, initialCodex)
			_ = assertDirty(t, err, "changed before submit")
			assertNoReturn(t, sim.typed)
		}},
		{"test_changed_composer_before_submit_withholds_return", func(t *testing.T) {
			line := "Chat from Codex: body"
			sim, tx := newSim(t, Claude)
			gone := false
			sim.onState = func(r *ComposerReading, immediate bool) (*ComposerReading, error) {
				if immediate && sim.body == line {
					gone = true
				}
				if gone {
					return nil, nil
				}
				return r, nil
			}
			err := tx.Send(ctx, line, initialClaude)
			_ = assertDirty(t, err, "composer changed before submit; submit withheld; composer cleanup failed")
			assertNoReturn(t, sim.typed)
		}},
	})
}

// preflightCtl is an agtermctl whose screen read fails.
type preflightCtl struct {
	textErr     error
	cursorCalls int
	typed       int
}

func (c *preflightCtl) Sessions(context.Context) ([]Session, error) { return nil, nil }
func (c *preflightCtl) Text(context.Context, Session, int) (string, error) {
	return "", c.textErr
}
func (c *preflightCtl) Cursor(context.Context, Session) (int, error) {
	c.cursorCalls++
	return EmptyCursorColumn, nil
}
func (c *preflightCtl) Type(context.Context, Session, string) error {
	c.typed++
	return nil
}
func (c *preflightCtl) NewSession(context.Context, NewSessionOpts) (string, error) { return "", nil }
func (c *preflightCtl) Close(context.Context, Session) error                       { return nil }

// ---- ComposerTransitionTests ----

func TestComposerTransition(t *testing.T) {
	ctx := context.Background()
	runCases(t, []namedCase{
		{"test_unchanged_composer_through_deadline_rejects_the_event", func(t *testing.T) {
			pane := &scriptPane{t: t, state: returnValue(cr("same body", 18))}
			tx := &Transaction{Pane: pane, Agent: Codex, Clock: monotonicScript(t, 0.0, 0.1, 3.1)}
			got, err := tx.waitForComposerChange(ctx, cr("same body", 18), ChunkSettleDelay, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertReading(t, got, nil)
			if pane.calls != 2 {
				t.Fatalf("composer_state calls = %d, want 2", pane.calls)
			}
		}},
		{"test_change_waits_for_the_composer_only_to_settle", func(t *testing.T) {
			if ComposerSettleDelay < 500*time.Millisecond {
				t.Fatalf("ComposerSettleDelay = %v, want >= 500ms", ComposerSettleDelay)
			}
			pane := &scriptPane{t: t}
			pane.state = sideEffect(t, cr("part 1", 2), cr("part 2", 2), cr("part 2", 2), cr("part 2", 2))
			clock := monotonicScript(t, 0.0, 0.1, 0.4, 0.7, 0.9)
			tx := &Transaction{Pane: pane, Agent: Codex, Clock: clock}
			got, err := tx.waitForComposerChange(ctx, cr("part 1", 2), ComposerSettleDelay, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertReading(t, got, cr("part 2", 2))
			if pane.calls != 4 || clock.sleeps != 3 {
				t.Fatalf("composer_state calls = %d, sleeps = %d; want 4, 3", pane.calls, clock.sleeps)
			}
		}},
		{"test_accepted_requires_changed_state_and_empty_cursor", func(t *testing.T) {
			pane := &scriptPane{t: t}
			pane.state = sideEffect(t, cr("reflowed body", 2), cr(CodexEmptyPrompt, 2), cr(CodexEmptyPrompt, 2))
			tx := &Transaction{Pane: pane, Agent: Codex, Clock: steppingTime(t)}
			ok, err := tx.waitForAccepted(ctx, ComposerReading{Content: "body", Cursor: 18})
			if err != nil || !ok {
				t.Fatalf("waitForAccepted = %v, %v; want true", ok, err)
			}
			if pane.calls != 3 {
				t.Fatalf("composer_state calls = %d, want 3", pane.calls)
			}
		}},
		{"test_reflowed_unsent_body_at_column_two_is_not_accepted", func(t *testing.T) {
			pane := &scriptPane{t: t, state: returnValue(cr("reflowed unsent body", 2))}
			tx := &Transaction{Pane: pane, Agent: Codex, Clock: monotonicScript(t, 0.0, 3.1)}
			ok, err := tx.waitForAccepted(ctx, ComposerReading{Content: "original unsent body", Cursor: 27})
			if err != nil || ok {
				t.Fatalf("waitForAccepted = %v, %v; want false", ok, err)
			}
		}},
		{"test_cursor_reset_with_the_same_body_is_not_accepted", func(t *testing.T) {
			pane := &scriptPane{t: t, state: returnValue(cr("same body", 2))}
			tx := &Transaction{Pane: pane, Agent: Codex, Clock: monotonicScript(t, 0.0, 3.1)}
			ok, err := tx.waitForAccepted(ctx, ComposerReading{Content: "same body", Cursor: 18})
			if err != nil || ok {
				t.Fatalf("waitForAccepted = %v, %v; want false", ok, err)
			}
		}},
		{"test_queued_placeholder_counts_as_accepted", func(t *testing.T) {
			pane := &scriptPane{t: t, state: returnValue(cr("Press up to edit queued messages", 2))}
			tx := &Transaction{Pane: pane, Agent: Claude, Clock: steppingTime(t)}
			ok, err := tx.waitForAccepted(ctx, ComposerReading{Content: "Chat from Codex: body", Cursor: 25})
			if err != nil || !ok {
				t.Fatalf("waitForAccepted = %v, %v; want true", ok, err)
			}
		}},
	})
}

// ---- TypeBodyThroughParticlesTests ----

func TestTypeBodyThroughParticles(t *testing.T) {
	ctx := context.Background()
	runCases(t, []namedCase{
		{"test_codex_chunk_verifies_and_unmarks_under_particles", func(t *testing.T) {
			marker := ComposerProbeMarker("ping")
			typed := "Chat from Claude: ping"
			markedSeen := "Chat from Cl\u2801ude: ping" + marker
			unmarkedSeen := "Chat from Claude: p\u2802ng"
			pane := &scriptPane{t: t}
			pane.state = afterTypes(pane, cr("", 2), cr(markedSeen, 40), cr(unmarkedSeen, 23))
			tx := &Transaction{Pane: pane, Agent: Codex, Clock: steppingTime(t)}
			owned := ""
			got, err := tx.typeBody(ctx, typed, ComposerReading{Content: "", Cursor: 2}, &owned)
			if err != nil {
				t.Fatalf("typeBody: %v", err)
			}
			assertReading(t, &got, cr(unmarkedSeen, 23))
		}},
		{"test_claude_chunk_keeps_strict_verification", func(t *testing.T) {
			// Python's mocked waiter hands type_body this reading and the tail check
			// calls it incomplete; the real waiter applies that check as its
			// predicate, so the chunk is never confirmed (verified: Python's real
			// wait_for_composer_change reports the same).
			marker := ComposerProbeMarker("ping")
			typed := "Chat from Codex: ping"
			seen := "Chat from C\u2801dex: ping" + marker
			if ComposerHasExpectedTail(seen, typed+marker, typed+marker, false) {
				t.Fatalf("Claude reading %q verified through a particle", seen)
			}
			pane := &scriptPane{t: t}
			pane.state = afterTypes(pane, cr("", 2), cr(seen, 40))
			tx := &Transaction{Pane: pane, Agent: Claude, Clock: steppingTime(t)}
			owned := ""
			_, err := tx.typeBody(ctx, typed, ComposerReading{Content: "", Cursor: 2}, &owned)
			_ = assertDirty(t, err, "message chunk 1/1 was typed but the target composer did not confirm it")
		}},
	})
}

// ---- IdleAnimationStabilityTests ----

func TestIdleAnimationStability(t *testing.T) {
	ctx := context.Background()
	const owned = "Chat from Claude: ping"
	frames := []*ComposerReading{
		cr("Chat from Cl\u2801ude: ping", 23),
		cr("Chat from Claude: p\u2802ng", 23),
		cr("Chat fr\u2804m Claude: ping", 23),
		cr("Chat from Claude: ping\u2808", 23),
	}
	matchesOwned := func(content string) bool {
		return ComposerHasExpectedTail(content, owned, owned, true)
	}
	runCases(t, []namedCase{
		{"test_matching_change_settles_through_moving_particles", func(t *testing.T) {
			pane := &scriptPane{t: t, state: sideEffect(t, frames...)}
			tx := &Transaction{Pane: pane, Agent: Codex, Clock: steppingTime(t)}
			got, err := tx.waitForComposerChange(ctx, cr("", 2), ChunkSettleDelay, matchesOwned)
			if err != nil {
				t.Fatal(err)
			}
			assertReading(t, got, frames[1])
		}},
		{"test_matching_change_does_not_settle_on_a_wrong_body", func(t *testing.T) {
			pane := &scriptPane{t: t, state: returnValue(cr("Chat from Claude: pinf", 23))}
			tx := &Transaction{Pane: pane, Agent: Codex, Clock: monotonicScript(t, 0.0, 0.1, 3.1)}
			got, err := tx.waitForComposerChange(ctx, cr("", 2), ChunkSettleDelay, matchesOwned)
			if err != nil {
				t.Fatal(err)
			}
			assertReading(t, got, nil)
		}},
		{"test_unchanged_text_under_moving_particles_is_no_change", func(t *testing.T) {
			pane := &scriptPane{t: t, state: sideEffect(t, frames...)}
			tx := &Transaction{Pane: pane, Agent: Codex, Clock: monotonicScript(t, 0.0, 0.1, 0.2, 0.3, 3.1)}
			got, err := tx.waitForComposerChange(ctx, frames[0], ChunkSettleDelay, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertReading(t, got, nil)
		}},
		{"test_acceptance_settles_through_particles_over_the_placeholder", func(t *testing.T) {
			pane := &scriptPane{t: t}
			pane.state = sideEffect(t,
				cr("Ask Codex to do\u2801anything", 2),
				cr("Ask\u2802Codex to do anything", 2),
				cr("Ask Codex to do anything \u2804", 2),
			)
			tx := &Transaction{Pane: pane, Agent: Codex, Clock: steppingTime(t)}
			ok, err := tx.waitForAccepted(ctx, ComposerReading{Content: owned, Cursor: 23})
			if err != nil || !ok {
				t.Fatalf("waitForAccepted = %v, %v; want true", ok, err)
			}
			if pane.calls != 2 {
				t.Fatalf("composer_state calls = %d, want 2", pane.calls)
			}
		}},
		{"test_cleanup_state_settles_on_owned_spans_through_particles", func(t *testing.T) {
			pane := &scriptPane{t: t}
			pane.state = sideEffect(t,
				cr("Chat from Claude: p\u2802ng", 23),
				cr("Chat from Cl\u2801ude: p", 20),
				cr("Chat fr\u2804m Claude: p", 20),
				cr("Chat from Claude: p", 20),
			)
			tx := &Transaction{Pane: pane, Agent: Codex, Clock: steppingTime(t)}
			got, err := tx.waitForCleanupState(ctx, frames[0], owned, ends(19, 20, 21), true)
			if err != nil || got == nil {
				t.Fatalf("waitForCleanupState = %v, %v", got, err)
			}
			assertReading(t, got.state, cr("Chat fr\u2804m Claude: p", 20))
			assertSpans(t, got.spans, Span{0, 19})
			if pane.calls != 3 {
				t.Fatalf("composer_state calls = %d, want 3", pane.calls)
			}
		}},
	})
}

// ---- ComposerCleanupTests ----

func TestComposerCleanup(t *testing.T) {
	ctx := context.Background()
	empty := cr("", 2)
	initial := ComposerReading{Content: "", Cursor: 2}
	words := func(format string, n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf(format, i)
		}
		return strings.Join(parts, " ")
	}
	runClear := func(t *testing.T, pane *scriptPane, clock Clock, owned string, want bool) {
		t.Helper()
		tx := &Transaction{Pane: pane, Agent: Claude, Clock: clock}
		got, err := tx.clearComposer(ctx, initial, owned)
		if err != nil || got != want {
			t.Fatalf("clearComposer = %v, %v; want %v", got, err, want)
		}
	}
	dropRunes := func(s string, n int) string {
		rs := []rune(s)
		return string(rs[:len(rs)-n])
	}
	runCases(t, []namedCase{
		{"test_clear_does_not_claim_an_unobserved_body_was_removed", func(t *testing.T) {
			pane := &scriptPane{t: t, state: returnValue(empty)}
			runClear(t, pane, monotonicScript(t, 0.0, 3.1), "queued body", false)
			assertTyped(t, pane.typed)
		}},
		{"test_clear_waits_for_body_after_the_initial_empty_state", func(t *testing.T) {
			owned := "delayed body"
			pane := &scriptPane{t: t}
			pane.state = sideEffect(t, empty, cr(owned, 14), cr(owned, 14), empty)
			runClear(t, pane, steppingTime(t), owned, true)
			assertTyped(t, pane.typed, strings.Repeat(del, runeLen(owned)))
		}},
		{"test_clear_backspaces_without_interrupting_the_active_turn", func(t *testing.T) {
			pane := &scriptPane{t: t}
			pane.state = sideEffect(t, cr("partial body", 14), cr("partial body", 14), empty)
			runClear(t, pane, steppingTime(t), "partial body", true)
			assertTyped(t, pane.typed, strings.Repeat(del, runeLen("partial body")))
		}},
		{"test_clear_does_not_repeat_unchanged_periodic_text", func(t *testing.T) {
			pane := &scriptPane{t: t}
			pane.state = sideEffect(t, append(repeatReading(cr(strings.Repeat("a", 80), 2), 10), empty)...)
			runClear(t, pane, steppingTime(t), strings.Repeat("a", 400), true)
			assertTyped(t, pane.typed, strings.Repeat(del, 80))
		}},
		{"test_clear_waits_for_a_lagging_backspace_batch", func(t *testing.T) {
			owned := words("word-%03d", 36)
			remaining := dropRunes(owned, TypeChunkBytes)
			full, partial := cr(owned, 28), cr(remaining, 17)
			pane := &scriptPane{t: t}
			pane.state = sideEffect(t, append(repeatReading(full, 12), partial, partial, empty)...)
			runClear(t, pane, steppingTime(t), owned, true)
			assertInts(t, "batches", deleteSizes(t, pane.typed), TypeChunkBytes, runeLen(remaining))
		}},
		{"test_clear_does_not_repeat_a_stalled_backspace_batch", func(t *testing.T) {
			owned := words("word-%03d", 36)
			pane := &scriptPane{t: t, state: returnValue(cr(owned, 28))}
			runClear(t, pane, steppingTime(t), owned, false)
			assertTyped(t, pane.typed, strings.Repeat(del, TypeChunkBytes))
		}},
		{"test_clear_ignores_one_torn_unowned_sample", func(t *testing.T) {
			owned := words("word-%03d", 36)
			remaining := dropRunes(owned, TypeChunkBytes)
			full, torn, partial := cr(owned, 28), cr(owned, 17), cr(remaining, 17)
			pane := &scriptPane{t: t}
			pane.state = sideEffect(t, full, full, torn, partial, partial, empty)
			runClear(t, pane, steppingTime(t), owned, true)
			assertInts(t, "batches", deleteSizes(t, pane.typed), TypeChunkBytes, runeLen(remaining))
		}},
		{"test_clear_ignores_one_unrecognised_sample", func(t *testing.T) {
			owned := words("word-%03d", 36)
			remaining := dropRunes(owned, TypeChunkBytes)
			full, partial := cr(owned, 28), cr(remaining, 17)
			pane := &scriptPane{t: t}
			pane.state = sideEffect(t, full, full, nil, partial, partial, empty)
			runClear(t, pane, steppingTime(t), owned, true)
			assertInts(t, "batches", deleteSizes(t, pane.typed), TypeChunkBytes, runeLen(remaining))
		}},
		{"test_clear_ignores_an_initial_unrecognised_sample", func(t *testing.T) {
			owned := "partial body"
			pane := &scriptPane{t: t}
			pane.state = sideEffect(t, nil, cr(owned, 14), cr(owned, 14), empty)
			runClear(t, pane, steppingTime(t), owned, true)
			assertTyped(t, pane.typed, strings.Repeat(del, runeLen(owned)))
		}},
		{"test_clear_continues_from_a_partly_applied_batch", func(t *testing.T) {
			owned := words("word-%03d", 36)
			afterPartial := dropRunes(owned, 120)
			afterSecond := dropRunes(afterPartial, TypeChunkBytes)
			pane := &scriptPane{t: t}
			pane.state = sideEffect(t,
				cr(owned, 28), cr(owned, 28),
				cr(afterPartial, 19), cr(afterPartial, 19),
				cr(afterSecond, 8), cr(afterSecond, 8),
				empty,
			)
			runClear(t, pane, steppingTime(t), owned, true)
			assertInts(t, "batches", deleteSizes(t, pane.typed), TypeChunkBytes, TypeChunkBytes, runeLen(afterSecond))
		}},
		{"test_clear_owns_a_distinct_scrolled_suffix", func(t *testing.T) {
			owned := words("token-%03d", 90)
			rs := []rune(owned)
			var states []*ComposerReading
			var want []int
			for end := len(rs); end > 0; end -= TypeChunkBytes {
				r := cr(string(rs[max(0, end-240):end]), 2)
				states = append(states, r, r)
				want = append(want, min(TypeChunkBytes, end))
			}
			states = append(states, empty)
			pane := &scriptPane{t: t, state: sideEffect(t, states...)}
			runClear(t, pane, steppingTime(t), owned, true)
			assertInts(t, "batches", deleteSizes(t, pane.typed), want...)
		}},
		{"test_clear_continues_while_small_batches_make_progress", func(t *testing.T) {
			var b strings.Builder
			for i := range 250 {
				fmt.Fprintf(&b, "%04d", i)
			}
			owned := b.String()
			remaining := len(owned)
			pane := &scriptPane{t: t}
			pane.state = func(int) (*ComposerReading, error) {
				if remaining == 0 {
					return empty, nil
				}
				return cr(owned[max(0, remaining-10):remaining], 11), nil
			}
			pane.onType = func(text string) error {
				if strings.Trim(text, del) != "" {
					t.Errorf("type_text(%q) is not a backspace batch", text)
				}
				remaining -= len(text)
				return nil
			}
			runClear(t, pane, steppingTime(t), owned, true)
			if remaining != 0 || len(pane.typed) != len(owned)/10 {
				t.Fatalf("remaining %d after %d writes; want 0 after %d", remaining, len(pane.typed), len(owned)/10)
			}
		}},
		{"test_clear_accepts_a_parser_elided_trailing_space", func(t *testing.T) {
			remaining := "alpha beta gamma "
			owned := remaining + strings.Repeat("x", TypeChunkBytes)
			full := cr(mustLive(t, Claude, claudeScreen(owned)), 24)
			trimmed := cr(mustLive(t, Claude, claudeScreen(remaining)), 18)
			pane := &scriptPane{t: t, state: sideEffect(t, full, full, trimmed, trimmed, empty)}
			runClear(t, pane, steppingTime(t), owned, true)
			assertInts(t, "batches", deleteSizes(t, pane.typed), TypeChunkBytes, runeLen(remaining))
		}},
		{"test_clear_stops_when_foreign_text_appears_after_one_batch", func(t *testing.T) {
			pane := &scriptPane{t: t}
			pane.state = sideEffect(t,
				cr("delta epsilon", 14), cr("delta epsilon", 14),
				cr("alpha beta USER", 16), cr("alpha beta USER", 16),
			)
			runClear(t, pane, steppingTime(t), "alpha beta gamma delta epsilon", false)
			assertTyped(t, pane.typed, strings.Repeat(del, runeLen("delta epsilon")))
		}},
		{"test_clear_accepts_a_partially_written_marker_prefix", func(t *testing.T) {
			partial := "payload [peer-ch"
			pane := &scriptPane{t: t, state: sideEffect(t, cr(partial, 17), cr(partial, 17), empty)}
			runClear(t, pane, steppingTime(t), "payload [peer-check:0]", true)
			assertTyped(t, pane.typed, strings.Repeat(del, runeLen(partial)))
		}},
		{"test_unrecognised_composer_is_never_backspaced", func(t *testing.T) {
			pane := &scriptPane{t: t, state: returnValue(nil)}
			runClear(t, pane, steppingTime(t), strings.Repeat("a", 400), false)
			assertTyped(t, pane.typed)
		}},
		{"test_foreign_suffix_is_never_backspaced", func(t *testing.T) {
			pane := &scriptPane{t: t, state: returnValue(cr("script prefix USER DRAFT", 27))}
			runClear(t, pane, steppingTime(t), "script prefix", false)
			assertTyped(t, pane.typed)
		}},
	})
}

// ---- TypeBodyTests (the cases that mock wait_for_composer_change) ----

func TestTypeBodyTransitions(t *testing.T) {
	ctx := context.Background()
	marker := ComposerProbeMarker("body")
	chunkLimit := TypeChunkBytes - len(marker)
	unmark := strings.Repeat(del, runeLen(marker))
	initial := ComposerReading{Content: "", Cursor: 2}
	runCases(t, []namedCase{
		{"test_each_chunk_waits_for_an_observed_composer_transition", func(t *testing.T) {
			message := strings.Repeat("a", chunkLimit+1)
			chunks := TextChunks(message, chunkLimit)
			pane := &scriptPane{t: t}
			pane.state = afterTypes(pane,
				cr("", 2),
				cr(chunks[0]+marker, 2),
				cr(chunks[0], 2),
				cr(message+marker, 3),
				cr(message, 3),
			)
			// The sleeping clock makes each wait's settle delay visible as reads:
			// 3 for ChunkSettleDelay (0, 50, 100ms), 11 for ComposerSettleDelay.
			tx := &Transaction{Pane: pane, Agent: Claude, Clock: sleepingClock(t)}
			owned := ""
			got, err := tx.typeBody(ctx, message, initial, &owned)
			if err != nil {
				t.Fatalf("typeBody: %v", err)
			}
			assertReading(t, &got, cr(message, 3))
			assertTyped(t, pane.typed, chunks[0]+marker, unmark, chunks[1]+marker, unmark)
			assertInts(t, "reads per wait", pane.statesAfter, 0, 3, 3, 3, 11)
			for _, text := range pane.typed {
				if len(text) > TypeChunkBytes {
					t.Fatalf("event of %d bytes exceeds %d", len(text), TypeChunkBytes)
				}
			}
		}},
		{"test_periodic_viewport_ambiguity_fails_before_submit", func(t *testing.T) {
			// Python's mocked waiter returns the clipped periodic reading and the
			// tail check calls chunk 2 incomplete; the real waiter never confirms it
			// (Python's real wait_for_composer_change: "chunk 2/3 was typed but the
			// target composer did not confirm it").
			message := strings.Repeat("x", chunkLimit*3)
			chunks := TextChunks(message, chunkLimit)
			visible := chunkLimit + ChunkVerifyOverlap
			readings := []*ComposerReading{cr("", 2)}
			expected := ""
			for _, chunk := range chunks {
				expected += chunk
				m := expected + marker
				readings = append(readings, cr(m[max(0, len(m)-(visible+len(marker))):], 2), cr(expected[max(0, len(expected)-visible):], 2))
			}
			if ComposerHasExpectedTail(readings[3].Content, chunks[0]+chunks[1]+marker, chunks[1]+marker, false) {
				t.Fatalf("periodic clipped reading verified")
			}
			pane := &scriptPane{t: t}
			pane.state = afterTypes(pane, readings...)
			tx := &Transaction{Pane: pane, Agent: Claude, Clock: steppingTime(t)}
			owned := ""
			_, err := tx.typeBody(ctx, message, initial, &owned)
			_ = assertDirty(t, err, "chunk 2/3 ")
			if len(pane.typed) != 3 || !strings.HasSuffix(pane.typed[2], marker) {
				t.Fatalf("type_text calls = %q, want 3 ending with the marked chunk 2", pane.typed)
			}
		}},
		{"test_missing_chunk_transition_stops_before_later_chunks", func(t *testing.T) {
			message := strings.Repeat("a", chunkLimit) + strings.Repeat("b", chunkLimit-1) + "Zc"
			chunks := TextChunks(message, chunkLimit)
			pane := &scriptPane{t: t}
			// After chunk 2 the composer shows no transition at all.
			pane.state = afterTypes(pane, cr("", 2), cr(chunks[0]+marker, 2), cr(chunks[0], 2))
			tx := &Transaction{Pane: pane, Agent: Claude, Clock: steppingTime(t)}
			owned := ""
			_, err := tx.typeBody(ctx, message, initial, &owned)
			_ = assertDirty(t, err, "chunk 2/3")
			assertTyped(t, pane.typed, chunks[0]+marker, unmark, chunks[1]+marker)
		}},
		{"test_incomplete_changed_chunk_stops_before_later_chunks", func(t *testing.T) {
			// As above: Python's "is incomplete" comes from the mocked waiter; the
			// real one leaves the chunk unconfirmed.
			message := strings.Repeat("a", chunkLimit) + strings.Repeat("b", chunkLimit-1) + "Zc"
			chunks := TextChunks(message, chunkLimit)
			incomplete := chunks[0] + chunks[1][:len(chunks[1])-1] + marker
			pane := &scriptPane{t: t}
			pane.state = afterTypes(pane, cr("", 2), cr(chunks[0]+marker, 2), cr(chunks[0], 2), cr(incomplete, 2))
			tx := &Transaction{Pane: pane, Agent: Claude, Clock: steppingTime(t)}
			owned := ""
			_, err := tx.typeBody(ctx, message, initial, &owned)
			_ = assertDirty(t, err, "chunk 2/3 ")
			if len(pane.typed) != 3 {
				t.Fatalf("type_text calls = %d, want 3", len(pane.typed))
			}
		}},
	})
}

// ---- ScrollIndicatorTests (ComposerSimulation) ----

func TestScrollIndicatorSimulation(t *testing.T) {
	ctx := context.Background()
	hidden := "Chat from Claude: Reviewed the example module and tests; "
	initial := ComposerReading{Content: CodexEmptyPrompt, Cursor: 2}
	checks := func() string {
		parts := make([]string, 90)
		for i := range parts {
			parts[i] = fmt.Sprintf("check-%03d", i)
		}
		return strings.Join(parts, " ")
	}
	newEditor := func(t *testing.T, width int) (*composerSim, *Transaction) {
		clock := steppingTime(t)
		sim := &composerSim{t: t, clock: clock, width: width, idle: CodexEmptyPrompt}
		return sim, &Transaction{Pane: sim, Agent: Codex, Clock: clock}
	}
	runCases(t, []namedCase{
		{"test_full_send_preserves_long_unicode_body_and_submits_once", func(t *testing.T) {
			parts := make([]string, 90)
			for n := range parts {
				parts[n] = fmt.Sprintf("café-%03d naïve résumé déjà-vu", n)
			}
			line := "Chat from Claude: " + strings.Join(parts, " ")
			for _, width := range []int{30, 57, 100} {
				t.Run(fmt.Sprint(width), func(t *testing.T) {
					editor, tx := newEditor(t, width)
					if err := tx.Send(ctx, line, initial); err != nil {
						t.Fatalf("Send: %v", err)
					}
					if !editor.scrolled {
						t.Fatalf("editor never scrolled")
					}
					if !slices.Equal(editor.submitted, []string{line}) || editor.body != "" {
						t.Fatalf("submitted %d messages, body %q; want the line once and an empty body", len(editor.submitted), editor.body)
					}
				})
			}
		}},
		{"test_cleanup_removes_only_owned_body_through_scrolling", func(t *testing.T) {
			editor, tx := newEditor(t, 40)
			editor.body = hidden + checks()
			cleared, err := tx.clearComposer(ctx, initial, editor.body)
			if err != nil || !cleared {
				t.Fatalf("clearComposer = %v, %v; want true", cleared, err)
			}
			if !editor.scrolled || editor.body != "" || len(editor.submitted) != 0 {
				t.Fatalf("scrolled %v, body %q, submitted %q", editor.scrolled, editor.body, editor.submitted)
			}
		}},
		{"test_cleanup_does_not_erase_foreign_suffix_in_scrolled_editor", func(t *testing.T) {
			editor, tx := newEditor(t, 40)
			owned := hidden + checks()
			editor.body = owned + " FOREIGN INPUT"
			before := editor.body
			cleared, err := tx.clearComposer(ctx, initial, owned)
			if err != nil || cleared {
				t.Fatalf("clearComposer = %v, %v; want false", cleared, err)
			}
			if editor.body != before || len(editor.submitted) != 0 {
				t.Fatalf("body %q, submitted %q; want it untouched", editor.body, editor.submitted)
			}
		}},
	})
}
