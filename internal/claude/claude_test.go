package claude

import (
	"strings"
	"testing"
)

func TestSystemPrompt(t *testing.T) {
	if p := SystemPrompt(false, 400, []string{"ru"}); strings.Contains(p, "VOICE NOTE") {
		t.Error("tts disabled must omit the voice instruction")
	}
	p := SystemPrompt(true, 400, []string{"ru", "ru-coach"})
	if !strings.Contains(p, "longer than 400 characters") || !strings.Contains(p, "en (default), ru, ru-coach") {
		t.Errorf("prompt: %s", p)
	}
	if strings.Contains(SystemPrompt(true, 400, nil), "lang=") {
		t.Error("no extra langs: no lang clause")
	}
}
