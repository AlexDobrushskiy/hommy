package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/go-telegram/bot/models"
)

// pickerChoice is what the picker page sends with Telegram.WebApp.sendData.
type pickerChoice struct {
	V      int    `json:"v"`
	Target string `json:"target"`
	Fresh  bool   `json:"fresh,omitempty"` // vault: start a new conversation
}

// targetUI handles the messages that switch the target instead of going to
// it: the picker's web_app_data and a press of a recent-target button.
// Both messages are deleted, so the chat keeps only the switch notice.
func (h *Hub) targetUI(ctx context.Context, msg *models.Message) (bool, error) {
	chatID := msg.Chat.ID
	if msg.WebAppData != nil {
		h.deleteMsg(ctx, chatID, msg.ID)
		return true, h.pickerSwitch(ctx, chatID, msg.WebAppData.Data)
	}
	if msg.Text == "" || msg.ReplyToMessage != nil {
		return false, nil
	}
	cur, err := h.currentTarget(chatID)
	if err != nil {
		return true, err
	}
	for _, b := range h.recentButtons(chatID, cur.String()) {
		if msg.Text != b.Text {
			continue
		}
		h.deleteMsg(ctx, chatID, msg.ID)
		t, err := ParseTarget(b.Target)
		if err != nil {
			return true, h.dropRecent(chatID, b.Target)
		}
		if h.sessionGone(t) {
			return true, h.goneNotice(ctx, chatID, b.Target, b.Label)
		}
		return true, h.switchTarget(ctx, chatID, t, "")
	}
	if msg.Text == h.targetButtonText(chatID, cur.String()) {
		// Only reachable without a Mini App URL: the 🎯 button is then plain text.
		h.say(ctx, chatID, "🎯 the picker isn't configured here; use /target <spec>", msg.ID)
		return true, nil
	}
	return false, nil
}

func (h *Hub) deleteMsg(ctx context.Context, chatID int64, msgID int) {
	if err := h.tg.Delete(ctx, chatID, msgID); err != nil {
		log.Printf("delete message %d: %v", msgID, err)
	}
}

// pickerSwitch applies the picker's choice.
func (h *Hub) pickerSwitch(ctx context.Context, chatID int64, data string) error {
	var c pickerChoice
	if err := json.Unmarshal([]byte(data), &c); err != nil || c.V != 1 {
		h.say(ctx, chatID, "⚠️ picker: unreadable choice", 0)
		return nil
	}
	t, err := h.validTarget(c.Target)
	if err == nil {
		err = h.checkNewDir(t) // /target new@… leaves the folder to the host
	}
	if err != nil {
		h.say(ctx, chatID, "⚠️ picker: "+err.Error(), 0)
		return nil
	}
	if h.sessionGone(t) {
		return h.goneNotice(ctx, chatID, t.String(), h.label(t))
	}
	note := ""
	if c.Fresh && t.Kind == KindVault {
		if _, err := h.st.NewGen(t.String(), h.now()); err != nil {
			return err
		}
		note = " (new conversation)"
	}
	return h.switchTarget(ctx, chatID, t, note)
}

// validTarget parses a target and checks its host is one of ours.
func (h *Hub) validTarget(spec string) (Target, error) {
	t, err := ParseTarget(spec)
	if err != nil {
		return t, err
	}
	if _, ok := h.cfg.HostKeys[t.Host]; !ok {
		return t, fmt.Errorf("unknown host %q (known: %s)", t.Host, strings.Join(h.hostNames(), ", "))
	}
	if s, ok := h.liveSession(t); ok {
		t.Dir = strings.ToUpper(s.ID) // a short id (as /status shows it) → the full one
	}
	return t, nil
}

func (h *Hub) targetCommand(ctx context.Context, chatID int64, args string) error {
	if args == "" {
		t, err := h.currentTarget(chatID)
		if err != nil {
			return err
		}
		text, ents := codeText("🎯 Talking to ", h.label(t), "\n"+t.String()+"\nusage: /target "+targetUsage)
		h.sayRich(ctx, chatID, text, ents, 0)
		return nil
	}
	t, err := h.validTarget(args)
	if err != nil {
		h.say(ctx, chatID, "⚠️ "+err.Error(), 0)
		return nil
	}
	if h.sessionGone(t) {
		return h.goneNotice(ctx, chatID, t.String(), h.label(t))
	}
	return h.switchTarget(ctx, chatID, t, "")
}
