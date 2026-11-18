package agterm

// Port of peer-chat.py's guarded send: type bounded, marker-checked chunks; verify
// the body before Return; confirm the composer cleared; on a failed body write,
// backspace only the text this send owns.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	ChunkSettleDelay    = 100 * time.Millisecond
	ComposerSettleDelay = 500 * time.Millisecond
	ProbeTimeout        = 3 * time.Second
	ProbeInterval       = 50 * time.Millisecond
	// SubmitAttempts: TUIs can read a Return that follows typed text closely as a
	// newline in the box (ops/type-into.sh), so Return is repeated while our text
	// is still there.
	SubmitAttempts = 3
)

// Clock is injected so tests never sleep.
type Clock interface {
	Now() time.Time
	Sleep(time.Duration)
}

type realClock struct{}

func (realClock) Now() time.Time        { return time.Now() }
func (realClock) Sleep(d time.Duration) { time.Sleep(d) }

// RealClock is the wall clock.
var RealClock Clock = realClock{}

// ErrDirty: a body write started but could not be verified; Return was withheld.
type ErrDirty struct {
	Msg     string
	Owned   string
	Cleared bool
	// Restored: the composer held text this send does not own (a draft whose
	// caret looked like an empty prompt); exactly the typed text was removed.
	Restored bool
}

func (e *ErrDirty) Error() string {
	detail := "composer cleanup failed"
	if e.Cleared {
		detail = "composer cleared"
	}
	return e.Msg + "; " + detail
}

// ErrAmbiguous: Return was sent but the composer did not confirm the submission,
// so the line may have been delivered and must not be retyped.
var ErrAmbiguous = errors.New("target did not confirm submission; delivery is ambiguous; do not resend")

// Pane is one composer the transaction reads and writes.
type Pane interface {
	// State is composer_state: nil when no live composer is recognisable.
	State(ctx context.Context) (*ComposerReading, error)
	TypeText(ctx context.Context, text string) error
}

type sessionPane struct {
	ctl   Ctl
	s     Session
	agent Agent
}

func (p *sessionPane) State(ctx context.Context) (*ComposerReading, error) {
	screen, err := p.ctl.Text(ctx, p.s, BoxLines)
	if err != nil {
		return nil, err
	}
	content, ok := LivePromptText(p.agent, screen)
	if !ok {
		return nil, nil
	}
	cursor, err := p.ctl.Cursor(ctx, p.s)
	if err != nil {
		return nil, err
	}
	return &ComposerReading{Content: content, Cursor: cursor}, nil
}

func (p *sessionPane) TypeText(ctx context.Context, text string) error {
	return p.ctl.Type(ctx, p.s, text)
}

// Transaction types one line into one composer.
type Transaction struct {
	Pane  Pane
	Agent Agent
	Clock Clock
	// BeforeSubmit runs once the body is verified, before the first Return; an
	// error withholds the Return and clears the body.
	BeforeSubmit func() error
}

func (t *Transaction) tolerant() bool { return t.Agent == Codex }

