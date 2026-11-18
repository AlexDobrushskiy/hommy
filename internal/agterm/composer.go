package agterm

// Go port of the composer reading in ~/.local/bin/peer-chat.py (Claude Code and
// Codex). Function names follow peer-chat's so its tests port one to one; string
// lengths and indices are in code points, as in Python.

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// TypeChunkBytes bounds one typed event: a narrow composer scrolls a longer
	// event's opening out of view, and TUIs lose bytes from large bursts.
	TypeChunkBytes     = 197
	ChunkVerifyOverlap = 32
	BoxLines           = 80
	EmptyCursorColumn  = 2
	CodexEmptyPrompt   = "Ask Codex to do anything"
)

// Agent is the TUI a pane runs.
type Agent string

const (
	Claude Agent = "claude"
	Codex  Agent = "codex"
)

var (
	ruleRE             = rx(`^[{WS}]*[─\x{2014}-]{10,}(?:[{WS}]+[^─\x{2014}-].*?[{WS}]+[─\x{2014}-]+)?[{WS}]*$`)
	codexPromptRE      = rx(`^[›»][{WS}\x{a0}\x{2800}-\x{28FF}]?[{WS}\x{a0}]*(.*?)[{WS}]*$`)
	codexShellPromptRE = rx(`^![{WS}\x{a0}]*(.*?)[{WS}]*$`)
	codexChoiceRE      = rx(`^\p{Nd}+\.[{WS}]`)
	codexParticleRE    = regexp.MustCompile(`[\x{2800}-\x{28FF}]`)
	codexIndentRE      = rx(`^[{WS}\x{2800}-\x{28FF}]{0,2}`)
	codexFooterRE      = rx(`^ {2}[^{WS}].*$`)
	codexNoticeRE      = rx(`^ {3,}[^{WS}].*$`)
	claudePromptRE     = rx(`^[{WS}]*❯[{WS}\x{a0}]*(.*?)[{WS}]*$`)
	claudeFooterRE     = rx(`^ {2,}[^{WS}].*$`)
	claudeStartupHint  = regexp.MustCompile(`^Try ".+"$`)
	tokenRE            = regexp.MustCompile(` ?[^ ]+`)
)

var claudeEmptyPrompts = map[string]bool{
	"":                                 true,
	"Press up to edit queued messages": true,
	"Press up to edit queued messages, Enter to send them immediately": true,
}

func isParticle(r rune) bool { return r >= 0x2800 && r <= 0x28FF }

// CodexRowIsBlank: a row holding nothing but whitespace and idle-animation particles.
func CodexRowIsBlank(line string) bool {
	return strip(codexParticleRE.ReplaceAllString(line, " ")) == ""
}

// TrailingInputBlock is the last non-blank block of the screen above Codex's footer.
func TrailingInputBlock(text string) []string {
	lines := lastN(splitLines(text), BoxLines)
	for len(lines) > 0 && CodexRowIsBlank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	footerRows := 0
	if len(lines) > 0 && codexFooterRE.MatchString(lines[len(lines)-1]) {
		footerRows = 1
	}
	n := len(lines)
	if n > 2 && CodexRowIsBlank(lines[n-3]) && codexFooterRE.MatchString(lines[n-2]) &&
		(codexFooterRE.MatchString(lines[n-1]) || codexNoticeRE.MatchString(lines[n-1])) {
		footerRows = 2
	}
	if footerRows > 0 {
		lines = lines[:len(lines)-footerRows]
		for len(lines) > 0 && CodexRowIsBlank(lines[len(lines)-1]) {
			lines = lines[:len(lines)-1]
		}
	}
	start := len(lines)
	for start > 0 && !CodexRowIsBlank(lines[start-1]) {
		start--
	}
	return lines[start:]
}

// CodexLivePromptText returns the live Codex composer content, ok=false when the
// screen does not end in a recognisable composer.
func CodexLivePromptText(text string) (string, bool) {
	block := TrailingInputBlock(text)
	if len(block) == 0 {
		return "", false
	}
	for _, line := range block {
		if codexShellPromptRE.MatchString(line) {
			return "", false
		}
	}
	var rows []string
	for _, line := range block {
		if !CodexRowIsBlank(line) {
			rows = append(rows, line)
		}
	}
	if len(rows) == 0 {
		return "", false
	}
	m := codexPromptRE.FindStringSubmatch(rows[0])
	if m == nil || codexChoiceRE.MatchString(m[1]) {
		return "", false
	}
	for _, line := range rows[1:] {
		r, ok := firstRune(line)
		if !ok || (!isSpace(r) && !isParticle(r)) {
			return "", false
		}
	}
	content := []string{m[1]}
	for _, line := range rows[1:] {
		content = append(content, strip(codexIndentRE.ReplaceAllString(line, "")))
	}
	return joinNonEmpty(content), true
}

