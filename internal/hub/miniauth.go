package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mini App auth. Telegram gives keyboard web_app buttons no initData, so the
// 🎯 picker authenticates with a read-only token the hub puts in the button
// URL; writes go back through sendData (web_app_data, Telegram-authenticated).
// The menu button and inline "Open" buttons carry initData.

const (
	pickerTokenTTL = 7 * 24 * time.Hour // hard cap; the keyboard reissues before it
	initDataMaxAge = time.Hour
)

// pickerSecret keys the picker tokens. Derived from the bot token, so a
// token rotation also invalidates every picker link.
func (h *Hub) pickerSecret() []byte {
	m := hmac.New(sha256.New, []byte("hommy picker token v1"))
	m.Write([]byte(h.cfg.TelegramToken))
	return m.Sum(nil)
}

// pickerToken is "<chat>.<serial>.<issued>.<mac>": bound to the chat and its
// keyboard generation (a switch supersedes it) and capped at pickerTokenTTL.
func (h *Hub) pickerToken(chatID int64, k kbState) string {
	body := fmt.Sprintf("%d.%d.%d", chatID, k.Serial, k.Issued)
	m := hmac.New(sha256.New, h.pickerSecret())
	m.Write([]byte("picker." + body))
	return body + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// checkPickerToken returns the token's chat when it is authentic, current
// (its keyboard generation is the chat's latest) and younger than the cap.
func (h *Hub) checkPickerToken(tok string) (int64, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 4 {
		return 0, fmt.Errorf("malformed token")
	}
	chatID, err1 := strconv.ParseInt(parts[0], 10, 64)
	serial, err2 := strconv.ParseInt(parts[1], 10, 64)
	issued, err3 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, fmt.Errorf("malformed token")
	}
	want := h.pickerToken(chatID, kbState{Serial: serial, Issued: issued})
	if !hmac.Equal([]byte(want), []byte(tok)) {
		return 0, fmt.Errorf("bad signature")
	}
	if chatID == 0 || chatID != h.ownerChat() {
		return 0, fmt.Errorf("wrong chat")
	}
	if age := h.now().Sub(time.Unix(issued, 0)); age >= pickerTokenTTL || age < -time.Minute {
		return 0, fmt.Errorf("expired")
	}
	h.kbMu.Lock()
	k, err := h.loadKeyboardState(chatID)
	h.kbMu.Unlock()
	if err != nil {
		return 0, err
	}
	if k.Serial != serial {
		return 0, fmt.Errorf("superseded")
	}
	return chatID, nil
}

// checkInitData validates Telegram Mini App initData: the hash is
// HMAC_SHA256(data_check_string, HMAC_SHA256(bot token, key "WebAppData")),
// auth_date is under an hour old and the user is the owner.
func (h *Hub) checkInitData(raw string) (int64, error) {
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return 0, fmt.Errorf("malformed initData")
	}
	hash := vals.Get("hash")
	if hash == "" {
		return 0, fmt.Errorf("no hash")
	}
	var keys []string
	for k := range vals {
		if k != "hash" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	lines := make([]string, len(keys))
	for i, k := range keys {
		lines[i] = k + "=" + vals.Get(k)
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(h.cfg.TelegramToken))
	m := hmac.New(sha256.New, secret.Sum(nil))
	m.Write([]byte(strings.Join(lines, "\n")))
	got, err := hex.DecodeString(hash)
	if err != nil || !hmac.Equal(got, m.Sum(nil)) {
		return 0, fmt.Errorf("bad hash")
	}
	at, err := strconv.ParseInt(vals.Get("auth_date"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("no auth_date")
	}
	if age := h.now().Sub(time.Unix(at, 0)); age >= initDataMaxAge || age < -5*time.Minute {
		return 0, fmt.Errorf("auth_date too old")
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal([]byte(vals.Get("user")), &user) != nil || user.ID == 0 || user.ID != h.ownerRow().UserID {
		return 0, fmt.Errorf("wrong user")
	}
	return user.ID, nil
}

// rateLimit is a fixed one-minute window per key (token chat, user, or
// client address for unauthenticated calls).
type rateLimit struct {
	mu    sync.Mutex
	limit int
	win   map[string]window
}

type window struct {
	start time.Time
	n     int
}

func newRateLimit(limit int) *rateLimit { return &rateLimit{limit: limit, win: map[string]window{}} }

func (r *rateLimit) allow(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.win[key]
	if now.Sub(w.start) >= time.Minute {
		w = window{start: now}
	}
	w.n++
	r.win[key] = w
	if len(r.win) > 10000 { // bounded memory: drop expired windows
		for k, v := range r.win {
			if now.Sub(v.start) >= time.Minute {
				delete(r.win, k)
			}
		}
	}
	return w.n <= r.limit
}
