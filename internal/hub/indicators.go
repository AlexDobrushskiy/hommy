package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// The current target is shown three ways, all refreshed on every switch: the
// input box placeholder, the persistent reply keyboard (🎯 button + recent
// targets, attached to every bot message) and a pinned line edited in place.

const (
	placeholderMax = 64 // Telegram's input_field_placeholder limit
	buttonMax      = 40 // keyboard button label, in UTF-16 units
	recentKeep     = 6  // recent targets remembered per chat
	recentShown    = 2  // recent targets on the keyboard besides 🎯
	kbReissueAfter = 6 * 24 * time.Hour
)

// recent is a target the owner talked to, with the label it had then.
type recent struct {
	Target string `json:"t"`
	Label  string `json:"l"`
}

// kbState is the chat's keyboard generation: the picker token in the 🎯
// button is bound to Serial, so a switch supersedes older tokens.
type kbState struct {
	Serial int64 `json:"serial"`
	Issued int64 `json:"issued"` // unix seconds
}

func kvKey(prefix string, chatID int64) string { return prefix + ":" + strconv.FormatInt(chatID, 10) }

// ---- labels ----

// label is a target's human name with its host: "vault · mac1",
// "trip-planner · mac2", "agent-briefs · mac1" (p@), "new interactive: x · mac1".
func (h *Hub) label(t Target) string {
	switch t.Kind {
	case KindVault:
		return "vault · " + t.Host
	case KindP:
		return dirName(t.Dir) + " · " + t.Host
	case KindSession:
		if s, ok := h.liveSession(t); ok {
			return sessionName(s) + " · " + t.Host
		}
		if l := h.recentLabel(t.String()); l != "" {
			return l
		}
		return "session " + shortID(t.Dir) + " · " + t.Host
	case KindNew:
		return "new " + t.Mode + ": " + dirName(t.Dir) + " · " + t.Host
	}
	return t.String()
}

