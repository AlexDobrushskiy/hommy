// Package claude runs `claude -p` for the vault conversation and other -p targets.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/textx"
)

// Options configures one `claude -p` run.
type Options struct {
	Bin                string
	Dir                string
	Timeout            time.Duration
	AppendSystemPrompt string // "" = no --append-system-prompt
}

type result struct {
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
}

const telegramSystemPrompt = `The user is messaging you from Telegram on their phone. Replies are delivered as plain-text Telegram messages: no markdown headers, no tables, no bullet-list walls, no code fences unless literally sharing code. Write like a concise chat reply — short sentences, minimal structure, noticeably more brief than usual. When a message starts with a bracketed tag like [voice message] it notes the original form the user sent.`

const ttsPromptTemplate = `

VOICE NOTE: If your reply is longer than %d characters, append — as the very last thing in the message — a spoken version of it wrapped in <tts> and </tts> tags. The spoken version is a full conversational retelling of the ENTIRE reply in natural spoken prose, not a summary: carry over all the substance. Never read URLs, file paths, code, or markdown syntax aloud — describe them in words instead. Nothing may appear after the closing </tts> tag. For replies at or under that length, omit the block entirely.%s`

// ttsLangPromptTemplate is appended only when a non-default language backend
// exists, so a single-language deployment keeps the shorter instruction.
const ttsLangPromptTemplate = ` Write the spoken version in the same language you wrote the reply in, and tag that language on the opening marker: <tts lang="ru">. A bare <tts> means English. Languages with a voice available: en (default), %s. If your reply is in any other language, write the spoken version in English instead — there is no voice for it.`

// SystemPrompt is the Telegram-style prompt, plus the voice-note instruction
// when voice replies are enabled. extraLangs are the non-English TTS languages.
func SystemPrompt(ttsEnabled bool, ttsMinChars int, extraLangs []string) string {
	if !ttsEnabled {
		return telegramSystemPrompt
	}
	langClause := ""
	if len(extraLangs) > 0 {
		langClause = fmt.Sprintf(ttsLangPromptTemplate, strings.Join(extraLangs, ", "))
	}
	return telegramSystemPrompt + fmt.Sprintf(ttsPromptTemplate, ttsMinChars, langClause)
}

// Run executes claude -p in opts.Dir and returns (reply, session id).
// A --resume against a vanished session is retried once without --resume.
// On error, the returned session id may still be non-empty (when claude ran
// but reported an error) and should be persisted.
func Run(ctx context.Context, opts Options, sessionID, prompt string) (string, string, error) {
	out, newID, err := runOnce(ctx, opts, sessionID, prompt)
	if err != nil && sessionID != "" && strings.Contains(strings.ToLower(err.Error()), "no conversation found") {
		return runOnce(ctx, opts, "", prompt)
	}
	return out, newID, err
}

func runOnce(ctx context.Context, opts Options, sessionID, prompt string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	args := []string{"-p", "--output-format", "json", "--dangerously-skip-permissions"}
	if opts.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.AppendSystemPrompt)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	cmd := exec.CommandContext(ctx, opts.Bin, args...)
	cmd.Dir = opts.Dir
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", "", fmt.Errorf("claude timed out after %s", opts.Timeout)
		}
		if ctx.Err() == context.Canceled {
			return "", "", fmt.Errorf("claude run cancelled")
		}
		return "", "", fmt.Errorf("claude failed: %v: %s", err, textx.Tail(stderr.String(), 500))
	}
	var res result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return "", "", fmt.Errorf("bad claude output: %v: %s", err, textx.Tail(stdout.String(), 500))
	}
	if res.IsError {
		return "", res.SessionID, fmt.Errorf("claude error: %s", textx.Tail(res.Result, 500))
	}
	return res.Result, res.SessionID, nil
}