func (t *Transaction) waitForComposerChange(ctx context.Context, previous *ComposerReading, settle time.Duration, matches func(string) bool) (*ComposerReading, error) {
	tolerant := t.tolerant()
	deadline := t.Clock.Now().Add(ProbeTimeout)
	var stable *ComposerReading
	var since time.Time
	for {
		state, err := t.Pane.State(ctx)
		if err != nil {
			return nil, err
		}
		now := t.Clock.Now()
		var good bool
		if matches != nil {
			good = state != nil && matches(state.Content)
		} else {
			good = state != nil && !SameComposerState(state, previous, tolerant)
		}
		if good {
			if since.IsZero() || (matches == nil && !SameComposerState(state, stable, tolerant)) {
				stable, since = state, now
			} else if now.Sub(since) >= settle {
				return state, nil
			}
		} else {
			stable, since = nil, time.Time{}
		}
		if !now.Before(deadline) {
			return nil, nil
		}
		t.Clock.Sleep(ProbeInterval)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

// typeBody types bounded marked chunks, removing each marker before continuing.
// owned tracks the exact composer text owned at the latest boundary.
func (t *Transaction) typeBody(ctx context.Context, text string, initial ComposerReading, owned *string) (ComposerReading, error) {
	marker := ComposerProbeMarker(text)
	tolerant := t.tolerant()
	chunks := TextChunks(text, TypeChunkBytes-len(marker))
	state := initial
	expected := ""
	for index, chunk := range chunks {
		attempted := expected + chunk
		marked := attempted + marker
		*owned = marked
		dirty := func(format string, args ...any) error {
			return &ErrDirty{Msg: fmt.Sprintf("message chunk %d/%d ", index+1, len(chunks)) + fmt.Sprintf(format, args...) + "; submit withheld", Owned: marked}
		}
		if err := t.Pane.TypeText(ctx, chunk+marker); err != nil {
			return state, dirty("failed after its body write started: %v", err)
		}
		changed, err := t.waitForComposerChange(ctx, &state, ChunkSettleDelay, func(c string) bool {
			return ChunkIsVisible(c, marked, chunk+marker, tolerant, "")
		})
		if err != nil {
			return state, dirty("failed after its body write started: %v", err)
		}
		if changed == nil {
			return state, dirty("was typed but the target composer did not confirm it")
		}
		if !ComposerHasExpectedTail(changed.Content, marked, chunk+marker, tolerant) {
			return state, dirty("is incomplete in the target composer")
		}
		if err := t.Pane.TypeText(ctx, strings.Repeat("\x7f", utf8.RuneCountInString(marker))); err != nil {
			return state, dirty("failed after its body write started: %v", err)
		}
		settle := ChunkSettleDelay
		if index == len(chunks)-1 {
			settle = ComposerSettleDelay
		}
		unmarked, err := t.waitForComposerChange(ctx, changed, settle, func(c string) bool {
			return ChunkIsVisible(c, attempted, chunk, tolerant, marker)
		})
		if err != nil {
			return state, dirty("failed after its body write started: %v", err)
		}
		if unmarked == nil || strings.Contains(unmarked.Content, marker) {
			return state, &ErrDirty{Msg: fmt.Sprintf("message marker after chunk %d/%d was not confirmably removed; submit withheld", index+1, len(chunks)), Owned: marked}
		}
		if !ComposerHasExpectedTail(unmarked.Content, attempted, chunk, tolerant) {
			return state, dirty("changed during marker removal")
		}
		*owned = attempted
		expected = attempted
		state = *unmarked
	}
	return state, nil
}

func (t *Transaction) isEmpty(s *ComposerReading) bool {
	return s != nil && s.Cursor == EmptyCursorColumn && ComposerIsEmpty(t.Agent, s.Content)
}

type cleanupState struct {
	state *ComposerReading
	spans map[Span]bool
}

// waitForCleanupState waits for a backspace batch to reach a stable observable state.
func (t *Transaction) waitForCleanupState(ctx context.Context, previous *ComposerReading, owned string, allowedEnds map[int]bool, acceptEmpty bool) (*cleanupState, error) {
	tolerant := t.tolerant()
	deadline := t.Clock.Now().Add(ProbeTimeout)
	type key struct {
		cursor int
		valid  bool
		spans  string
	}
	var stable *key
	var since time.Time
	for {
		state, err := t.Pane.State(ctx)
		if err != nil {
			return nil, err
		}
		now := t.Clock.Now()
		empty := t.isEmpty(state)
		if empty && acceptEmpty {
			return &cleanupState{state, map[Span]bool{}}, nil
		}
		spans := map[Span]bool{}
		if state != nil {
			spans = ComposerOwnedSpans(state.Content, owned, allowedEnds, tolerant)
		}
		if SameComposerState(state, previous, tolerant) || (empty && !acceptEmpty) {
			stable, since = nil, time.Time{}
		} else {
			k := key{spans: spanKey(spans)}
			if state != nil {
				k.cursor, k.valid = state.Cursor, true
			}
			if stable == nil || *stable != k {
				stable, since = &k, now
			} else if !since.IsZero() && now.Sub(since) >= ChunkSettleDelay {
				if state == nil {
					return nil, nil
				}
				return &cleanupState{state, spans}, nil
			}
		}
		if !now.Before(deadline) {
			return nil, nil
		}
		t.Clock.Sleep(ProbeInterval)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

func spanKey(spans map[Span]bool) string {
	keys := make([]string, 0, len(spans))
	for s := range spans {
		keys = append(keys, fmt.Sprintf("%d:%d", s.Start, s.End))
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

// clearComposer backspaces text owned by this send without touching anything else.
func (t *Transaction) clearComposer(ctx context.Context, initial ComposerReading, owned string) (bool, error) {
	ownedLen := utf8.RuneCountInString(owned)
	allowed := map[int]bool{}
	for i := 1; i <= ownedLen; i++ {
		allowed[i] = true
	}
	settled, err := t.waitForCleanupState(ctx, &initial, owned, allowed, false)
	if err != nil || settled == nil {
		return false, err
	}
	state, spans := settled.state, settled.spans
	for {
		if *state == initial || t.isEmpty(state) {
			return true, nil
		}
		if len(spans) == 0 {
			return false, nil
		}
		previousEnd, minLen := 0, -1
		for s := range spans {
			previousEnd = max(previousEnd, s.End)
			if l := s.End - s.Start; minLen < 0 || l < minLen {
				minLen = l
			}
		}
		deleteCount := min(TypeChunkBytes, minLen)
		if deleteCount == 0 {
			return false, nil
		}
		if err := t.Pane.TypeText(ctx, strings.Repeat("\x7f", deleteCount)); err != nil {
			return false, err
		}
		next := map[int]bool{}
		for s := range spans {
			for c := s.End - deleteCount; c < s.End; c++ {
				next[c] = true
			}
		}
		settled, err = t.waitForCleanupState(ctx, state, owned, next, true)
		if err != nil || settled == nil {
			return false, err
		}
		state, spans = settled.state, settled.spans
		if t.isEmpty(state) {
			return true, nil
		}
		if len(spans) == 0 {
			return false, nil
		}
		end := 0
		for s := range spans {
			end = max(end, s.End)
		}
		if end >= previousEnd {
			return false, nil
		}
	}
}

// waitForAccepted waits until submission clears the composed state and restores
// the empty cursor column.
func (t *Transaction) waitForAccepted(ctx context.Context, composed ComposerReading) (bool, error) {
	deadline := t.Clock.Now().Add(ProbeTimeout)
	var since time.Time
	for {
		state, err := t.Pane.State(ctx)
		if err != nil {
			return false, err
		}
		now := t.Clock.Now()
		if t.isEmpty(state) && state.Content != composed.Content {
			if since.IsZero() {
				since = now
			} else if now.Sub(since) >= ChunkSettleDelay {
				return true, nil
			}
		} else {
			since = time.Time{}
		}
		if !now.Before(deadline) {
			return false, nil
		}
		t.Clock.Sleep(ProbeInterval)
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
}

func (t *Transaction) dirty(ctx context.Context, initial ComposerReading, d *ErrDirty) error {
	// clean up even when the caller is cancelled (host shutdown mid-body), as
	// peer-chat does after an interrupt; the probes are bounded by ProbeTimeout.
	ctx = context.WithoutCancel(ctx)
	cleared, err := t.clearComposer(ctx, initial, d.Owned)
	d.Cleared = cleared && err == nil
	if !d.Cleared && !t.tolerant() {
		if ok, err := t.restoreDraft(ctx, initial, d.Owned); err == nil && ok {
			d.Cleared, d.Restored = true, true
		}
	}
	return d
}

// restoreDraft undoes text typed into someone's draft. Claude shows its ghost
// suggestion at the prompt column, so a draft whose caret was moved to column 2
// reads the same; the body then lands at the caret, inside the draft. When the
// composer is exactly the initial text with owned inserted at one place, the
// caret sits right after owned, and backspacing its runes restores the draft.
func (t *Transaction) restoreDraft(ctx context.Context, initial ComposerReading, owned string) (bool, error) {
	if owned == "" || !singleCellRunes(owned) {
		return false, nil // a backspace could remove more than one rune
	}
	state, err := t.Pane.State(ctx)
	if err != nil || state == nil {
		return false, err
	}
	base, typed := compact(initial.Content), compact(owned)
	at := insertedAt(compact(state.Content), base, typed)
	if len(at) == 0 {
		return false, nil
	}
	rs := []rune(owned)
	for left := len(rs); left > 0; {
		n := min(TypeChunkBytes, left)
		if err := t.Pane.TypeText(ctx, strings.Repeat("\x7f", n)); err != nil {
			return false, err
		}
		left -= n
		want := compact(string(rs[:left]))
		got, err := t.waitForComposerChange(ctx, nil, ChunkSettleDelay, func(c string) bool {
			cc := compact(c)
			for _, i := range at {
				if len(cc) == len(base)+len(want) && string(cc) == string(base[:i])+string(want)+string(base[i:]) {
					return true
				}
			}
			return false
		})
		if err != nil || got == nil {
			return false, err
		}
		if left == 0 {
			return SameComposerText(got.Content, initial.Content, false) && got.Cursor == initial.Cursor, nil
		}
	}
	return false, nil
}

// insertedAt lists where typed sits in got such that removing it leaves base.
func insertedAt(got, base, typed []rune) []int {
	var at []int
	if len(got) != len(base)+len(typed) {
		return nil
	}
	for i := 0; i+len(typed) <= len(got); i++ {
		if slices.Equal(got[i:i+len(typed)], typed) && slices.Equal(got[:i], base[:i]) && slices.Equal(got[i+len(typed):], base[i:]) {
			at = append(at, i)
		}
	}
	return at
}

// compact drops whitespace (row breaks, wrap-eaten spaces) for comparing text
// across rewrapping.
func compact(s string) []rune {
	var out []rune
	for _, r := range s {
		if !unicode.IsSpace(r) {
			out = append(out, r)
		}
	}
	return out
}

// singleCellRunes: no rune that a TUI could fold into its neighbour's grapheme
// (combining marks, joiners, variation selectors, skin tones, flags).
func singleCellRunes(s string) bool {
	for _, r := range s {
		switch {
		case unicode.In(r, unicode.Mn, unicode.Me, unicode.Mc),
			r == 0x200C || r == 0x200D,
			r >= 0xFE00 && r <= 0xFE0F,
			r >= 0x1F3FB && r <= 0x1F3FF,
			r >= 0x1F1E6 && r <= 0x1F1FF,
			r >= 0xE0020 && r <= 0xE007F:
			return false
		}
	}
	return true
}

// Send types line into a composer that the caller observed as ready (initial is
// that reading) and submits it. Errors: *ErrDirty (nothing submitted), ErrAmbiguous
// (Return sent, unconfirmed: never retype), others = transport failure before or
// after submission as wrapped.
func (t *Transaction) Send(ctx context.Context, line string, initial ComposerReading) error {
	owned := ""
	composed, err := t.typeBody(ctx, line, initial, &owned)
	if err != nil {
		var d *ErrDirty
		if errors.As(err, &d) {
			return t.dirty(ctx, initial, d)
		}
		return t.dirty(ctx, initial, &ErrDirty{Msg: "body write failed: " + err.Error() + "; submit withheld", Owned: owned})
	}
	// A particle can hide a wrong character until it moves, so verify the visible
	// body once more against the final chunk.
	tolerant := t.tolerant()
	chunks := TextChunks(line, TypeChunkBytes-len(ComposerProbeMarker(line)))
	finalChunk := chunks[len(chunks)-1]
	before, err := t.Pane.State(ctx)
	if err != nil {
		return t.dirty(ctx, initial, &ErrDirty{Msg: "final composer check failed: " + err.Error() + "; submit withheld", Owned: line})
	}
	if before == nil || !SameComposerState(before, &composed, tolerant) || !ComposerHasExpectedTail(before.Content, line, finalChunk, tolerant) {
		return t.dirty(ctx, initial, &ErrDirty{Msg: "target composer changed before submit; submit withheld", Owned: line})
	}
	if t.BeforeSubmit != nil {
		if err := t.BeforeSubmit(); err != nil {
			return t.dirty(ctx, initial, &ErrDirty{Msg: "could not record the submit (" + err.Error() + "); submit withheld", Owned: line})
		}
	}
	for attempt := 1; attempt <= SubmitAttempts; attempt++ {
		if err := t.Pane.TypeText(ctx, "\r"); err != nil {
			return fmt.Errorf("submit request failed; delivery is ambiguous; do not resend: %w", err)
		}
		accepted, err := t.waitForAccepted(ctx, composed)
		if err != nil {
			return fmt.Errorf("submission confirmation failed; delivery is ambiguous; do not resend: %w", err)
		}
		if accepted {
			return nil
		}
		// Return again only while our whole body is still the composer's content:
		// the Return was read as a newline in the box.
		now, err := t.Pane.State(ctx)
		if err != nil || now == nil || !ComposerHasExpectedTail(now.Content, line, finalChunk, tolerant) || !startsWithLine(now.Content, line, tolerant) {
			return ErrAmbiguous
		}
	}
	return ErrAmbiguous
}

// startsWithLine: the composer holds nothing but line (wrapped), so a further
// Return submits exactly it.
func startsWithLine(content, line string, tolerant bool) bool {
	return ComposerHasExpectedTail(content, line, line, tolerant)
}