func dirName(dir string) string {
	if dir == "~" || dir == "/" {
		return dir
	}
	return path.Base(strings.TrimRight(dir, "/"))
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// sessionName is the agterm session name, else the agent slug, else the id.
func sessionName(s agtermSession) string {
	switch {
	case s.Name != "":
		return s.Name
	case s.BriefDir != "":
		return dirName(s.BriefDir)
	}
	return "session " + shortID(s.ID)
}

// recentLabel is the label a target had when last used ("" if never).
func (h *Hub) recentLabel(target string) string {
	for _, r := range h.recents(h.ownerChat()) {
		if r.Target == target {
			return r.Label
		}
	}
	return ""
}

// ---- UTF-16 aware text helpers (Telegram counts entities and limits in UTF-16 units) ----

func u16len(s string) int { return len(utf16.Encode([]rune(s))) }

// truncate cuts s to at most max UTF-16 units, ending in "…" when cut.
func truncate(s string, max int) string {
	if u16len(s) <= max {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		w := len(utf16.Encode([]rune{r}))
		if n+w > max-1 {
			break
		}
		b.WriteRune(r)
		n += w
	}
	return strings.TrimRight(b.String(), " ") + "…"
}

// codeText builds "<before><code>…</code><after>" with the code entity's UTF-16 offsets.
func codeText(before, code, after string) (string, []models.MessageEntity) {
	return before + code + after, []models.MessageEntity{{Type: models.MessageEntityTypeCode,
		Offset: u16len(before), Length: u16len(code)}}
}

// ---- recents ----

func (h *Hub) recents(chatID int64) []recent {
	raw, err := h.st.Get(kvKey("recent", chatID))
	if err != nil || raw == "" {
		return nil
	}
	var rs []recent
	if err := json.Unmarshal([]byte(raw), &rs); err != nil {
		log.Printf("recent targets: %v", err)
	}
	return rs
}

func (h *Hub) setRecents(chatID int64, rs []recent) error {
	if len(rs) > recentKeep {
		rs = rs[:recentKeep]
	}
	return h.st.Set(kvKey("recent", chatID), mustJSON(rs))
}

// pushRecent puts target first in the chat's recent list.
func (h *Hub) pushRecent(chatID int64, target, label string) error {
	rs := []recent{{Target: target, Label: label}}
	for _, r := range h.recents(chatID) {
		if r.Target != target {
			rs = append(rs, r)
		}
	}
	return h.setRecents(chatID, rs)
}

func (h *Hub) dropRecent(chatID int64, target string) error {
	var rs []recent
	for _, r := range h.recents(chatID) {
		if r.Target != target {
			rs = append(rs, r)
		}
	}
	return h.setRecents(chatID, rs)
}

// recentButton is one plain keyboard button: pressing it sends Text.
type recentButton struct {
	Text   string
	Target string
	Label  string
}

// recentButtons are the recent targets the keyboard shows next to 🎯, with
// unique button texts (the hub matches a press by its exact text).
func (h *Hub) recentButtons(chatID int64, current string) []recentButton {
	var out []recentButton
	seen := map[string]bool{h.targetButtonText(chatID, current): true}
	for _, r := range h.recents(chatID) {
		if r.Target == current || len(out) == recentShown {
			continue
		}
		text := truncate(r.Label, buttonMax)
		if seen[text] {
			text = truncate(r.Label, buttonMax-9) + " #" + shortID(strings.TrimPrefix(r.Target, "session@"))
		}
		seen[text] = true
		out = append(out, recentButton{Text: text, Target: r.Target, Label: r.Label})
	}
	return out
}

func (h *Hub) targetButtonText(chatID int64, current string) string {
	t, err := ParseTarget(current)
	if err != nil {
		return "🎯 ?"
	}
	return "🎯 " + truncate(h.label(t), buttonMax) + " ▾"
}

// ---- keyboard ----

// keyboardState returns the chat's keyboard generation, reissuing the token
// before it ages out (hard cap pickerTokenTTL).
func (h *Hub) keyboardState(chatID int64, bump bool) (kbState, error) {
	h.kbMu.Lock()
	defer h.kbMu.Unlock()
	k, err := h.loadKeyboardState(chatID)
	if err != nil {
		return k, err
	}
	now := h.now()
	if bump || k.Serial == 0 || now.Sub(time.Unix(k.Issued, 0)) >= kbReissueAfter {
		k = kbState{Serial: k.Serial + 1, Issued: now.Unix()}
		if err := h.st.Set(kvKey("kb", chatID), mustJSON(k)); err != nil {
			return k, err
		}
	}
	return k, nil
}

// loadKeyboardState reads the chat's keyboard generation without issuing one
// (checking a token must not supersede the keyboard the owner is looking at).
func (h *Hub) loadKeyboardState(chatID int64) (kbState, error) {
	var k kbState
	raw, err := h.st.Get(kvKey("kb", chatID))
	if err != nil || raw == "" {
		return k, err
	}
	return k, json.Unmarshal([]byte(raw), &k)
}

// markup is the reply keyboard every bot message carries: 🎯 <target> ▾
// (opens the picker) + the 2 most recent other targets, with the target in
// the input placeholder.
func (h *Hub) markup(chatID int64) models.ReplyMarkup {
	if chatID == 0 || chatID != h.ownerChat() {
		return nil
	}
	t, err := h.currentTarget(chatID)
	if err != nil {
		log.Printf("keyboard: %v", err)
		return nil
	}
	k, err := h.keyboardState(chatID, false)
	if err != nil {
		log.Printf("keyboard: %v", err)
		return nil
	}
	cur := t.String()
	pick := models.KeyboardButton{Text: h.targetButtonText(chatID, cur)}
	if h.cfg.MiniAppURL != "" {
		pick.WebApp = &models.WebAppInfo{URL: h.cfg.MiniAppURL + "/picker?t=" + h.pickerToken(chatID, k)}
	}
	row := []models.KeyboardButton{pick}
	for _, b := range h.recentButtons(chatID, cur) {
		row = append(row, models.KeyboardButton{Text: b.Text})
	}
	return &models.ReplyKeyboardMarkup{Keyboard: [][]models.KeyboardButton{row}, IsPersistent: true,
		ResizeKeyboard: true, InputFieldPlaceholder: placeholder(h.label(t))}
}

func placeholder(label string) string { return truncate("→ "+label, placeholderMax) }

// ---- switching ----

// switchTarget makes t the chat's target and refreshes all three indicators:
// a "🎯 Now talking to X" message carrying the new keyboard (new token) and
// the pinned line.
func (h *Hub) switchTarget(ctx context.Context, chatID int64, t Target, note string) error {
	lbl, err := h.applyTarget(ctx, chatID, t)
	if err != nil {
		return err
	}
	text, ents := codeText("🎯 Now talking to ", lbl, note)
	if t.Kind == KindP || t.Kind == KindNew {
		// The label is only the folder's name: show which folder, also in code.
		spec := t.String()
		text += "\n"
		ents = append(ents, models.MessageEntity{Type: models.MessageEntityTypeCode, Offset: u16len(text), Length: u16len(spec)})
		text += spec
	}
	text += linkLine(h.sessionLink(t))
	h.sayRich(ctx, chatID, text, ents, 0)
	h.updatePin(ctx, chatID, lbl)
	return nil
}

// applyTarget sets the target, its recents and a new keyboard generation; the
// caller posts the message that carries the new keyboard and updates the pin.
func (h *Hub) applyTarget(ctx context.Context, chatID int64, t Target) (string, error) {
	lbl := h.label(t)
	if prev, err := h.currentTarget(chatID); err == nil && prev.String() != t.String() {
		// The target being left becomes a recent (the default one was never "switched to").
		if err := h.pushRecent(chatID, prev.String(), h.label(prev)); err != nil {
			return "", err
		}
	}
	if err := h.st.SetChatTarget(chatID, t.String(), h.now()); err != nil {
		return "", err
	}
	if err := h.pushRecent(chatID, t.String(), lbl); err != nil {
		return "", err
	}
	if _, err := h.keyboardState(chatID, true); err != nil {
		return "", err
	}
	return lbl, nil
}

// sayRich sends a notice with entities and records it.
func (h *Hub) sayRich(ctx context.Context, chatID int64, text string, ents []models.MessageEntity, replyTo int) int {
	id, err := h.tg.SendOut(ctx, chatID, Out{Text: text, Entities: ents, ReplyTo: replyTo, NoPreview: strings.Contains(text, "🔗 ")})
	if err != nil {
		log.Printf("send: %v", err)
		return 0
	}
	h.record(Message{ChatID: chatID, MsgID: id, Direction: "out", Kind: "notice"})
	return id
}

// ---- pinned line ----

func pinText(label string) (string, []models.MessageEntity) {
	return codeText("🎯 Talking to: ", label, "")
}

func (h *Hub) pinnedID(chatID int64) int {
	raw, _ := h.st.Get(kvKey("pin", chatID))
	id, _ := strconv.Atoi(raw)
	return id
}

// ensurePin creates the pinned line once per chat, and re-pins it while an
// earlier pinChatMessage failed.
func (h *Hub) ensurePin(ctx context.Context, chatID int64) {
	if chatID == 0 || chatID != h.ownerChat() {
		return
	}
	if id := h.pinnedID(chatID); id != 0 {
		h.pinLine(ctx, chatID, id)
		return
	}
	t, err := h.currentTarget(chatID)
	if err != nil {
		return
	}
	h.createPin(ctx, chatID, h.label(t))
}

// pinLine pins line id unless it is already known to be pinned.
func (h *Hub) pinLine(ctx context.Context, chatID int64, id int) {
	key := kvKey("pinned", chatID)
	if raw, _ := h.st.Get(key); raw == strconv.Itoa(id) {
		return
	}
	if err := h.tg.Pin(ctx, chatID, id); err != nil {
		log.Printf("pinned line: pin %d: %v (retried on the next message)", id, err)
		return
	}
	if err := h.st.Set(key, strconv.Itoa(id)); err != nil {
		log.Printf("pinned line: %v", err)
	}
}

// updatePin edits the pinned line in place, re-creating it when it's gone.
func (h *Hub) updatePin(ctx context.Context, chatID int64, label string) {
	id := h.pinnedID(chatID)
	if id == 0 {
		h.createPin(ctx, chatID, label)
		return
	}
	text, ents := pinText(label)
	err := h.tg.EditText(ctx, chatID, id, text, ents)
	switch {
	case err == nil, isNotModified(err):
	case errors.Is(err, bot.ErrorBadRequest):
		log.Printf("pinned line %d: %v; pinning a new one", id, err)
		h.createPin(ctx, chatID, label)
	default:
		log.Printf("pinned line %d: %v", id, err)
	}
}

func isNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}

