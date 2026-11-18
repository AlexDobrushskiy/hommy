// Package claude runs `claude -p` for the vault conversation and other -p targets.
package claude

import (
	"fmt"
	"strings"
)

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
