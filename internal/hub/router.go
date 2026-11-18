package hub

import (
	"strings"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/ping"
)

// Routing rules, in priority order.
const (
	RuleCallback = 1 // inline button `ag:<msgid>:<i>` → that message's session
	RuleReaction = 2 // reaction → the reacted message's session
	RuleReply    = 3 // reply → the session that wrote the replied-to message; target unchanged
	RuleCurrent  = 4 // otherwise → the chat's current target
)

// Route is where an update goes.
type Route struct {
	Rule   int
	Target Target
	Ignore bool // callback / reaction on a message no session wrote
	// AnswerTo: RuleAnswer's agent message (the "Answer…" button's).
	AnswerTo int
	// UnknownAgent: a reply to a bot message the hub has no record of; an
	// agent's spool entry may still name its session (agents.go parks it).
	UnknownAgent bool
	// Unrouted: a reply to an agent's message whose session the hub doesn't
	// know (sent around the hub, or without a session). Not delivered: the
	// current target is someone else, and a guess there is a misroute.
	Unrouted bool
}

// KindPlain: an agent's message sent through the hub without a session.
const KindPlain = "plain"

// route decides where an update goes.
func (h *Hub) route(chatID int64, u *models.Update) (Route, error) {
	switch {
	case u.CallbackQuery != nil:
		msgID, _, ok := ping.ParseCallback(u.CallbackQuery.Data)
		if !ok {
			return Route{Rule: RuleCallback, Ignore: true}, nil
		}
		return h.routeToWriter(chatID, msgID, RuleCallback)
	case u.MessageReaction != nil:
		return h.routeToWriter(chatID, u.MessageReaction.MessageID, RuleReaction)
	case u.Message != nil && u.Message.ReplyToMessage != nil:
		r, err := h.routeToWriter(chatID, u.Message.ReplyToMessage.ID, RuleReply)
		if err != nil {
			return r, err
		}
		if !r.Ignore {
			return r, nil
		}
		rm := u.Message.ReplyToMessage
		if t, ok := h.headerTarget(rm); ok {
			return Route{Rule: RuleReply, Target: t}, nil
		}
		m, err := h.st.GetMessage(chatID, rm.ID)
		if err != nil {
			return Route{}, err
		}
		unknown := m == nil && (ping.HeaderRe.MatchString(rm.Text+rm.Caption) || (rm.From != nil && rm.From.IsBot))
		sessionless := m != nil && m.Direction == "out" && m.Target == "" && (m.Kind == KindPlain || m.Kind == "ping")
		if unknown || sessionless {
			return Route{Rule: RuleReply, Unrouted: true, UnknownAgent: unknown}, nil
		}
	case u.Message != nil:
		msgID, err := h.answerContext(chatID)
		if err != nil {
			return Route{}, err
		}
		if msgID != 0 {
			if r, err := h.routeToWriter(chatID, msgID, RuleAnswer); err != nil || !r.Ignore {
				r.AnswerTo = msgID
				return r, err
			}
		}
	}
	t, err := h.currentTarget(chatID)
	return Route{Rule: RuleCurrent, Target: t}, err
}

// routeToWriter routes to the target that wrote msgID (the DB's message ↔
// target link); Ignore when no target wrote it.
func (h *Hub) routeToWriter(chatID int64, msgID, rule int) (Route, error) {
	m, err := h.st.GetMessage(chatID, msgID)
	if err != nil {
		return Route{}, err
	}
	if m == nil || m.Direction != "out" || m.Target == "" {
		return Route{Rule: rule, Ignore: true}, nil
	}
	spec := m.Target
	if strings.HasPrefix(spec, KindSession+"@") && strings.HasSuffix(spec, ":") && m.SessionID != "" {
		spec += m.SessionID // a /spawn confirmation recorded before its session had an id
	}
	t, err := ParseTarget(spec)
	if err != nil {
		return Route{Rule: rule, Ignore: true}, nil
	}
	return Route{Rule: rule, Target: t}, nil
}

// headerTarget reads the `🤖 name · host` header (or `🤖 name@host`) of a message the DB doesn't know.
func (h *Hub) headerTarget(m *models.Message) (Target, bool) {
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	g := ping.HeaderRe.FindStringSubmatch(text)
	if g == nil {
		return Target{}, false
	}
	name, host := g[1], g[2]
	if _, ok := h.cfg.HostKeys[host]; !ok {
		return Target{}, false
	}
	if name == "vault" {
		return Target{Kind: KindVault, Host: host}, true
	}
	if s, ok := h.sessionByName(host, name); ok {
		return Target{Kind: KindSession, Host: host, Dir: strings.ToUpper(s.ID)}, true
	}
	return Target{}, false
}