// ClaudeLivePromptText returns the live Claude Code composer content.
func ClaudeLivePromptText(text string) (string, bool) {
	rows, ok := ClaudeComposerRows(text)
	if !ok {
		return "", false
	}
	return joinNonEmpty(rows), true
}

// ClaudeComposerRows is the live composer's rows as shown (blank rows kept):
// the prompt row's text, then each row up to the closing rule.
func ClaudeComposerRows(text string) ([]string, bool) {
	lines := lastN(splitLines(text), BoxLines)
	for index := len(lines) - 1; index >= 0; index-- {
		m := claudePromptRE.FindStringSubmatch(lines[index])
		if m == nil {
			continue
		}
		content := []string{m[1]}
		for offset := index + 1; offset < len(lines); offset++ {
			line := lines[offset]
			if ruleRE.MatchString(line) {
				for _, item := range lines[offset+1:] {
					if strip(item) == "" {
						continue
					}
					if !claudeFooterRE.MatchString(item) || codexChoiceRE.MatchString(strip(item)) {
						return nil, false
					}
				}
				return content, true
			}
			content = append(content, strip(line))
		}
		if index == len(lines)-1 {
			return content, true
		}
		return nil, false
	}
	return nil, false
}

// claudeHasBlankRow: the composer spans rows and one is empty, which only a
// typed line break makes (a suggestion is never split by an empty row).
func claudeHasBlankRow(screen string) bool {
	rows, ok := ClaudeComposerRows(screen)
	if !ok || len(rows) < 2 {
		return false
	}
	for _, r := range rows {
		if strip(r) == "" {
			return true
		}
	}
	return false
}

// LivePromptText dispatches on the agent profile.
func LivePromptText(agent Agent, text string) (string, bool) {
	if agent == Codex {
		return CodexLivePromptText(text)
	}
	return ClaudeLivePromptText(text)
}

// CodexPlaceholderShown reads the empty-composer placeholder through the idle
// animation: every particle may stand for the character under it or an empty cell.
func CodexPlaceholderShown(content string) bool {
	var parts []string
	for _, row := range splitLines(content) {
		parts = append(parts, strip(row))
	}
	shown := strings.Join(parts, " ")
	expected := []rune(CodexEmptyPrompt)
	reached := map[int]bool{0: true}
	for _, char := range shown {
		advanced := map[int]bool{}
		for index := range reached {
			switch {
			case isParticle(char):
				advanced[index] = true
				if index < len(expected) {
					advanced[index+1] = true
				}
			case index < len(expected) && expected[index] == char:
				advanced[index+1] = true
			case index == len(expected) && char == ' ':
				advanced[index] = true
			}
		}
		reached = advanced
		if len(reached) == 0 {
			return false
		}
	}
	return reached[len(expected)]
}

// ComposerIsEmpty recognises known empty-input content.
func ComposerIsEmpty(agent Agent, content string) bool {
	if agent == Codex {
		return CodexPlaceholderShown(content)
	}
	joined := strings.Join(splitLines(content), " ")
	return claudeEmptyPrompts[joined] || claudeStartupHint.MatchString(joined)
}

