package faketg

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// The user's side of the chat: what a Telegram client does, turned into Bot API updates.

type cliError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *cliError) Error() string { return e.Message }

// Exit is the telegram-agent-cli exit code for the error code.
func (e *cliError) Exit() int {
	switch e.Code {
	case "usage":
		return 2
	case "not_found", "ambiguous":
		return 4
	case "telegram":
		return 5
	case "e2e_refused":
		return 8
	case "timeout":
		return 9
	}
	return 1
}

func usage(f string, a ...any) *cliError {
	return &cliError{Code: "usage", Message: fmt.Sprintf(f, a...)}
}
func notFound(f string, a ...any) *cliError {
	return &cliError{Code: "not_found", Message: fmt.Sprintf(f, a...)}
}

func (s *Server) userChat(id int64) (*chat, *cliError) {
	c := s.chats[id]
	if c == nil {
		return nil, notFound("no user %d (users: owner %d, other %d)", id, OwnerID, OtherID)
	}
	return c, nil
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// userText adds a text message from the user and queues its update; callers hold mu.
func (s *Server) userText(c *chat, text string, replyTo int) (*message, *cliError) {
	if strings.TrimSpace(text) == "" {
		return nil, usage("the message is empty")
	}
	if replyTo != 0 && c.find(replyTo) == nil {
		return nil, notFound("message %d not found in the chat with %s", replyTo, BotName)
	}
	m := &message{Date: s.unix(), Text: text, ReplyTo: replyTo}
	if strings.HasPrefix(text, "/") {
		cmd := strings.Fields(text)[0]
		m.Entities, _ = json.Marshal([]map[string]any{{"type": "bot_command", "offset": 0, "length": utf16Len(cmd)}})
	}
	c.add(m)
	s.push("message", s.apiMessage(c, m, false))
	return m, nil
}

func (s *Server) userVoice(c *chat, data []byte, duration, replyTo int) (*message, *cliError) {
	if len(data) == 0 {
		return nil, usage("the voice file is empty")
	}
	if !strings.HasPrefix(string(data), "OggS") {
		return nil, usage("a voice note must be an Ogg/Opus file")
	}
	if replyTo != 0 && c.find(replyTo) == nil {
		return nil, notFound("message %d not found in the chat with %s", replyTo, BotName)
	}
	f := s.addFile(data, "voice")
	if duration > 0 {
		f.Duration = duration
	}
	m := c.add(&message{Date: s.unix(), Voice: f, ReplyTo: replyTo})
	s.push("message", s.apiMessage(c, m, false))
	return m, nil
}

func (s *Server) userReact(c *chat, id int, emoji string) (*message, *cliError) {
	m := c.find(id)
	if m == nil {
		return nil, notFound("message %d not found in the chat with %s", id, BotName)
	}
	old := m.UserReaction
	m.UserReaction = nil
	if emoji != "" {
		m.UserReaction = []reactionType{{Type: "emoji", Emoji: emoji}}
	}
	s.push("message_reaction", map[string]any{"chat": c.user.apiChat(), "message_id": m.ID, "user": c.user.api(),
		"date": s.unix(), "old_reaction": nonNil(old), "new_reaction": nonNil(m.UserReaction)})
	return m, nil
}

func nonNil(r []reactionType) []reactionType {
	if r == nil {
		return []reactionType{}
	}
	return r
}

// pressInline queues a callback_query for the button and returns its query id.
func (s *Server) pressInline(c *chat, id int, btn string) (button, string, *cliError) {
	m := c.find(id)
	if m == nil {
		return button{}, "", notFound("message %d not found in the chat with %s", id, BotName)
	}
	rows := inlineButtons(m.Inline)
	if len(rows) == 0 {
		return button{}, "", notFound("message %d has no inline buttons", id)
	}
	b, err := pickButton(rows, btn)
	if err != nil {
		return button{}, "", err
	}
	if b.Type != "callback" {
		return b, "", usage("button %q is a %s button, not a callback button", b.Text, b.Type)
	}
	s.nextQuery++
	qid := fmt.Sprintf("%d%06d", c.user.ID, s.nextQuery)
	s.queries[qid] = &query{}
	s.push("callback_query", map[string]any{"id": qid, "from": c.user.api(), "message": s.apiMessage(c, m, false),
		"chat_instance": strconv.FormatInt(c.user.ID*7919, 10), "data": b.Data})
	return b, qid, nil
}

func (s *Server) userStop(c *chat, draftID int64) {
	s.push("stopped_message_generation", map[string]any{"chat": c.user.apiChat(),
		"draft_id": strconv.FormatInt(draftID, 10)})
}

// keyboardInEffect is the reply keyboard the client shows (nil when none or hidden).
func (c *chat) keyboardInEffect() (*message, *replyMarkup) {
	if c.keyboard == 0 {
		return nil, nil
	}
	m := c.find(c.keyboard)
	if m == nil {
		return nil, nil
	}
	rm := markupView(m.Markup)
	if rm == nil || rm.Type != "keyboard" {
		return nil, nil
	}
	return m, rm
}

func pickButton(rows [][]button, sel string) (button, *cliError) {
	var all []button
	for _, r := range rows {
		all = append(all, r...)
	}
	for _, b := range all {
		if b.Text == sel {
			return b, nil
		}
	}
	if i, err := strconv.Atoi(sel); err == nil && i >= 0 && i < len(all) {
		return all[i], nil
	}
	return button{}, notFound("no button %q", sel)
}

// InitData returns Mini App initData for the user, signed with the bot token the way Telegram signs it.
func (s *Server) InitData(userID int64) (string, error) {
	s.mu.Lock()
	c := s.chats[userID]
	now := s.now()
	s.mu.Unlock()
	if c == nil {
		return "", fmt.Errorf("no user %d", userID)
	}
	u := c.user
	user, _ := json.Marshal(map[string]any{"id": u.ID, "first_name": u.FirstName, "last_name": u.LastName,
		"username": u.Username, "language_code": u.Lang, "allows_write_to_pm": true})
	fields := map[string]string{
		"query_id":  fmt.Sprintf("AAFake%010d", u.ID),
		"user":      string(user),
		"auth_date": strconv.FormatInt(now.Unix(), 10),
	}
	fields["hash"] = SignInitData(s.token, fields)
	v := url.Values{}
	for k, x := range fields {
		v.Set(k, x)
	}
	return v.Encode(), nil
}

// SignInitData is Telegram's Mini App initData hash: HMAC-SHA256 of the sorted "key=value" lines, keyed with
// HMAC-SHA256("WebAppData", token).
func SignInitData(token string, fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k != "hash" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	lines := make([]string, len(keys))
	for i, k := range keys {
		lines[i] = k + "=" + fields[k]
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(token))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(mac.Sum(nil))
}

// --- the client's view (telegram-agent-cli e2e JSON shapes) ---

type entity struct {
	Type          string `json:"type"`
	Offset        int    `json:"offset"`
	Length        int    `json:"length"`
	Text          string `json:"text"`
	URL           string `json:"url,omitempty"`
	UserID        int64  `json:"user_id,omitempty"`
	Language      string `json:"language,omitempty"`
	CustomEmojiID string `json:"custom_emoji_id,omitempty"`
}

type media struct {
	Type string `json:"type"`
	Mime string `json:"mime,omitempty"`
	Size int64  `json:"size,omitempty"`
}

type service struct {
	Type string `json:"type"`
}

type reaction struct {
	Emoji       string  `json:"emoji,omitempty"`
	CustomEmoji string  `json:"custom_emoji_id,omitempty"`
	Count       int     `json:"count"`
	Mine        bool    `json:"mine"`
	By          []int64 `json:"by,omitempty"`
}

type button struct {
	Text  string `json:"text"`
	Type  string `json:"type"`
	Data  string `json:"data,omitempty"`
	URL   string `json:"url,omitempty"`
	Query string `json:"query,omitempty"`
	Copy  string `json:"copy,omitempty"`
}

type replyMarkup struct {
	Type        string     `json:"type"`
	Rows        [][]button `json:"rows,omitempty"`
	Persistent  bool       `json:"persistent,omitempty"`
	Resize      bool       `json:"resize,omitempty"`
	OneTime     bool       `json:"one_time,omitempty"`
	Selective   bool       `json:"selective,omitempty"`
	Placeholder string     `json:"placeholder,omitempty"`
}

type botMessage struct {
	ID        int          `json:"id"`
	Date      string       `json:"date"`
	Edited    string       `json:"edited,omitempty"`
	From      string       `json:"from"`
	Text      string       `json:"text"`
	Entities  []entity     `json:"entities,omitempty"`
	ReplyTo   int          `json:"reply_to,omitempty"`
	Media     *media       `json:"media,omitempty"`
	Service   *service     `json:"service,omitempty"`
	Reactions []reaction   `json:"reactions,omitempty"`
	Inline    [][]button   `json:"inline_keyboard,omitempty"`
	Markup    *replyMarkup `json:"reply_markup,omitempty"`
}

type draftView struct {
	Date       string `json:"date"`
	DraftID    int64  `json:"draft_id"`
	Text       string `json:"text"`
	CanStop    bool   `json:"can_stop,omitempty"`
	KeepOnStop bool   `json:"keep_on_stop,omitempty"`
}

func stamp(unix int64) string { return time.Unix(unix, 0).UTC().Format(time.RFC3339) }

func (s *Server) view(c *chat, m *message) botMessage {
	v := botMessage{ID: m.ID, Date: stamp(m.Date), From: "me", Text: m.Text, ReplyTo: m.ReplyTo,
		Inline: inlineButtons(m.Inline), Markup: markupView(m.Markup)}
	if m.FromBot {
		v.From = "bot"
	}
	if m.Edited != 0 {
		v.Edited = stamp(m.Edited)
	}
	if m.Voice != nil {
		v.Media = &media{Type: "voice", Mime: "audio/ogg", Size: int64(len(m.Voice.Data))}
	}
	if m.Service != "" {
		v.Service = &service{Type: m.Service}
	}
	var ents []struct {
		Type     string `json:"type"`
		Offset   int    `json:"offset"`
		Length   int    `json:"length"`
		URL      string `json:"url"`
		Language string `json:"language"`
		Custom   string `json:"custom_emoji_id"`
		User     *struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if len(m.Entities) > 0 && json.Unmarshal(m.Entities, &ents) == nil {
		u := utf16.Encode([]rune(m.Text))
		for _, e := range ents {
			x := entity{Type: e.Type, Offset: e.Offset, Length: e.Length, URL: e.URL, Language: e.Language, CustomEmojiID: e.Custom}
			if e.Offset >= 0 && e.Length >= 0 && e.Offset+e.Length <= len(u) {
				x.Text = string(utf16.Decode(u[e.Offset : e.Offset+e.Length]))
			}
			if e.User != nil {
				x.UserID = e.User.ID
			}
			v.Entities = append(v.Entities, x)
		}
	}
	add := func(r reactionType, by int64, mine bool) {
		for i := range v.Reactions {
			x := &v.Reactions[i]
			if x.Emoji == r.Emoji && x.CustomEmoji == r.CustomEmojiID {
				x.Count++
				x.Mine = x.Mine || mine
				x.By = append(x.By, by)
				return
			}
		}
		v.Reactions = append(v.Reactions, reaction{Emoji: r.Emoji, CustomEmoji: r.CustomEmojiID, Count: 1, Mine: mine, By: []int64{by}})
	}
	for _, r := range m.UserReaction {
		add(r, c.user.ID, true)
	}
	for _, r := range m.BotReaction {
		add(r, s.bot.ID, false)
	}
	return v
}

func inlineButtons(raw json.RawMessage) [][]button {
	if raw == nil {
		return nil
	}
	var rows [][]struct {
		Text         string `json:"text"`
		CallbackData string `json:"callback_data"`
		URL          string `json:"url"`
		WebApp       *struct {
			URL string `json:"url"`
		} `json:"web_app"`
		SwitchInline *string `json:"switch_inline_query"`
		CopyText     *struct {
			Text string `json:"text"`
		} `json:"copy_text"`
	}
	if json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	var out [][]button
	for _, r := range rows {
		var row []button
		for _, b := range r {
			x := button{Text: b.Text, Type: "callback", Data: b.CallbackData}
			switch {
			case b.WebApp != nil:
				x = button{Text: b.Text, Type: "web_app", URL: b.WebApp.URL}
			case b.URL != "":
				x = button{Text: b.Text, Type: "url", URL: b.URL}
			case b.SwitchInline != nil:
				x = button{Text: b.Text, Type: "switch_inline", Query: *b.SwitchInline}
			case b.CopyText != nil:
				x = button{Text: b.Text, Type: "copy", Copy: b.CopyText.Text}
			}
			row = append(row, x)
		}
		out = append(out, row)
	}
	return out
}

func markupView(raw json.RawMessage) *replyMarkup {
	if raw == nil {
		return nil
	}
	var mk struct {
		Keyboard    [][]json.RawMessage `json:"keyboard"`
		Remove      bool                `json:"remove_keyboard"`
		Force       bool                `json:"force_reply"`
		Persistent  bool                `json:"is_persistent"`
		Resize      bool                `json:"resize_keyboard"`
		OneTime     bool                `json:"one_time_keyboard"`
		Selective   bool                `json:"selective"`
		Placeholder string              `json:"input_field_placeholder"`
	}
	if json.Unmarshal(raw, &mk) != nil {
		return nil
	}
	switch {
	case mk.Remove:
		return &replyMarkup{Type: "hide", Selective: mk.Selective}
	case mk.Force:
		return &replyMarkup{Type: "force_reply", Selective: mk.Selective, Placeholder: mk.Placeholder}
	}
	rm := &replyMarkup{Type: "keyboard", Persistent: mk.Persistent, Resize: mk.Resize, OneTime: mk.OneTime,
		Selective: mk.Selective, Placeholder: mk.Placeholder}
	for _, r := range mk.Keyboard {
		var row []button
		for _, raw := range r {
			var txt string
			if json.Unmarshal(raw, &txt) == nil {
				row = append(row, button{Text: txt, Type: "text"})
				continue
			}
			var b struct {
				Text     string          `json:"text"`
				Contact  bool            `json:"request_contact"`
				Location bool            `json:"request_location"`
				WebApp   json.RawMessage `json:"web_app"`
			}
			_ = json.Unmarshal(raw, &b)
			x := button{Text: b.Text, Type: "text"}
			switch {
			case b.Contact:
				x.Type = "request_phone"
			case b.Location:
				x.Type = "request_location"
			case b.WebApp != nil:
				x.Type = "web_app"
			}
			row = append(row, x)
		}
		rm.Rows = append(rm.Rows, row)
	}
	return rm
}
