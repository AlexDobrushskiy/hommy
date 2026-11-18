package agterm

// Ports of the pure-parser cases in ~/.claude/skills/peer-chat/test_peer_chat.py.
// Each subtest is named after the Python test method it reproduces.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	rule        = "────────────────────────────────────────" // "─" * 40
	codexFooter = "  repo · master · gpt-5.6 high · Context 0% used"
	codexNotice = "                                                       ⚠ 1 warning · f2 to view" // " " * 55 + ...
)

type namedCase struct {
	name string
	run  func(t *testing.T)
}

func runCases(t *testing.T, cases []namedCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, c.run)
	}
}

// live is LIVE_PROMPT_TEXT with Python's None as a nil pointer.
func live(agent Agent, screen string) *string {
	content, ok := LivePromptText(agent, screen)
	if !ok {
		return nil
	}
	return &content
}

func assertLive(t *testing.T, agent Agent, screen, want string) string {
	t.Helper()
	got := live(agent, screen)
	if got == nil {
		t.Fatalf("live prompt = None, want %q\nscreen:\n%s", want, screen)
	}
	if *got != want {
		t.Fatalf("live prompt = %q, want %q", *got, want)
	}
	return *got
}

func assertNotLive(t *testing.T, agent Agent, screen string) {
	t.Helper()
	if got := live(agent, screen); got != nil {
		t.Fatalf("live prompt = %q, want None\nscreen:\n%s", *got, screen)
	}
}

