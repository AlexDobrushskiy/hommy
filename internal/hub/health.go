package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/version"
)

func (h *Hub) healthLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	lastPing := time.Time{}
	for {
		h.checkHealth(ctx)
		if time.Since(lastPing) >= time.Minute {
			lastPing = time.Now()
			h.refreshTelegram(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// checkHealth alerts once per outage when a host has been silent for
// SilentAfter, and tells the owner once when messages wait for it longer than
// QueuedNotice. The "is back" line comes from register.
func (h *Hub) checkHealth(ctx context.Context) {
	h.healthMu.Lock()
	defer h.healthMu.Unlock()
	now := h.now()
	for _, name := range h.hostNames() {
		if h.connected(name) {
			continue
		}
		row, err := h.st.Host(name)
		if err != nil {
			log.Printf("health %s: %v", name, err)
			continue
		}
		// Silence counts from the hub's own start at the earliest, so hosts get
		// a chance to reconnect after a hub restart before anyone is alerted.
		since := maxTime(row.LastSeen, h.started)
		alerted, noticed := row.SilentAlerted, row.QueuedNoticed
		// A host that never connected isn't silent, just not set up yet (/status
		// says "never seen"); messages queued for it still get the notice below.
		if !alerted && !row.LastSeen.IsZero() && now.Sub(since) >= h.cfg.SilentAfter {
			h.say(ctx, h.ownerChat(), fmt.Sprintf("🔴 host %s silent for %s (last seen %s)",
				name, shortDur(now.Sub(since)), row.LastSeen.Local().Format("15:04:05")), 0)
			alerted = true
		}
		if !noticed {
			if j, err := h.st.OldestWaiting(name); err == nil && j != nil && now.Sub(j.CreatedAt) >= h.cfg.QueuedNotice {
				n, _ := h.st.QueueDepth(name)
				h.say(ctx, j.ChatID, fmt.Sprintf("%s is offline; queued (%d waiting)", name, n), j.MsgID)
				noticed = true
			}
		}
		if alerted != row.SilentAlerted || noticed != row.QueuedNoticed {
			if err := h.st.SetHostFlags(name, alerted, noticed); err != nil {
				log.Printf("health %s: %v", name, err)
			}
		}
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// shortDur renders 4s / 2m5s / 3h10m.
func shortDur(d time.Duration) string {
	if d < time.Hour {
		return d.Round(time.Second).String()
	}
	return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
}

// statusRich is /status with each session linked to its claude.ai page when known.
func (h *Hub) statusRich(chatID int64) (string, []models.MessageEntity) {
	now := h.now()
	var b richText
	fmt.Fprintf(&b, "hub: up %s · %s\n", shortDur(now.Sub(h.started)), version.String())
	if t, err := h.currentTarget(chatID); err == nil {
		gen, _ := h.st.CurrentGen(t.String())
		sid, _ := h.st.SessionID(t.String(), gen)
		if sid == "" {
			sid = "(none)"
		}
		b.WriteString("target: ")
		b.link(t.String(), h.sessionLink(t))
		fmt.Fprintf(&b, "\nsession: %s\n", sid)
		if raw, _ := h.st.Get("last:" + t.String()); raw != "" {
			var lr lastRun
			if json.Unmarshal([]byte(raw), &lr) == nil {
				fmt.Fprintf(&b, "last run: %s\n", lr.At)
				if lr.Error != "" {
					fmt.Fprintf(&b, "last error: %s\n", lr.Error)
				}
			}
		}
	}
	total := 0
	b.WriteString("hosts:\n")
	for _, name := range h.hostNames() {
		row, _ := h.st.Host(name)
		q, _ := h.st.QueueDepth(name)
		total += q
		state := "offline"
		if h.connected(name) {
			state = "online"
		}
		seen := "never seen"
		if !row.LastSeen.IsZero() {
			seen = "last seen " + shortDur(now.Sub(row.LastSeen)) + " ago"
		}
		ver := row.Version
		if ver == "" {
			ver = "?"
		}
		fmt.Fprintf(&b, "• %s: %s · %s · v %s · claude sessions %d · running %d · queue %d\n",
			name, state, seen, ver, row.Heartbeat.ClaudeSessions, row.Heartbeat.RunningJobs, q)
		writeSessionLines(&b, row.Heartbeat.Agterm, h.hookLinks(name))
	}
	fmt.Fprintf(&b, "queue: %d", total)
	return b.String(), b.ents
}

type healthz struct {
	OK       bool            `json:"ok"`
	DB       bool            `json:"db"`
	Telegram bool            `json:"telegram"`
	Hosts    map[string]bool `json:"hosts"` // name → connected
}

// serveHealthz is the liveness check: 200 when the DB answers and the Bot API was
// reachable within the last 3 minutes. Host state is informational.
func (h *Hub) serveHealthz(w http.ResponseWriter, r *http.Request) {
	hz := healthz{DB: h.st.Ping() == nil, Hosts: map[string]bool{}}
	h.tgMu.Lock()
	hz.Telegram = !h.tgOK.IsZero() && h.now().Sub(h.tgOK) < 3*time.Minute
	h.tgMu.Unlock()
	for _, n := range h.hostNames() {
		hz.Hosts[n] = h.connected(n)
	}
	hz.OK = hz.DB && hz.Telegram
	w.Header().Set("Content-Type", "application/json")
	if !hz.OK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(hz)
}

func (h *Hub) addTyping(chatID int64) {
	if chatID == 0 { // an API job (console peek): no chat to type in
		return
	}
	h.mu.Lock()
	h.typing[chatID]++
	h.mu.Unlock()
}

func (h *Hub) removeTyping(chatID int64) {
	h.mu.Lock()
	if h.typing[chatID] > 1 {
		h.typing[chatID]--
	} else {
		delete(h.typing, chatID)
	}
	h.mu.Unlock()
}

// typingLoop keeps "typing…" up while a run is going, as the bot always has.
func (h *Hub) typingLoop(ctx context.Context) {
	t := time.NewTicker(4500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.mu.Lock()
		var chats []int64
		for c := range h.typing {
			chats = append(chats, c)
		}
		h.mu.Unlock()
		for _, c := range chats {
			if !h.drafting(c) {
				h.tg.Typing(ctx, c)
			}
		}
	}
}
