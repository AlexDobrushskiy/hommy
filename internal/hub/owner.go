package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/pairing"
	"github.com/AlexDobrushskiy/hommy/internal/version"
)

// ownerRow is the paired owner (zero when unpaired).
func (h *Hub) ownerRow() Owner {
	h.ownerMu.Lock()
	defer h.ownerMu.Unlock()
	return h.owner
}

// ownerChat is the owner's private chat (0 when unpaired): the only chat the
// hub sends to.
func (h *Hub) ownerChat() int64 { return h.ownerRow().ChatID }

// setOwner caches a pairing change already stored; a new owner gets the bot's
// commands set again.
func (h *Hub) setOwner(o Owner) {
	h.ownerMu.Lock()
	h.owner = o
	h.ownerMu.Unlock()
	h.tgMu.Lock()
	h.botSetUp = false
	h.tgMu.Unlock()
}

// dropped logs a rejected update: its id and why (a user id at most, never text).
func dropped(u *models.Update, why string) {
	log.Printf("dropped update %d: %s", u.ID, why)
}

// startArg reads a message's `/start <arg>` (or `/start@bot <arg>`).
func startArg(m *models.Message) (arg string, ok bool) {
	if m == nil {
		return "", false
	}
	cmd, rest, _ := strings.Cut(strings.TrimSpace(m.Text), " ")
	cmd, _, _ = strings.Cut(cmd, "@")
	if cmd != "/start" {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// pairingGate applies the pairing rules (master spec §4.3) before classify:
// on an unpaired hub it decides every update (only a private `/start <code>`
// with the pending code binds its sender; the rest is dropped); on a paired
// hub it drops a `/start <code>` from anyone but the owner. handled=false
// passes the update on. An error means nothing was decided (the poller
// retries the update).
func (h *Hub) pairingGate(ctx context.Context, u *models.Update) (bool, error) {
	o := h.ownerRow()
	m := u.Message
	arg, isStart := startArg(m)
	if o.Paired() {
		if isStart && arg != "" && (m.From == nil || m.From.ID != o.UserID || m.Chat.ID != o.ChatID) {
			from := int64(0)
			if m.From != nil {
				from = m.From.ID
			}
			dropped(u, fmt.Sprintf("pairing code from user %d on a paired hub", from))
			return true, nil
		}
		return false, nil
	}
	switch {
	case !isStart:
		dropped(u, "hub not paired")
		return true, nil
	case m.From == nil:
		dropped(u, "pairing: no sender")
		return true, nil
	case m.Chat.Type != models.ChatTypePrivate:
		dropped(u, fmt.Sprintf("pairing: user %d in chat %d type %q", m.From.ID, m.Chat.ID, m.Chat.Type))
		return true, nil
	case arg == "":
		dropped(u, fmt.Sprintf("pairing: user %d sent no code", m.From.ID))
		return true, nil
	}
	user := m.From.ID
	n, err := h.st.PairingAttempts(user)
	if err != nil {
		return true, err
	}
	if n >= pairing.MaxWrong {
		dropped(u, fmt.Sprintf("pairing: user %d ignored after %d wrong codes", user, n))
		return true, nil
	}
	if canon, ok := pairing.Normalize(arg); ok {
		owner := Owner{UserID: user, ChatID: m.Chat.ID, FirstName: m.From.FirstName, PairedAt: h.now()}
		claimed, err := h.st.ClaimPairing(pairing.Hash(canon), owner, h.now())
		if err != nil {
			return true, err
		}
		if claimed {
			h.setOwner(owner)
			log.Printf("paired with user %d", user)
			h.welcome(ctx, m.ID)
			return true, nil
		}
	}
	if n, err = h.st.AddPairingAttempt(user); err != nil {
		return true, err
	}
	dropped(u, fmt.Sprintf("pairing: wrong or expired code from user %d (%d/%d)", user, n, pairing.MaxWrong))
	return true, nil
}

// welcome is the new owner's first message: the pairing line, the help text
// and the reply keyboard; then the bot's commands (and menu button) are set.
func (h *Hub) welcome(ctx context.Context, replyTo int) {
	line := "✅ Paired. Send a text or a voice note."
	if name := h.botUsername(ctx); name != "" {
		line = "✅ Paired with @" + name + ". Send a text or a voice note."
	}
	h.say(ctx, h.ownerChat(), line+"\n\n"+helpText, replyTo)
	h.setupBot(ctx)
}

// setupBot sets the bot's commands and, with a Mini App URL, the owner's menu
// button; once per owner, and only when paired (the guard refuses otherwise).
func (h *Hub) setupBot(ctx context.Context) {
	chat := h.ownerChat()
	h.tgMu.Lock()
	done := h.botSetUp
	h.tgMu.Unlock()
	if chat == 0 || done {
		return
	}
	if err := h.tg.SetCommands(ctx, commands); err != nil {
		log.Printf("set commands: %v", err)
		return
	}
	if h.cfg.MiniAppURL != "" {
		if err := h.tg.SetMenuButton(ctx, chat, "Agents", h.cfg.MiniAppURL+"/board"); err != nil {
			log.Printf("menu button: %v", err)
		}
	}
	h.tgMu.Lock()
	h.botSetUp = chat == h.ownerChat()
	h.tgMu.Unlock()
}

// refreshTelegram checks the Bot API (getMe): it learns the bot's name and,
// when paired, sets the commands not set yet.
func (h *Hub) refreshTelegram(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	name, err := h.tg.Me(pctx)
	if err != nil {
		h.telegramFailed(err)
		log.Printf("telegram: getMe: %v", err)
		return
	}
	h.markTelegramOK()
	h.tgMu.Lock()
	h.botName = name
	h.tgMu.Unlock()
	h.setupBot(ctx)
}

// botUsername is the bot's username, asking Telegram when not known yet ("" if unreachable).
func (h *Hub) botUsername(ctx context.Context) string {
	h.tgMu.Lock()
	name := h.botName
	h.tgMu.Unlock()
	if name != "" {
		return name
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	name, err := h.tg.Me(pctx)
	if err != nil {
		h.telegramFailed(err)
		log.Printf("telegram: getMe: %v", err)
		return ""
	}
	h.tgMu.Lock()
	h.botName = name
	h.tgMu.Unlock()
	return name
}

// ---- HTTP: /api/pairing, /api/status (host key) ----

// hostAuth checks the host key (Bearer + X-Hommy-Host); 401 otherwise.
func (h *Hub) hostAuth(w http.ResponseWriter, r *http.Request) bool {
	name := r.Header.Get("X-Hommy-Host")
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !h.cfg.checkKey(name, key) {
		log.Printf("api %s: rejected host %q from %s", r.URL.Path, name, r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	return true
}

func loopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (h *Hub) pairingState() (pairing.State, error) {
	o := h.ownerRow()
	st := pairing.State{Paired: o.Paired(), OwnerChatID: o.ChatID, OwnerFirstName: o.FirstName}
	exp, ok, err := h.st.PendingPairing(h.now())
	if err != nil {
		return st, err
	}
	if ok {
		st.Pending, st.ExpiresAt = true, &exp
	}
	return st, nil
}

// servePairing: GET = the state; POST (loopback only) {} = a new code,
// {"reset":true} = no owner, {"owner_id":N} = bind N directly.
func (h *Hub) servePairing(w http.ResponseWriter, r *http.Request) {
	if !h.hostAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		if !loopback(r.RemoteAddr) {
			log.Printf("api pairing: refused from %s (loopback only)", r.RemoteAddr)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "pairing changes are made on the hub's Mac only"})
			return
		}
		var req pairing.Request
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: " + err.Error()})
			return
		}
		switch {
		case req.Reset:
			if err := h.st.ResetPairing(); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			h.setOwner(Owner{})
			log.Printf("pairing reset: no owner")
		case req.OwnerID < 0:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "owner_id must be positive"})
			return
		case req.OwnerID > 0:
			o := Owner{UserID: req.OwnerID, ChatID: req.OwnerID, PairedAt: h.now()}
			if err := h.st.SetOwner(o); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			h.setOwner(o)
			log.Printf("paired with user %d", o.UserID)
			h.setupBot(h.rootCtx())
		default:
			h.newCode(w, r)
			return
		}
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET or POST"})
		return
	}
	st, err := h.pairingState()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// newCode makes a pairing code (replacing a pending one); a paired hub