func mustLive(t *testing.T, agent Agent, screen string) string {
	t.Helper()
	got := live(agent, screen)
	if got == nil {
		t.Fatalf("live prompt = None\nscreen:\n%s", screen)
	}
	return *got
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func ends(values ...int) map[int]bool {
	out := map[int]bool{}
	for _, v := range values {
		out[v] = true
	}
	return out
}

func assertSpans(t *testing.T, got map[Span]bool, want ...Span) {
	t.Helper()
	ok := len(got) == len(want)
	for _, s := range want {
		ok = ok && got[s]
	}
	if !ok {
		t.Fatalf("owned spans = %v, want %v", got, want)
	}
}

func assertTail(t *testing.T, want bool, content, expected, chunk string, tolerant bool) {
	t.Helper()
	if got := ComposerHasExpectedTail(content, expected, chunk, tolerant); got != want {
		t.Fatalf("ComposerHasExpectedTail(%q, %q, %q, %v) = %v, want %v",
			content, expected, chunk, tolerant, got, want)
	}
}

func TestClaudeLivePromptText(t *testing.T) {
	runCases(t, []namedCase{
		{"test_live_wrapped_box_without_closing_rule_is_refused", func(t *testing.T) {
			screen := "older output\n" +
				"❯ Chat from Codex: a wrapped message\n" +
				"  with a continuation"
			assertNotLive(t, Claude, screen)
		}},
		{"test_live_stale_prompt_under_a_dialog_is_refused", func(t *testing.T) {
			assertNotLive(t, Claude, "❯ previous user prompt\nAllow this command?\n  1. Yes\n  2. No")
		}},
		{"test_closed_historical_prompt_above_a_dialog_is_refused", func(t *testing.T) {
			screen := rule + "\n" +
				"❯ previous user prompt\n" +
				"assistant response\n" +
				rule + "\n" +
				"Allow this command?\n" +
				"  1. Yes\n" +
				"  2. No"
			assertNotLive(t, Claude, screen)
		}},
		{"test_live_composer_accepts_indented_status_rows", func(t *testing.T) {
			screen := rule + "\n" +
				"❯ Chat from Codex: review this\n" +
				rule + "\n" +
				"  repo [branch] [Opus 5] 70% /rc\n" +
				"  ⏵⏵ bypass permissions on (shift+tab to cycle)"
			assertLive(t, Claude, screen, "Chat from Codex: review this")
		}},
		{"test_live_composer_accepts_padded_status_rows", func(t *testing.T) {
			for _, status := range []string{
				"   repo [branch] [Opus 5] 70% /rc\n  ⏵⏵ auto mode on (shift+tab to cycle)",
				"  repo [branch] [Opus 5] 70% /rc\n    --%\n  ⏵⏵ auto mode on (shift+tab to cycle)",
			} {
				screen := rule + "\n❯ Chat from Codex: review this\n" + rule + "\n" + status
				assertLive(t, Claude, screen, "Chat from Codex: review this")
			}
		}},
		{"test_padded_status_row_does_not_admit_a_dialog", func(t *testing.T) {
			for _, trailer := range []string{
				"   repo [branch] [Opus 5] 70% /rc\nAllow this command?",
				"   repo [branch] [Opus 5] 70% /rc\n    1. Yes\n    2. No",
			} {
				screen := rule + "\n❯ previous user prompt\n" + rule + "\n" + trailer
				assertNotLive(t, Claude, screen)
			}
		}},
		{"test_queue_hints_are_known_empty_claude_placeholders", func(t *testing.T) {
			for _, hint := range []string{
				"Press up to edit queued messages",
				"Press up to edit queued messages, Enter to send them immediately",
			} {
				content := assertLive(t, Claude, rule+"\n❯ "+hint+"\n"+rule, hint)
				if !ComposerIsEmpty(Claude, content) {
					t.Fatalf("ComposerIsEmpty(claude, %q) = false", content)
				}
			}
		}},
		{"test_startup_hints_are_known_empty_claude_placeholders", func(t *testing.T) {
			for _, hint := range []string{
				`Try "fix lint errors"`,
				`Try "create a util logging.py that..."`,
				`Try "edit cookbook/two-agent-chat/peer-chat.py to..."`,
				`Try "refactor "odd\"name.py""`,
			} {
				content := assertLive(t, Claude, rule+"\n❯ "+hint+"\n"+rule, hint)
				if !ComposerIsEmpty(Claude, content) {
					t.Fatalf("ComposerIsEmpty(claude, %q) = false", content)
				}
			}
		}},
		{"test_wrapped_startup_hint_is_a_known_empty_claude_placeholder", func(t *testing.T) {
			screen := rule + "\n" +
				"❯ Try \"write a test for cookbook/two-agent-chat/\n" +
				"  peer-chat.py\"\n" +
				rule
			content := mustLive(t, Claude, screen)
			if !ComposerIsEmpty(Claude, content) {
				t.Fatalf("ComposerIsEmpty(claude, %q) = false", content)
			}
		}},
		{"test_startup_hint_pattern_does_not_accept_other_claude_text", func(t *testing.T) {
			for _, draft := range []string{
				"Try to fix the lint errors",
				`please Try "fix lint errors"`,
				`Try ""`,
			} {
				content := mustLive(t, Claude, rule+"\n❯ "+draft+"\n"+rule)
				if ComposerIsEmpty(Claude, content) {
					t.Fatalf("ComposerIsEmpty(claude, %q) = true", content)
				}
			}
		}},
		{"test_wrapped_queue_hint_is_a_known_empty_claude_placeholder", func(t *testing.T) {
			screen := rule + "\n" +
				"❯ Press up to edit queued messages, Enter to\n" +
				"  send them immediately\n" +
				rule
			content := assertLive(t, Claude, screen,
				"Press up to edit queued messages, Enter to\nsend them immediately")
			if !ComposerIsEmpty(Claude, content) {
				t.Fatalf("ComposerIsEmpty(claude, %q) = false", content)
			}
		}},
	})
}

func TestCodexLivePromptText(t *testing.T) {
	const wrappedReply = "Chat from Claude: a long reply whose first row wraps\nonto a second " +
		"row and remains in the composer"
	runCases(t, []namedCase{
		{"test_numbered_approval_selection_is_not_a_composer", func(t *testing.T) {
			screen := "Would you like to run the following command?\n\n" +
				"› 1. Yes, proceed (y)\n" +
				"  2. Yes, and don't ask again\n" +
				"  3. No, and tell Codex what to do differently\n"
			assertNotLive(t, Codex, screen)
		}},
		{"test_default_glyph", func(t *testing.T) {
			assertLive(t, Codex, "› Chat from Claude: ping\n"+codexFooter, "Chat from Claude: ping")
		}},
		{"test_wrapped_composer_ignores_the_changing_footer", func(t *testing.T) {
			screen := "» Chat from Claude: a long reply whose first row wraps\n" +
				"  onto a second row and remains in the composer\n" +
				codexFooter
			assertLive(t, Codex, screen, wrappedReply)
		}},
		{"test_ultra_effort_glyph", func(t *testing.T) {
			assertLive(t, Codex, "» [Pasted Content 1868 chars]\n"+codexFooter, "[Pasted Content 1868 chars]")
		}},
		{"test_empty_composer_placeholder", func(t *testing.T) {
			assertLive(t, Codex, "» Ask Codex to do anything\n"+codexFooter, "Ask Codex to do anything")
		}},
		{"test_wrapped_empty_composer_placeholder_is_known_empty", func(t *testing.T) {
			content := assertLive(t, Codex, "» Ask Codex to do\n  anything\n"+codexFooter, "Ask Codex to do\nanything")
			if !ComposerIsEmpty(Codex, content) {
				t.Fatalf("ComposerIsEmpty(codex, %q) = false", content)
			}
		}},
		{"test_placeholder_is_read_through_particles", func(t *testing.T) {
			for _, shown := range []string{
				"Ask Co⠁ex to do anything",
				"Ask Codex to do anything⠁",
				"⠁sk Codex to do anything",
				"Ask⠂Codex to do anything",
				"Ask Codex to do⠁\nanything",
				"Ask Codex to d⠁\nanything",
			} {
				if !ComposerIsEmpty(Codex, shown) {
					t.Errorf("ComposerIsEmpty(codex, %q) = false, want true", shown)
				}
			}
			for _, shown := range []string{
				"Ask Codex to do anythin",
				"Ask Codex to do anything!",
				"Ask Codex to do⠁\nanythin",
				"Chat from Claude: ping",
			} {
				if ComposerIsEmpty(Codex, shown) {
					t.Errorf("ComposerIsEmpty(codex, %q) = true, want false", shown)
				}
			}
		}},
		{"test_idle_animation_above_the_prompt_is_ignored", func(t *testing.T) {
			screen := "  ⠁   ⠈         ⠄          ⡀  ⠈\n" +
				"›⠁Ask Codex to do anything⡀  ⠈     ⠁\n" +
				"       ⡀         ⠄                ⠂\n" +
				codexFooter
			content := mustLive(t, Codex, screen)
			if !ComposerIsEmpty(Codex, content) {
				t.Fatalf("ComposerIsEmpty(codex, %q) = false", content)
			}
		}},
		{"test_particle_rows_separate_the_prompt_from_a_queue_notice", func(t *testing.T) {
			screen := "• Messages to be submitted after next tool call\n" +
				"  ↳ Chat from Claude: Review done\n" +
				"    …\n" +
				"    ⠈        ⠁   ⡀\n" +
				"›⠁Ask Codex to do anything⡀  ⠈\n" +
				"       ⡀         ⠄\n" +
				codexFooter
			content := mustLive(t, Codex, screen)
			if !ComposerIsEmpty(Codex, content) {
				t.Fatalf("ComposerIsEmpty(codex, %q) = false", content)
			}
		}},
		{"test_idle_animation_does_not_hide_a_draft", func(t *testing.T) {
			screen := "  ⠁      ⠈\n" +
				"›Chat from Claude: ping\n" +
				codexFooter
			content := assertLive(t, Codex, screen, "Chat from Claude: ping")
			if ComposerIsEmpty(Codex, content) {
				t.Fatalf("ComposerIsEmpty(codex, %q) = true", content)
			}
		}},
		{"test_composer_content_keeps_every_typed_character", func(t *testing.T) {
			assertLive(t, Codex, "›Chat from Claude: explain ⠁ please\n"+codexFooter,
				"Chat from Claude: explain ⠁ please")
		}},
		{"test_foreign_character_beside_owned_text_is_not_owned", func(t *testing.T) {
			content := mustLive(t, Codex, "›Chat from Claude: ping!\n"+codexFooter)
			owned := "Chat from Claude: ping"
			assertTail(t, false, content, owned, owned, false)
		}},
		{"test_particle_beside_owned_text_is_decoration", func(t *testing.T) {
			content := mustLive(t, Codex, "›Chat from Claude: ping⠁\n"+codexFooter)
			owned := "Chat from Claude: ping"
			assertTail(t, true, content, owned, owned, true)
			assertSpans(t, ComposerOwnedSpans(content, owned, ends(runeLen(owned)), true), Span{0, runeLen(owned)})
		}},
		{"test_space_separated_trailing_particles_are_decoration", func(t *testing.T) {
			owned := "Chat from Claude: ping"
			for _, shown := range []string{
				"›Chat from Claude: ping ⠁\n" + codexFooter,
				"›Chat from Claude: ping⠁  ⠂\n" + codexFooter,
				"›Chat from Claude: pin⠁  ⠂\n" + codexFooter,
			} {
				content := mustLive(t, Codex, shown)
				assertTail(t, true, content, owned, owned, true)
				assertSpans(t, ComposerOwnedSpans(content, owned, ends(runeLen(owned)), true), Span{0, runeLen(owned)})
			}
		}},
		{"test_particle_beyond_the_row_end_is_decoration", func(t *testing.T) {
			screen := "›Chat from Claude: a long reply that wraps onto\n" +
				"  a second row [peer-check:0]        ⠈\n" + codexFooter
			content := mustLive(t, Codex, screen)
			marked := "Chat from Claude: a long reply that wraps onto a second row [peer-check:0]"
			assertTail(t, true, content, marked, marked, true)
			assertSpans(t, ComposerOwnedSpans(content, marked, ends(runeLen(marked)), true), Span{0, runeLen(marked)})
		}},
		{"test_particle_over_the_last_typed_character_still_verifies", func(t *testing.T) {
			content := mustLive(t, Codex, "›Chat from Claude: pin⠁\n"+codexFooter)
			owned := "Chat from Claude: ping"
			assertTail(t, true, content, owned, owned, true)
		}},
		{"test_particle_over_a_typed_character_still_verifies", func(t *testing.T) {
			content := mustLive(t, Codex, "›Chat from Cl⠁ude: ping\n"+codexFooter)
			owned := "Chat from Claude: ping"
			assertTail(t, true, content, owned, owned, true)
			assertSpans(t, ComposerOwnedSpans(content, owned, ends(runeLen(owned)), true), Span{0, runeLen(owned)})
		}},
		{"test_wrong_character_under_no_particle_still_fails", func(t *testing.T) {
			content := mustLive(t, Codex, "›Chat from Claude: pinf\n"+codexFooter)
			owned := "Chat from Claude: ping"
			assertTail(t, false, content, owned, owned, true)
			assertSpans(t, ComposerOwnedSpans(content, owned, ends(runeLen(owned)), true))
		}},
		{"test_particle_cannot_hide_a_missing_character", func(t *testing.T) {
			content := mustLive(t, Codex, "›Chat from Claude: pin⠁\n"+codexFooter)
			owned := "Chat from Claude: ping!"
			assertTail(t, false, content, owned, owned, true)
		}},
		{"test_claude_rows_are_compared_verbatim", func(t *testing.T) {
			content := "Chat from Codex: p⠁ng"
			owned := "Chat from Codex: ping"
			assertTail(t, false, content, owned, owned, false)
			assertSpans(t, ComposerOwnedSpans(content, owned, ends(runeLen(owned)), false))
		}},
		{"test_particles_in_the_prompt_gap_and_indent_are_ignored", func(t *testing.T) {
			screen := "›⠁Chat from Claude: a long\n" +
				"⠐ line here⠂\n" +
				codexFooter
			content := assertLive(t, Codex, screen, "Chat from Claude: a long\nline here⠂")
			owned := "Chat from Claude: a long line here"
			assertTail(t, true, content, owned, owned, true)
		}},
		{"test_particle_on_the_first_wrapped_cell_still_verifies", func(t *testing.T) {
			screen := "› ⠁hat from Claude: a long\n" +
				"  ⠐ine here\n" +
				codexFooter
			content := assertLive(t, Codex, screen, "⠁hat from Claude: a long\n⠐ine here")
			owned := "Chat from Claude: a long line here"
			assertTail(t, true, content, owned, owned, true)
		}},
		{"test_idle_animation_over_a_dialog_is_refused", func(t *testing.T) {
			screen := "›⠁Ask Codex to do anything⡀   ⠈\n" +
				"1. yes⠁\n" +
				"2. no\n" +
				codexFooter
			assertNotLive(t, Codex, screen)
		}},
		{"test_shell_mode_is_not_a_prompt", func(t *testing.T) {
			assertNotLive(t, Codex, "! ls -la\n"+codexFooter)
		}},
		{"test_live_shell_blocks_an_older_composer", func(t *testing.T) {
			screen := "› earlier prompt\n" +
				"  some response output\n" +
				"! ls -la\n" +
				"  shell mode active"
			assertNotLive(t, Codex, screen)
		}},
		{"test_prompt_shaped_shell_output_is_not_a_composer", func(t *testing.T) {
			assertNotLive(t, Codex, "! printf output\n  › fake output\n"+codexFooter)
		}},
		{"test_live_prompt_is_adjacent_to_the_footer", func(t *testing.T) {
			assertLive(t, Codex, "» Ask Codex to do anything\n\n"+codexFooter, "Ask Codex to do anything")
		}},
		{"test_live_prompt_accepts_configurable_footers", func(t *testing.T) {
			for _, footer := range []string{
				"  repo · master · gpt-5.6 high",
				"  Context 100% left · repo · master",
				"  Context 0% used · repo · master",
				"  Context 100% left",
				"  gpt-5.6 high",
			} {
				assertLive(t, Codex, "» Ask Codex to do anything\n"+footer, "Ask Codex to do anything")
			}
		}},
		{"test_live_prompt_accepts_two_footer_rows_below_a_blank", func(t *testing.T) {
			screen := "› Ask Codex to do anything\n" +
				"\n" +
				"  GPT-6-Astra high · Context 100% left · /Volumes/Data/Devzone/over…\n" +
				"  ? for shortcuts\n"
			assertLive(t, Codex, screen, "Ask Codex to do anything")
		}},
		{"test_wrapped_composer_keeps_its_rows_above_two_footer_rows", func(t *testing.T) {
			screen := "» Chat from Claude: a long reply whose first row wraps\n" +
				"  onto a second row and remains in the composer\n" +
				"\n" +
				"  repo · master · gpt-5.6 high\n" +
				"  Context 100% left\n"
			assertLive(t, Codex, screen, wrappedReply)
		}},
		{"test_live_prompt_accepts_a_notice_alone_below_the_status_row", func(t *testing.T) {
			screen := "› Chat from Claude: hello\n" +
				"\n" +
				"  GPT-6-Astra medium · sysadm · Context 0% used · weekly 4% left\n" +
				codexNotice + "\n"
			assertLive(t, Codex, screen, "Chat from Claude: hello")
		}},
		{"test_live_prompt_accepts_a_notice_beside_the_shortcut_hint", func(t *testing.T) {
			screen := "› Ask Codex to do anything\n" +
				"\n" +
				"  GPT-6-Astra medium · sysadm · Context 0% used · weekly 4%\n" +
				"  ← for agents · ? for shortcuts                       ⚠ 1 warning · f2 to view\n"
			assertLive(t, Codex, screen, "Ask Codex to do anything")
		}},
		{"test_wrapped_composer_keeps_its_rows_above_a_notice_footer", func(t *testing.T) {
			screen := "» Chat from Claude: a long reply whose first row wraps\n" +
				"  onto a second row and remains in the composer\n" +
				"\n" +
				"  repo · master · gpt-5.6 high\n" +
				codexNotice + "\n"
			assertLive(t, Codex, screen, wrappedReply)
		}},
		{"test_notice_without_a_status_row_is_not_live", func(t *testing.T) {
			assertNotLive(t, Codex, "› Chat from Claude: hello\n\n"+codexNotice+"\n")
		}},
		{"test_three_trailing_indented_rows_are_not_live", func(t *testing.T) {
			screen := "» Ask Codex to do anything\n" +
				"\n" +
				"  /keymap customize\n" +
				"  repo · master · gpt-5.6 high\n" +
				"  ? / esc close\n"
			assertNotLive(t, Codex, screen)
		}},
		{"test_three_trailing_rows_ending_in_a_notice_are_not_live", func(t *testing.T) {
			screen := "» Ask Codex to do anything\n" +
				"\n" +
				"  /keymap customize\n" +
				"  repo · master · gpt-5.6 high\n" +
				codexNotice + "\n"
			assertNotLive(t, Codex, screen)
		}},
		{"test_transcript_history_above_live_prompt_is_ignored", func(t *testing.T) {
			screen := "› earlier prompt\n" +
				"response output\n" +
				"! old shell transcript\n" +
				"old shell output\n" +
				"\n" +
				"» Ask Codex to do anything\n" +
				"\n" +
				codexFooter
			assertLive(t, Codex, screen, "Ask Codex to do anything")
		}},
		{"test_unrecognised_trailing_row_is_not_live", func(t *testing.T) {
			assertNotLive(t, Codex, "» Ask Codex to do anything\nkey hint whose layout is unknown")
		}},
		{"test_dialog_without_a_footer_is_not_live", func(t *testing.T) {
			screen := "› previous user prompt\n" +
				"Approval required\n" +
				"  1. Allow\n" +
				"  2. Deny"
			assertNotLive(t, Codex, screen)
		}},
		{"test_stale_prompt_under_a_dialog_is_not_live", func(t *testing.T) {
			screen := "› previous user prompt\n" +
				"Approval required\n" +
				"  1. Allow\n" +
				"  2. Deny\n" +
				codexFooter
			assertNotLive(t, Codex, screen)
		}},
	})
}

func TestComposerTail(t *testing.T) {
	runCases(t, []namedCase{
		{"test_terminal_wrapping_is_ignored", func(t *testing.T) {
			typed := "Chat from Claude: one two three"
			assertTail(t, true, "Chat from Claude: one tw\no three", typed, typed, false)
		}},
		{"test_space_loss_at_visual_wrap_is_unobservable", func(t *testing.T) {
			typed := strings.Repeat("a", 64) + " beta [peer-check:0]"
			rendered := strings.Repeat("a", 64) + "\nbeta [peer-check:0]"
			assertTail(t, true, rendered, typed, typed, false)
		}},
		{"test_missing_space_on_one_visual_row_is_rejected", func(t *testing.T) {
			typed := "Chat from Claude: alpha beta"
			assertTail(t, false, "Chat from Claude: alphabeta", typed, typed, false)
		}},
		{"test_separator_anchor_rejects_a_missing_first_word_character", func(t *testing.T) {
			expected := strings.Repeat("a", 400) + " alpha beta"
			chunk := " alpha beta"
			corrupted := []rune(strings.Repeat("a", 400) + " lpha beta")
			assertTail(t, false, string(corrupted[len(corrupted)-100:]), expected, chunk, false)
		}},
		{"test_missing_middle_is_rejected_even_when_tail_is_present", func(t *testing.T) {
			typed := "Chat from Claude: opening missing middle closing"
			assertTail(t, false, "Chat from Claude: opening closing", typed, typed, false)
		}},
		{"test_truncated_periodic_chunk_is_ambiguous", func(t *testing.T) {
			marker := " [peer-check:0]"
			previous := strings.Repeat("a", 64)
			chunk := strings.Repeat("a", 50) + marker
			expected := previous + chunk
			missingChunkPrefix := strings.Repeat("a", 104) + marker
			assertTail(t, false, missingChunkPrefix, expected, chunk, false)
		}},
		{"test_scrolled_prefix_is_not_required_after_earlier_chunks_passed", func(t *testing.T) {
			earlier := strings.Repeat("old ", 300)
			latestWords := make([]string, 30)
			for i := range latestWords {
				latestWords[i] = "latest"
			}
			latest := strings.Join(latestWords, " ")
			expected := earlier + latest
			exp := []rune(expected)
			visible := string(exp[len(exp)-(runeLen(latest)+ChunkVerifyOverlap):])
			visible = strings.Replace(visible, " latest", "\nlatest", 2)
			assertTail(t, true, visible, expected, latest, false)
		}},
	})
}

func TestScrollIndicator(t *testing.T) {
	// setUp
	hidden := "Chat from Claude: Reviewed the example module and tests; "
	rows := []string{
		"The first visible row ends beside the scroll indicator.",
		"The next row contains another part of the message.",
		"The remaining rows must still match the source text;",
		"override [peer-check:0]",
	}
	expected := hidden + strings.Join(rows, " ")
	chunk := " override [peer-check:0]"
	rendered := strings.Join(append([]string{rows[0] + "↑"}, rows[1:]...), "\n")

	runCases(t, []namedCase{
		{"test_live_failure_shape_confirms_body_and_cleanup", func(t *testing.T) {
			screen := "› " + strings.ReplaceAll(rendered, "\n", "\n  ")
			screen += "\n\n  model · project\n  tab to queue message\n"
			content := assertLive(t, Codex, screen, rendered)
			assertTail(t, true, content, expected, chunk, true)
			assertSpans(t, ComposerOwnedSpans(content, expected, ends(runeLen(expected)), true),
				Span{runeLen(hidden), runeLen(expected)})
		}},
		{"test_claude_does_not_ignore_arrow", func(t *testing.T) {
			assertTail(t, false, rendered, expected, chunk, false)
			assertSpans(t, ComposerOwnedSpans(rendered, expected, ends(runeLen(expected)), false))
		}},
		{"test_no_hidden_prefix_is_not_a_scroll_indicator", func(t *testing.T) {
			expected := strings.Join(rows, " ")
			assertTail(t, false, rendered, expected, chunk, true)
			assertSpans(t, ComposerOwnedSpans(rendered, expected, ends(runeLen(expected)), true))
		}},
		{"test_one_row_or_bottom_arrow_is_not_ignored", func(t *testing.T) {
			for _, rendered := range []string{
				expected + "↑",
				strings.Join(rows, "\n") + "↑",
				strings.Join(rows, "\n") + "↓",
			} {
				assertTail(t, false, rendered, expected, chunk, true)
				assertSpans(t, ComposerOwnedSpans(rendered, expected, ends(runeLen(expected)), true))
			}
		}},
		{"test_literal_arrows_are_preserved_with_and_without_indicator", func(t *testing.T) {
			rows := append([]string(nil), rows...)
			rows[0] += "↑"
			rows[1] += " literal ↑ arrow ↓"
			expected := hidden + strings.Join(rows, " ")
			for _, decoration := range []string{"", "↑"} {
				rendered := strings.Join(append([]string{rows[0] + decoration}, rows[1:]...), "\n")
				assertTail(t, true, rendered, expected, chunk, true)
				if len(ComposerOwnedSpans(rendered, expected, ends(runeLen(expected)), true)) == 0 {
					t.Fatalf("decoration %q: owned spans empty, want non-empty", decoration)
				}
			}
		}},
		{"test_missing_literal_arrow_and_foreign_input_are_rejected", func(t *testing.T) {
			rows := append([]string(nil), rows...)
			rows[1] += " literal ↑ arrow"
			expected := hidden + strings.Join(rows, " ")
			rendered := strings.Join(append([]string{rows[0] + "↑"}, rows[1:]...), "\n")
			for _, broken := range []string{
				strings.ReplaceAll(rendered, "literal ↑", "literal"),
				strings.ReplaceAll(rendered, "contains", "FOREIGN_TEXT"),
				strings.ReplaceAll(rendered, "override", "overide"),
				rendered + " foreign input",
			} {
				assertTail(t, false, broken, expected, chunk, true)
				assertSpans(t, ComposerOwnedSpans(broken, expected, ends(runeLen(expected)), true))
			}
		}},
		{"test_insufficient_tail_and_periodic_truncation_stay_rejected", func(t *testing.T) {
			assertTail(t, false, "old↑\n"+rows[len(rows)-1], expected, chunk, true)
			expected := strings.Repeat("a", 114) + " [peer-check:0]"
			chunk := strings.Repeat("a", 50) + " [peer-check:0]"
			assertTail(t, false, "aaa↑\n"+strings.Repeat("a", 101)+" [peer-check:0]", expected, chunk, true)
		}},
	})
}

func assertChunksBounded(t *testing.T, chunks []string) {
	t.Helper()
	for _, chunk := range chunks {
		if len(chunk) > TypeChunkBytes || !utf8.ValidString(chunk) {
			t.Fatalf("chunk %q: %d bytes (limit %d), valid utf-8 %v", chunk, len(chunk), TypeChunkBytes, utf8.ValidString(chunk))
		}
	}
}

func assertChunksJoin(t *testing.T, chunks []string, message string) {
	t.Helper()
	if got := strings.Join(chunks, ""); got != message {
		t.Fatalf("joined chunks = %q, want %q", got, message)
	}
}

func TestTypeBody(t *testing.T) {
	runCases(t, []namedCase{
		{"test_probe_marker_is_absent_from_the_message", func(t *testing.T) {
			message := "body [peer-check:0] more body"
			marker := ComposerProbeMarker(message)
			if marker != " [peer-check:1]" {
				t.Fatalf("marker = %q, want %q", marker, " [peer-check:1]")
			}
			if strings.Contains(message, marker) {
				t.Fatalf("marker %q occurs in the message", marker)
			}
		}},
		{"test_chunks_preserve_utf8_without_exceeding_limit", func(t *testing.T) {
			message := strings.Repeat("a", TypeChunkBytes-1) + "€" + strings.Repeat("b", 901)
			chunks := TextChunks(message, TypeChunkBytes)
			assertChunksJoin(t, chunks, message)
			assertChunksBounded(t, chunks)
		}},
		{"test_chunks_start_at_word_boundaries", func(t *testing.T) {
			message := "first " + strings.Repeat("word ", 100) + "last"
			chunks := TextChunks(message, TypeChunkBytes)
			assertChunksJoin(t, chunks, message)
			for _, chunk := range chunks[1:] {
				if !strings.HasPrefix(chunk, " ") {
					t.Fatalf("chunk %q does not start at a word boundary", chunk)
				}
			}
		}},
		{"test_repeated_words_stay_in_bounded_events", func(t *testing.T) {
			message := "prefix " + strings.Repeat("foo ", 300) + "suffix"
			chunks := TextChunks(message, TypeChunkBytes)
			assertChunksJoin(t, chunks, message)
			if len(chunks) != 7 {
				t.Fatalf("len(chunks) = %d, want 7", len(chunks))
			}
			assertChunksBounded(t, chunks)
		}},
		{"test_overlong_unicode_token_uses_utf8_safe_bounded_events", func(t *testing.T) {
			message := strings.Repeat("ж", TypeChunkBytes)
			chunks := TextChunks(message, TypeChunkBytes)
			assertChunksJoin(t, chunks, message)
			if len(chunks) <= 1 {
				t.Fatalf("len(chunks) = %d, want > 1", len(chunks))
			}
			assertChunksBounded(t, chunks)
		}},
	})
}

var npmCodex = []string{
	"/Users/tester/.nvm/versions/node/v20.10.0/bin/node",
	"/Users/tester/.nvm/versions/node/v20.10.0/lib/node_modules/@openai/codex/bin/codex.js",
	"--dangerously-bypass-approvals-and-sandbox",
	"--search",
	"-c",
	`shell_environment_policy.set.AGTERM_SESSION_ID="session-full-id"`,
}

// Only the runs()/leading_command() assertions; the resolve/bind_pane halves of
// these Python tests are not ported here.
func TestPaneResolution(t *testing.T) {
	assertRuns := func(t *testing.T, want bool, foreground []string, command string) {
		t.Helper()
		if got := Runs(foreground, command); got != want {
			t.Fatalf("Runs(%q, %q) = %v, want %v", foreground, command, got, want)
		}
	}
	runCases(t, []namedCase{
		{"test_npm_installed_codex_is_recognised_by_its_script", func(t *testing.T) {
			assertRuns(t, true, npmCodex, "codex")
			assertRuns(t, false, npmCodex, "claude")
			for _, suffix := range []string{".mjs", ".cjs"} {
				assertRuns(t, true, []string{"node", "/opt/codex/bin/codex" + suffix}, "codex")
			}
			assertRuns(t, true, []string{"codex"}, "codex")
			assertRuns(t, true, []string{"claude"}, "claude")
		}},
		{"test_other_interpreter_scripts_are_not_the_agent", func(t *testing.T) {
			for _, foreground := range [][]string{
				{"node", "/opt/tools/something-else.js"},
				{"node", "/opt/codex/bin/run.js"},
				{"node", "/opt/bin/codex-helper.js"},
				{"node", "/opt/bin/codex.ts"},
				{"node", "--inspect", "/opt/bin/codex.js"},
			} {
				assertRuns(t, false, foreground, "codex")
			}
		}},
	})
}