// createPin sends and pins the line. The "pinned" service message Telegram
// adds comes back as an update and is deleted there (pin_service).
func (h *Hub) createPin(ctx context.Context, chatID int64, label string) {
	old := h.pinnedID(chatID)
	text, ents := pinText(label)
	id, err := h.tg.SendBare(ctx, chatID, text, ents)
	if err != nil {
		log.Printf("pinned line: send: %v", err)
		return
	}
	if old != 0 {
		if err := h.tg.Unpin(ctx, chatID, old); err != nil {
			log.Printf("pinned line: unpin %d: %v", old, err)
		}
	}
	h.record(Message{ChatID: chatID, MsgID: id, Direction: "out", Kind: "pin"})
	// Stored even if pinning fails: the line is still edited in place, never re-sent per message.
	if err := h.st.Set(kvKey("pin", chatID), strconv.Itoa(id)); err != nil {
		log.Printf("pinned line: %v", err)
	}
	h.pinLine(ctx, chatID, id)
}

// ---- dead sessions ----

// sessionGone reports a session target whose host reports a fresh agterm
// snapshot without it. An offline host or one without agterm data can't
// tell, so the session counts as alive (messages queue for it).
func (h *Hub) sessionGone(t Target) bool {
	if t.Kind != KindSession || !h.connected(t.Host) {
		return false
	}
	snap, ok := h.snapshot(t.Host)
	if !ok {
		return false
	}
	_, n := findSession(snap.Sessions, t.Dir)
	return n == 0
}

// goneNotice tells the owner a target is gone and drops it from the recents (the
// notice carries the keyboard without it, 🎯 opens the picker).
func (h *Hub) goneNotice(ctx context.Context, chatID int64, target, label string) error {
	if err := h.dropRecent(chatID, target); err != nil {
		return err
	}
	text, ents := codeText("⚠️ ", label, " is gone (session closed). Tap 🎯 to pick another.")
	h.sayRich(ctx, chatID, text, ents, 0)
	return nil
}
