package hook

import (
	"testing"

	"github.com/AlexDobrushskiy/hommy/internal/sanitize"
)

// The typed prefix the host writes and the relay regex that opens a Telegram
// turn agree; a local prompt or another bridge's prefix opens none.
func TestTypedPrefixAndRelayRegexAgree(t *testing.T) {
	for _, c := range []struct {
		id    int64
		quote string
	}{{42, ""}, {9001, `🤖 probe asks "which?" [x]`}, {7, "a\nb"}} {
		line := sanitize.Prefix(c.id, c.quote) + "hello"
		m := telegramRe.FindStringSubmatch(line)
		if m == nil || m[1] != itoa(c.id) {
			t.Errorf("%q: %v", line, m)
		}
	}
	for _, local := range []string{"hello", "[Telegram] x", "[Telegram \u2190 Someone · tg-5] x", "please see [Telegram · tg-5] x"} {
		if telegramRe.MatchString(local) {
			t.Errorf("%q opened a Telegram turn", local)
		}
	}
}

func itoa(n int64) string {
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