// refuses: re-pairing starts with a reset.
func (h *Hub) newCode(w http.ResponseWriter, r *http.Request) {
	if h.ownerRow().Paired() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already paired; reset first (hommy pair --reset)"})
		return
	}
	canon, err := pairing.Generate()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	exp := h.now().Add(h.cfg.PairingTTL)
	if err := h.st.NewPairingCode(pairing.Hash(canon), exp); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	code := pairing.Code{Code: pairing.Display(canon), ExpiresAt: exp}
	if name := h.botUsername(r.Context()); name != "" {
		code.Bot, code.Link = "@"+name, "https://t.me/"+name+"?start="+code.Code
	}
	log.Printf("pairing code made, valid until %s", exp.Local().Format(time.RFC3339))
	writeJSON(w, http.StatusOK, code)
}

// serveStatus is the hub at a glance for `hommy doctor` / `status`.
func (h *Hub) serveStatus(w http.ResponseWriter, r *http.Request) {
	if !h.hostAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	o := h.ownerRow()
	st := pairing.Status{Version: version.String(), Telegram: h.telegramState(), Paired: o.Paired(), Hosts: []pairing.Host{}}
	if o.Paired() {
		st.Owner = &pairing.Owner{FirstName: o.FirstName}
	}
	for _, name := range h.hostNames() {
		row, err := h.st.Host(name)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		hs := pairing.Host{Name: name, Online: h.connected(name), Version: row.Version, Checks: row.Heartbeat.Checks}
		if !row.LastSeen.IsZero() {
			seen := row.LastSeen
			hs.LastSeen = &seen
		}
		st.Hosts = append(st.Hosts, hs)
	}
	writeJSON(w, http.StatusOK, st)
}