// TextChunks splits normalised text into independently observable input events.
func TextChunks(text string, maxBytes int) []string {
	maxBytes = max(4, min(TypeChunkBytes, maxBytes))
	var chunks []string
	current := ""
	for _, token := range tokenRE.FindAllString(text, -1) {
		if len(current+token) <= maxBytes {
			current += token
			continue
		}
		if current != "" {
			chunks = append(chunks, current)
			current = ""
		}
		if len(token) <= maxBytes {
			current = token
			continue
		}
		var piece strings.Builder
		for _, char := range token {
			if piece.Len() > 0 && piece.Len()+utf8.RuneLen(char) > maxBytes {
				chunks = append(chunks, piece.String())
				piece.Reset()
			}
			piece.WriteRune(char)
		}
		if piece.Len() > 0 {
			chunks = append(chunks, piece.String())
		}
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks
}

// ComposerProbeMarker chooses a short visible marker that cannot occur in text.
func ComposerProbeMarker(text string) string {
	for index := 0; ; index++ {
		marker := fmt.Sprintf(" [peer-check:%x]", index)
		if !strings.Contains(text, marker) {
			return marker
		}
	}
}

// TrailingParticleVariants: the row as shown, then with one more trailing particle
// removed each time.
func TrailingParticleVariants(row string) []string {
	row = rstrip(row)
	variants := []string{row}
	for {
		r, ok := lastRune(row)
		if !ok || !isParticle(r) {
			return variants
		}
		rs := []rune(row)
		row = rstrip(string(rs[:len(rs)-1]))
		variants = append(variants, row)
	}
}

func rowReadings(row string, tolerant bool) []string {
	if tolerant {
		return TrailingParticleVariants(row)
	}
	return []string{row}
}

type rowReading struct {
	text     []rune
	scrolled bool
}

// composerRowReadings offers the literal row and, for Codex, the reading without
// the ↑ scroll indicator it draws on the first visible row of a scrolled composer.
func composerRowReadings(rows []string, index int, tolerant bool) []rowReading {
	row := rows[index]
	var readings []rowReading
	for _, r := range rowReadings(row, tolerant) {
		readings = append(readings, rowReading{[]rune(r), false})
	}
	if tolerant && index == 0 && len(rows) > 1 && strings.HasSuffix(row, "↑") {
		trimmed := rstrip(strings.TrimSuffix(row, "↑"))
		for _, r := range rowReadings(trimmed, tolerant) {
			if r != "" {
				readings = append(readings, rowReading{[]rune(r), true})
			}
		}
	}
	return readings
}

// rowMatchesRunes compares one screen row with source text of the same length;
// with tolerant a braille particle stands for the one cell it covers.
func rowMatchesRunes(expected, row []rune, tolerant bool) bool {
	if len(expected) != len(row) {
		return false
	}
	for i := range expected {
		if expected[i] != row[i] && (!tolerant || !isParticle(row[i])) {
			return false
		}
	}
	return true
}

// SameComposerText: equal composer text; with tolerant a particle on either side
// is its cell.
func SameComposerText(first, second string, tolerant bool) bool {
	if !tolerant {
		return first == second
	}
	a, b := rowsOf(first), rowsOf(second)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameRow(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sameRow(one, other string) bool {
	for _, x := range TrailingParticleVariants(one) {
		for _, y := range TrailingParticleVariants(other) {
			xs, ys := []rune(x), []rune(y)
			if len(xs) != len(ys) {
				continue
			}
			same := true
			for i := range xs {
				if xs[i] != ys[i] && !isParticle(xs[i]) && !isParticle(ys[i]) {
					same = false
					break
				}
			}
			if same {
				return true
			}
		}
	}
	return false
}

// ComposerReading is one sample of a composer: its content and cursor column.
type ComposerReading struct {
	Content string
	Cursor  int
}

// SameComposerState compares two readings; nil only equals nil.
func SameComposerState(first, second *ComposerReading, tolerant bool) bool {
	if first == nil || second == nil {
		return first == second
	}
	return first.Cursor == second.Cursor && SameComposerText(first.Content, second.Content, tolerant)
}

// ChunkIsVisible: the typed chunk shows in the composer, without a marker that
// should be gone.
func ChunkIsVisible(content, expected, chunk string, tolerant bool, marker string) bool {
	if marker != "" && strings.Contains(content, marker) {
		return false
	}
	return ComposerHasExpectedTail(content, expected, chunk, tolerant)
}

// ComposerHasExpectedTail matches a source suffix while allowing visual wrapping.
func ComposerHasExpectedTail(content, expected, chunk string, tolerant bool) bool {
	exp := []rune(expected)
	positions := map[int]bool{len(exp): true}
	rows := rowsOf(content)
	for index := len(rows) - 1; index >= 0; index-- {
		readings := composerRowReadings(rows, index, tolerant)
		matched := map[int]bool{}
		for end := range positions {
			for _, rd := range readings {
				n := len(rd.text)
				if end >= n && (!rd.scrolled || end > n) && rowMatchesRunes(exp[end-n:end], rd.text, tolerant) {
					matched[end-n] = true
				}
			}
		}
		if len(matched) == 0 {
			return false
		}
		if index > 0 {
			// Plain screen text omits a source space consumed by visual wrapping.
			next := map[int]bool{}
			for start := range matched {
				next[start] = true
				if start > 0 && exp[start-1] == ' ' {
					next[start-1] = true
				}
			}
			positions = next
		} else {
			positions = matched
		}
	}
	probeStart := max(0, len(exp)-utf8.RuneCountInString(chunk)-ChunkVerifyOverlap)
	anyStart := false
	for start := range positions {
		if start <= probeStart {
			anyStart = true
			if start == 0 {
				return true
			}
		}
	}
	if !anyStart {
		return false
	}
	return !ChunkTailIsAmbiguous(expected, chunk)
}

// ChunkTailIsAmbiguous rejects a clipped tail that could hide a deletion from chunk.
func ChunkTailIsAmbiguous(expected, chunk string) bool {
	if chunk == "" {
		return false
	}
	exp := []rune(expected)
	expectedLength := len(exp)
	chunkLength := utf8.RuneCountInString(chunk)
	visibleLength := min(expectedLength, chunkLength+ChunkVerifyOverlap)
	chunkStart := expectedLength - chunkLength
	for missingStart := chunkStart; missingStart < expectedLength; missingStart++ {
		for missingEnd := missingStart + 1; missingEnd <= expectedLength; missingEnd++ {
			missingLength := missingEnd - missingStart
			earlierStart := expectedLength - missingLength - visibleLength
			if earlierStart < 0 {
				continue
			}
			if slices.Equal(exp[earlierStart:missingStart], exp[expectedLength-visibleLength:missingEnd]) {
				return true
			}
		}
	}
	return false
}

// Span is a [start, end) range of owned text visible in the composer.
type Span struct{ Start, End int }

// ComposerOwnedSpans locates visible contiguous owned text ending at an allowed
// prefix boundary.
func ComposerOwnedSpans(content, ownedText string, allowedEnds map[int]bool, tolerant bool) map[Span]bool {
	owned := []rune(ownedText)
	result := map[Span]bool{}
	if content == "" {
		for end := range allowedEnds {
			if end == 1 && len(owned) >= 1 && owned[0] == ' ' {
				result[Span{end - 1, end}] = true
			}
		}
		return result
	}
	positions := map[Span]bool{}
	for end := range allowedEnds {
		if end > 0 && end <= len(owned) {
			positions[Span{end, end}] = true
			if owned[end-1] == ' ' {
				positions[Span{end - 1, end}] = true
			}
		}
	}
	rows := rowsOf(content)
	for index := len(rows) - 1; index >= 0; index-- {
		readings := composerRowReadings(rows, index, tolerant)
		matched := map[Span]bool{}
		for pos := range positions {
			for _, rd := range readings {
				n := len(rd.text)
				if pos.Start >= n && (!rd.scrolled || pos.Start > n) &&
					rowMatchesRunes(owned[pos.Start-n:pos.Start], rd.text, tolerant) {
					matched[Span{pos.Start - n, pos.End}] = true
				}
			}
		}
		if len(matched) == 0 {
			return map[Span]bool{}
		}
		if index > 0 {
			next := map[Span]bool{}
			for s := range matched {
				next[s] = true
				if s.Start > 0 && owned[s.Start-1] == ' ' {
					next[Span{s.Start - 1, s.End}] = true
				}
			}
			positions = next
		} else {
			positions = matched
		}
	}
	return positions
}

func joinNonEmpty(parts []string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "\n")
}

// ---- pane resolution (which program a pane runs) ----

var interpreterRE = regexp.MustCompile(`^(?:node|bun|deno|python[0-9.]*|ruby|perl|sh|bash|zsh)$`)

// LeadingCommand names the program a pane runs, only when its command line
// leaves no doubt.
func LeadingCommand(foreground []string) string {
	if len(foreground) == 0 {
		return ""
	}
	name := filepath.Base(foreground[0])
	if !interpreterRE.MatchString(name) {
		return name
	}
	if len(foreground) > 1 && !strings.HasPrefix(foreground[1], "-") {
		script := filepath.Base(foreground[1])
		stem, suffix := splitExt(script)
		if suffix == ".js" || suffix == ".mjs" || suffix == ".cjs" {
			return stem
		}
		return script
	}
	return ""
}

// Runs reports whether a foreground command line runs command.
func Runs(foreground []string, command string) bool {
	if len(foreground) == 0 {
		return false
	}
	pattern := rx(`(?:^|[/{WS}])` + regexp.QuoteMeta(command) + `(?:$|[{WS}])`)
	for _, part := range foreground {
		if pattern.MatchString(part) {
			return true
		}
	}
	return LeadingCommand(foreground) == command
}

// splitExt is Python's os.path.splitext for a base name.
func splitExt(name string) (string, string) {
	dot := strings.LastIndex(name, ".")
	if dot <= 0 || strings.Trim(name[:dot], ".") == "" {
		return name, ""
	}
	return name[:dot], name[dot:]
}
