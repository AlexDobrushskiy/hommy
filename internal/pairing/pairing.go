// Package pairing is the hub's one-time owner code (master spec §4.3) and the
// HTTP client the CLI uses for the hub's /api/pairing and /api/status.
package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// alphabet is Crockford's base32 (no I, L, O, U).
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Len is a code's length in characters (40 bits).
const Len = 8

// DefaultTTL is how long a code stays valid when PAIRING_TTL is unset.
const DefaultTTL = 30 * time.Minute

// MaxWrong is how many wrong codes one user may send before their /start is
// ignored until a new code is made.
const MaxWrong = 5

// Generate returns a fresh code in canonical form (8 characters, no dash).
func Generate() (string, error) {
	b := make([]byte, Len)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, Len)
	for i, v := range b {
		out[i] = alphabet[v&31]
	}
	return string(out), nil
}

// Display is the code as shown to people: XXXX-XXXX.
func Display(canon string) string {
	if len(canon) != Len {
		return canon
	}
	return canon[:4] + "-" + canon[4:]
}

// Normalize turns what a person typed into the canonical code: case-insensitive,
// dashes ignored, O→0 and I/L→1 (Crockford). ok=false when it is not a code.
func Normalize(s string) (string, bool) {
	s = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
	if len(s) != Len {
		return "", false
	}
	out := make([]byte, Len)
	for i := 0; i < Len; i++ {
		c := s[i]
		switch c {
		case 'O':
			c = '0'
		case 'I', 'L':
			c = '1'
		}
		if strings.IndexByte(alphabet, c) < 0 {
			return "", false
		}
		out[i] = c
	}
	return string(out), true
}

// Hash is what the hub stores for a canonical code.
func Hash(canon string) string {
	sum := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(sum[:])
}
