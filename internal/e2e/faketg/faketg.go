// Package faketg is a fake Telegram Bot API for tests (docs/testing.md §4): the Bot API methods the hub and `send`
// use, file downloads, two synthetic users in private chats, and a control API under /_fake/ that drives the user's
// side the way `telegram-agent-cli e2e` drives a real account.
package faketg

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Synthetic identities.
const (
	OwnerID      int64 = 1001
	OtherID      int64 = 2002
	DefaultBotID int64 = 1000001
	BotUsername        = "hommy_test_bot"
	BotName            = "hommy test"
)

// User is a Telegram user as the Bot API shows it.
type User struct {
	ID        int64
	IsBot     bool
	FirstName string
	LastName  string
	Username  string
	Lang      string
}

func (u User) api() map[string]any {
	m := map[string]any{"id": u.ID, "is_bot": u.IsBot, "first_name": u.FirstName}
	if u.LastName != "" {
		m["last_name"] = u.LastName
	}
	if u.Username != "" {
		m["username"] = u.Username
	}
	if u.Lang != "" {
		m["language_code"] = u.Lang
	}
	return m
}

func (u User) apiChat() map[string]any {
	m := map[string]any{"id": u.ID, "type": "private", "first_name": u.FirstName}
	if u.LastName != "" {
		m["last_name"] = u.LastName
	}
	if u.Username != "" {
		m["username"] = u.Username
	}
	return m
}

// Options configure a Server. Token is required.
type Options struct {
	Token string
	// Now and After are the clock (tests inject their own); defaults: time.Now, time.After.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
	// StateFile keeps the update and message id counters across restarts, so a
	// restarted fake never reuses an id a running hub already stored ("" = off).
	StateFile string
}

type reactionType struct {
	Type          string `json:"type"`
	Emoji         string `json:"emoji,omitempty"`
	CustomEmojiID string `json:"custom_emoji_id,omitempty"`
}

type file struct {
	ID, UniqueID, Path string
	Data               []byte
	Duration           int
}

type message struct {
	ID           int
	Date, Edited int64
	FromBot      bool
	Text         string
	Entities     json.RawMessage
	ReplyTo      int
	Voice        *file
	Inline       json.RawMessage // the inline_keyboard array
	Markup       json.RawMessage // a reply keyboard / remove / force_reply object
	NoEdit       bool            // sent with a non-inline reply markup: Telegram refuses edits
	Deleted      bool
	Service      string // "PinMessage" for the pin service line
	PinnedID     int
	BotReaction  []reactionType
	UserReaction []reactionType
}

type draft struct {
	seq        int
	At         int64
	ID         int64
	Text       string
	CanStop    bool
	KeepOnStop bool
}

type chat struct {
	user     User
	msgs     []*message
	nextID   int
	drafts   []draft
	pinned   int
	keyboard int // the message whose reply markup set the keyboard in effect
	menu     json.RawMessage
	action   string
	// seen is the newest message the tester has seen through the control API
	// (sent, read, or returned by a reply wait): a plain `wait --for reply`
	// returns the next bot message after it, so a fast answer that lands
	// between a send and the wait is not missed.
	seen int
	// onAdd records the id counters (Server.saveCounters).
	onAdd func()
}

func (c *chat) find(id int) *message {
	for i := len(c.msgs) - 1; i >= 0; i-- {
		if m := c.msgs[i]; m.ID == id {
			if m.Deleted {
				return nil
			}
			return m
		}
	}
	return nil
}

func (c *chat) add(m *message) *message {
	c.nextID++
	m.ID = c.nextID
	c.msgs = append(c.msgs, m)
	if c.onAdd != nil {
		c.onAdd()
	}
	return m
}

type update struct {
	ID      int64
	Kind    string
	Payload any
}

func (u update) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"update_id": u.ID, u.Kind: u.Payload})
}

type query struct {
	answered bool
	text     string
	alert    bool
	url      string
}

// Call is one Bot API request in the call log.
type Call struct {
	Seq         int            `json:"seq"`
	At          string         `json:"at"`
	Method      string         `json:"method"`
	Params      map[string]any `json:"params,omitempty"`
	Status      int            `json:"status"`
	OK          bool           `json:"ok"`
	Description string         `json:"description,omitempty"`
}

type fault struct {
	Method string `json:"method"`
	Status int    `json:"status"`
	Times  int    `json:"times"` // 0: until cleared
}

// Server is the fake. Its zero value is not usable: use New.
type Server struct {
	token string
	now   func() time.Time
	after func(time.Duration) <-chan time.Time

	mu         sync.Mutex
	changed    chan struct{}
	bot        User
	chats      map[int64]*chat
	updates    []update
	nextUpdate int64
	allowed    []string
	poll       chan struct{}
	files      map[string]*file
	nextFile   int
	calls      []Call
	fault      *fault
	queries    map[string]*query
	nextQuery  int
	draftSeq   int
	watching   int // waits in progress (tests sync on it)
	commands   json.RawMessage
	menu       json.RawMessage
	stateFile  string
}

// New returns a fake whose bot id is the token's numeric prefix (DefaultBotID when it has none).
func New(o Options) *Server {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.After == nil {
		o.After = time.After
	}
	botID := DefaultBotID
	if i := strings.IndexByte(o.Token, ':'); i > 0 {
		if n, err := strconv.ParseInt(o.Token[:i], 10, 64); err == nil {
			botID = n
		}
	}
	s := &Server{token: o.Token, now: o.Now, after: o.After, changed: make(chan struct{}),
		bot:   User{ID: botID, IsBot: true, FirstName: BotName, Username: BotUsername},
		chats: map[int64]*chat{}, files: map[string]*file{}, queries: map[string]*query{}, nextUpdate: 1}
	for _, u := range []User{
		{ID: OwnerID, FirstName: "Test", LastName: "Owner", Username: "test_owner", Lang: "en"},
		{ID: OtherID, FirstName: "Other", LastName: "User", Username: "other_user", Lang: "en"},
	} {
		s.chats[u.ID] = &chat{user: u, onAdd: s.saveCounters}
	}
	s.stateFile = o.StateFile
	s.loadCounters()
	return s
}

type counters struct {
	NextUpdate int64         `json:"next_update"`
	NextIDs    map[int64]int `json:"next_ids"`
}

// loadCounters continues the id spaces of an earlier run (StateFile).
func (s *Server) loadCounters() {
	if s.stateFile == "" {
		return
	}
	b, err := os.ReadFile(s.stateFile)
	if err != nil {
		return
	}
	var c counters
	if json.Unmarshal(b, &c) != nil {
		return
	}
	if c.NextUpdate > s.nextUpdate {
		s.nextUpdate = c.NextUpdate
	}
	for id, n := range c.NextIDs {
		if ch := s.chats[id]; ch != nil && n > ch.nextID {
			ch.nextID = n
		}
	}
}

// saveCounters records the id spaces; callers hold mu.
func (s *Server) saveCounters() {
	if s.stateFile == "" {
		return
	}
	c := counters{NextUpdate: s.nextUpdate, NextIDs: map[int64]int{}}
	for id, ch := range s.chats {
		c.NextIDs[id] = ch.nextID
	}
	b, _ := json.Marshal(c)
	tmp := s.stateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, s.stateFile)
	}
}

// Token is the bot token the fake accepts.
func (s *Server) Token() string { return s.token }

// bump wakes every waiter; callers hold mu.
func (s *Server) bump() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Server) unix() int64 { return s.now().Unix() }

// defaultUpdates: what Telegram delivers when allowed_updates was never set (reactions are opt-in).
var optIn = map[string]bool{"message_reaction": true, "message_reaction_count": true, "chat_member": true}

// push queues an update if the bot's allowed_updates lets that kind through (Telegram drops the others).
func (s *Server) push(kind string, payload any) {
	if len(s.allowed) == 0 {
		if optIn[kind] {
			return
		}
	} else if !contains(s.allowed, kind) {
		return
	}
	s.updates = append(s.updates, update{ID: s.nextUpdate, Kind: kind, Payload: payload})
	s.nextUpdate++
	s.saveCounters()
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Server) apiMessage(c *chat, m *message, nested bool) map[string]any {
	out := map[string]any{"message_id": m.ID, "date": m.Date, "chat": c.user.apiChat()}
	if m.FromBot {
		out["from"] = s.bot.api()
	} else {
		out["from"] = c.user.api()
	}
	if m.Edited != 0 {
		out["edit_date"] = m.Edited
	}
	if m.Voice != nil {
		out["voice"] = map[string]any{"file_id": m.Voice.ID, "file_unique_id": m.Voice.UniqueID,
			"duration": m.Voice.Duration, "mime_type": "audio/ogg", "file_size": len(m.Voice.Data)}
		if m.Text != "" {
			out["caption"] = m.Text
		}
	} else if m.Text != "" {
		out["text"] = m.Text
	}
	if len(m.Entities) > 0 && string(m.Entities) != "[]" {
		key := "entities"
		if m.Voice != nil {
			key = "caption_entities"
		}
		out[key] = m.Entities
	}
	if m.ReplyTo != 0 && !nested {
		if r := c.find(m.ReplyTo); r != nil {
			out["reply_to_message"] = s.apiMessage(c, r, true)
		}
	}
	if m.Inline != nil {
		out["reply_markup"] = map[string]any{"inline_keyboard": m.Inline}
	}
	if m.Service == "PinMessage" && !nested {
		if p := c.find(m.PinnedID); p != nil {
			out["pinned_message"] = s.apiMessage(c, p, true)
		}
	}
	return out
}

// ServeHTTP routes Bot API calls, file downloads and the control API.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/_fake/"):
		s.serveControl(w, r)
	case strings.HasPrefix(r.URL.Path, "/file/bot"):
		s.serveFile(w, r)
	case strings.HasPrefix(r.URL.Path, "/bot"):
		s.serveBotAPI(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error_code": 404, "description": "Not Found"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	code int
	desc string
}

func (e *apiError) Error() string { return e.desc }

func badRequest(desc string) *apiError { return &apiError{400, "Bad Request: " + desc} }

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/file/bot")
	tok, path, ok := strings.Cut(rest, "/")
	if !ok || tok != s.token {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
		return
	}
	s.mu.Lock()
	if f := s.takeFault("file"); f != nil {
		s.mu.Unlock()
		writeJSON(w, f.code, map[string]any{"ok": false, "error_code": f.code, "description": f.desc})
		return
	}
	var data []byte
	for _, f := range s.files {
		if f.Path == path {
			data = f.Data
		}
	}
	s.mu.Unlock()
	if data == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error_code": 404, "description": "Not Found"})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// takeFault returns the configured fault for method (lower case), consuming one use; callers hold mu.
func (s *Server) takeFault(method string) *apiError {
	f := s.fault
	if f == nil || (f.Method != "*" && !strings.EqualFold(f.Method, method)) {
		return nil
	}
	if f.Times > 0 {
		f.Times--
		if f.Times == 0 {
			s.fault = nil
		}
	}
	return &apiError{f.Status, http.StatusText(f.Status) + ": fake fault"}
}

func (s *Server) serveBotAPI(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/bot")
	tok, method, _ := strings.Cut(rest, "/")
	if tok != s.token {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
		return
	}
	p, err := parseParams(r)
	var result any
	var aerr *apiError
	if err != nil {
		aerr = badRequest(err.Error())
	} else if strings.EqualFold(method, "getUpdates") {
		result, aerr = s.getUpdates(r, p)
	} else {
		s.mu.Lock()
		if aerr = s.takeFault(method); aerr == nil {
			result, aerr = s.call(method, p)
		}
		s.bump()
		s.mu.Unlock()
	}
	status, body := http.StatusOK, map[string]any{"ok": true, "result": result}
	if aerr != nil {
		status, body = aerr.code, map[string]any{"ok": false, "error_code": aerr.code, "description": aerr.desc}
	}
	s.mu.Lock()
	c := Call{Seq: len(s.calls) + 1, At: s.now().UTC().Format(time.RFC3339Nano), Method: method, Status: status, OK: aerr == nil}
	if p != nil {
		c.Params = p.logged()
	}
	if aerr != nil {
		c.Description = aerr.desc
	}
	s.calls = append(s.calls, c)
	s.bump()
	s.mu.Unlock()
	writeJSON(w, status, body)
}

func (s *Server) chatParam(p *params) (*chat, *apiError) {
	id, ok := p.int64("chat_id")
	if !ok {
		if p.has("chat_id") {
			return nil, badRequest("chat not found")
		}
		return nil, badRequest("chat_id is empty")
	}
	c := s.chats[id]
	if c == nil {
		return nil, badRequest("chat not found")
	}
	return c, nil
}

func (s *Server) msgParam(c *chat, p *params, notFound string) (*message, *apiError) {
	id, ok := p.int64("message_id")
	if !ok {
		return nil, badRequest("message identifier is not specified")
	}
	m := c.find(int(id))
	if m == nil {
		return nil, badRequest(notFound)
	}
	return m, nil
}

// replyTo reads reply_parameters (or the legacy reply_to_message_id).
func replyTo(c *chat, p *params) (int, *apiError) {
	var rp struct {
		MessageID                int  `json:"message_id"`
		AllowSendingWithoutReply bool `json:"allow_sending_without_reply"`
	}
	if raw := p.json("reply_parameters"); raw != nil {
		if err := json.Unmarshal(raw, &rp); err != nil {
			return 0, badRequest("can't parse reply parameters JSON object")
		}
	} else if id, ok := p.int64("reply_to_message_id"); ok {
		rp.MessageID = int(id)
		rp.AllowSendingWithoutReply = p.bool("allow_sending_without_reply")
	}
	if rp.MessageID == 0 {
		return 0, nil
	}
	if c.find(rp.MessageID) == nil {
		if rp.AllowSendingWithoutReply {
			return 0, nil
		}
		return 0, badRequest("message to be replied not found")
	}
	return rp.MessageID, nil
}

// applyMarkup stores reply_markup on a new or edited bot message.
func (s *Server) applyMarkup(c *chat, m *message, raw json.RawMessage) *apiError {
	if raw == nil {
		return nil
	}
	var mk map[string]json.RawMessage
	if err := json.Unmarshal(raw, &mk); err != nil {
		return badRequest("can't parse reply keyboard markup JSON object")
	}
	switch {
	case mk["inline_keyboard"] != nil:
		m.Inline = mk["inline_keyboard"]
	case mk["keyboard"] != nil, mk["remove_keyboard"] != nil:
		m.Markup, m.NoEdit = raw, true
		c.keyboard = m.ID
	case mk["force_reply"] != nil:
		m.Markup, m.NoEdit = raw, true
	}
	return nil
}

func (s *Server) call(method string, p *params) (any, *apiError) {
	switch strings.ToLower(method) {
	case "getme":
		u := s.bot.api()
		u["can_join_groups"], u["can_read_all_group_messages"], u["supports_inline_queries"] = false, false, false
		return u, nil
	case "sendmessage":
		return s.sendMessage(p)
	case "sendmessagedraft":
		return s.sendMessageDraft(p)
	case "editmessagetext":
		return s.editMessageText(p)
	case "editmessagereplymarkup":
		return s.editMessageReplyMarkup(p)
	case "deletemessage":
		c, err := s.chatParam(p)
		if err != nil {
			return nil, err
		}
		m, err := s.msgParam(c, p, "message to delete not found")
		if err != nil {
			return nil, err
		}
		m.Deleted = true
		if c.pinned == m.ID {
			c.pinned = 0
		}
		return true, nil
	case "sendvoice":
		return s.sendVoice(p)
	case "sendchataction":
		c, err := s.chatParam(p)
		if err != nil {
			return nil, err
		}
		if p.str("action") == "" {
			return nil, badRequest("action is empty")
		}
		c.action = p.str("action")
		return true, nil
	case "setmessagereaction":
		c, err := s.chatParam(p)
		if err != nil {
			return nil, err
		}
		m, err := s.msgParam(c, p, "message to react not found")
		if err != nil {
			return nil, err
		}
		var rs []reactionType
		if raw := p.json("reaction"); raw != nil {
			if json.Unmarshal(raw, &rs) != nil {
				return nil, badRequest("can't parse reaction types JSON object")
			}
		}
		m.BotReaction = rs
		return true, nil
	case "pinchatmessage":
		c, err := s.chatParam(p)
		if err != nil {
			return nil, err
		}
		m, err := s.msgParam(c, p, "message to pin not found")
		if err != nil {
			return nil, err
		}
		c.pinned = m.ID
		svc := c.add(&message{Date: s.unix(), FromBot: true, Service: "PinMessage", PinnedID: m.ID})
		s.push("message", s.apiMessage(c, svc, false))
		return true, nil
	case "unpinchatmessage":
		c, err := s.chatParam(p)
		if err != nil {
			return nil, err
		}
		if id, ok := p.int64("message_id"); !ok || int(id) == c.pinned {
			c.pinned = 0
		}
		return true, nil
	case "answercallbackquery":
		q := s.queries[p.str("callback_query_id")]
		if q == nil || q.answered {
			return nil, badRequest("query is too old and response timeout expired or query ID is invalid")
		}
		q.answered, q.text, q.alert, q.url = true, p.str("text"), p.bool("show_alert"), p.str("url")
		return true, nil
	case "setmycommands":
		raw := p.json("commands")
		if raw == nil {
			return nil, badRequest("parameter \"commands\" is required")
		}
		s.commands = raw
		return true, nil
	case "getmycommands":
		if s.commands == nil {
			return []any{}, nil
		}
		return s.commands, nil
	case "setchatmenubutton":
		raw := p.json("menu_button")
		if !p.has("chat_id") {
			s.menu = raw
			return true, nil
		}
		c, err := s.chatParam(p)
		if err != nil {
			return nil, err
		}
		c.menu = raw
		return true, nil
	case "getchatmenubutton":
		raw := s.menu
		if c, err := s.chatParam(p); err == nil && c.menu != nil {
			raw = c.menu
		}
		if raw == nil {
			return map[string]string{"type": "default"}, nil
		}
		return raw, nil
	case "getfile":
		f := s.files[p.str("file_id")]
		if f == nil {
			return nil, badRequest("invalid file_id")
		}
		return map[string]any{"file_id": f.ID, "file_unique_id": f.UniqueID, "file_size": len(f.Data), "file_path": f.Path}, nil
	}
	return nil, &apiError{404, "Not Found"}
}

func (s *Server) sendMessage(p *params) (any, *apiError) {
	c, err := s.chatParam(p)
	if err != nil {
		return nil, err
	}
	text := p.str("text")
	if strings.TrimSpace(text) == "" {
		return nil, badRequest("message text is empty")
	}
	rt, err := replyTo(c, p)
	if err != nil {
		return nil, err
	}
	m := &message{Date: s.unix(), FromBot: true, Text: text, Entities: p.json("entities"), ReplyTo: rt}
	c.add(m)
	if err := s.applyMarkup(c, m, p.json("reply_markup")); err != nil {
		return nil, err
	}
	return s.apiMessage(c, m, false), nil
}

func (s *Server) sendMessageDraft(p *params) (any, *apiError) {
	c, err := s.chatParam(p)
	if err != nil {
		return nil, err
	}
	id, ok := p.int64("draft_id")
	if !ok || id == 0 {
		return nil, badRequest("draft_id must be non-zero")
	}
	text := p.str("text")
	if strings.TrimSpace(text) == "" {
		return nil, badRequest("text is empty")
	}
	s.draftSeq++
	c.drafts = append(c.drafts, draft{seq: s.draftSeq, At: s.unix(), ID: id, Text: text,
		CanStop: p.bool("can_stop"), KeepOnStop: p.bool("keep_on_stop")})
	return true, nil
}

func sameJSON(a, b json.RawMessage) bool {
	norm := func(r json.RawMessage) string {
		if len(r) == 0 || string(r) == "null" || string(r) == "[]" {
			return ""
		}
		var buf bytes.Buffer
		if json.Compact(&buf, r) != nil {
			return string(r)
		}
		return buf.String()
	}
	return norm(a) == norm(b)
}

func inlineOf(raw json.RawMessage) (json.RawMessage, *apiError) {
	if raw == nil {
		return nil, nil
	}
	var mk struct {
		Inline json.RawMessage `json:"inline_keyboard"`
	}
	if json.Unmarshal(raw, &mk) != nil {
		return nil, badRequest("can't parse inline keyboard markup JSON object")
	}
	return mk.Inline, nil
}

const notModified = "message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message"

func (s *Server) editable(p *params) (*chat, *message, *apiError) {
	c, err := s.chatParam(p)
	if err != nil {
		return nil, nil, err
	}
	m, err := s.msgParam(c, p, "message to edit not found")
	if err != nil {
		return nil, nil, err
	}
	if !m.FromBot || m.NoEdit || m.Service != "" {
		return nil, nil, badRequest("message can't be edited")
	}
	return c, m, nil
}

func (s *Server) editMessageText(p *params) (any, *apiError) {
	c, m, err := s.editable(p)
	if err != nil {
		return nil, err
	}
	text := p.str("text")
	if strings.TrimSpace(text) == "" {
		return nil, badRequest("message text is empty")
	}
	inline, err := inlineOf(p.json("reply_markup"))
	if err != nil {
		return nil, err
	}
	ents := p.json("entities")
	if text == m.Text && sameJSON(ents, m.Entities) && sameJSON(inline, m.Inline) {
		return nil, badRequest(notModified)
	}
	m.Text, m.Entities, m.Inline, m.Edited = text, ents, inline, s.unix()
	return s.apiMessage(c, m, false), nil
}

func (s *Server) editMessageReplyMarkup(p *params) (any, *apiError) {
	c, m, err := s.editable(p)
	if err != nil {
		return nil, err
	}
	inline, err := inlineOf(p.json("reply_markup"))
	if err != nil {
		return nil, err
	}
	if sameJSON(inline, m.Inline) {
		return nil, badRequest(notModified)
	}
	m.Inline, m.Edited = inline, s.unix()
	return s.apiMessage(c, m, false), nil
}

func (s *Server) addFile(data []byte, kind string) *file {
	s.nextFile++
	f := &file{ID: fmt.Sprintf("fake-file-%s-%d", kind, s.nextFile), UniqueID: fmt.Sprintf("fu%d", s.nextFile),
		Path: fmt.Sprintf("%s/file_%d.oga", kind, s.nextFile), Data: data, Duration: oggDuration(data)}
	s.files[f.ID] = f
	return f
}

func (s *Server) sendVoice(p *params) (any, *apiError) {
	c, err := s.chatParam(p)
	if err != nil {
		return nil, err
	}
	var f *file
	if up, ok := p.files["voice"]; ok {
		f = s.addFile(up.Data, "voice")
	} else if id := p.str("voice"); id != "" {
		if f = s.files[id]; f == nil {
			return nil, badRequest("wrong remote file identifier specified")
		}
	} else {
		return nil, badRequest("there is no voice in the request")
	}
	if d, ok := p.int64("duration"); ok {
		f.Duration = int(d)
	}
	rt, err := replyTo(c, p)
	if err != nil {
		return nil, err
	}
	m := &message{Date: s.unix(), FromBot: true, Voice: f, Text: p.str("caption"), Entities: p.json("caption_entities"), ReplyTo: rt}
	c.add(m)
	if err := s.applyMarkup(c, m, p.json("reply_markup")); err != nil {
		return nil, err
	}
	return s.apiMessage(c, m, false), nil
}

// getUpdates long-polls: it answers at once when updates are pending, else when one arrives or the timeout ends.
// A newer getUpdates call ends a waiting one with 409, as Telegram does.
func (s *Server) getUpdates(r *http.Request, p *params) (any, *apiError) {
	offset, _ := p.int64("offset")
	limit, ok := p.int64("limit")
	if !ok || limit <= 0 || limit > 100 {
		limit = 100
	}
	timeout, _ := p.int64("timeout")
	var timer <-chan time.Time
	if timeout > 0 {
		timer = s.after(time.Duration(timeout) * time.Second)
	}
	s.mu.Lock()
	if f := s.takeFault("getUpdates"); f != nil {
		s.mu.Unlock()
		return nil, f
	}
	if raw := p.json("allowed_updates"); raw != nil {
		var list []string
		if json.Unmarshal(raw, &list) == nil {
			s.allowed = list
		}
	}
	if offset > 0 {
		keep := s.updates[:0]
		for _, u := range s.updates {
			if u.ID >= offset {
				keep = append(keep, u)
			}
		}
		s.updates = keep
	}
	if s.poll != nil {
		close(s.poll)
	}
	mine := make(chan struct{})
	s.poll = mine
	s.bump()
	defer func() {
		s.mu.Lock()
		if s.poll == mine {
			s.poll = nil
		}
		s.mu.Unlock()
	}()
	for {
		if n := len(s.updates); n > 0 || timeout <= 0 {
			out := append([]update{}, s.updates[:min(n, int(limit))]...)
			s.mu.Unlock()
			return out, nil
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-mine:
			return nil, &apiError{409, "Conflict: terminated by other getUpdates request; make sure that only one bot instance is running"}
		case <-timer:
			return []update{}, nil
		case <-r.Context().Done():
			return []update{}, nil
		}
		s.mu.Lock()
	}
}

// oggDuration reads an Ogg Opus file's length in whole seconds (rounded up; 0 when unreadable).
func oggDuration(b []byte) int {
	head := bytes.Index(b, []byte("OpusHead"))
	last := bytes.LastIndex(b, []byte("OggS"))
	if head < 0 || last < 0 || len(b) < last+14 || len(b) < head+12 {
		return 0
	}
	granule := binary.LittleEndian.Uint64(b[last+6:])
	preskip := uint64(binary.LittleEndian.Uint16(b[head+10:]))
	if granule <= preskip {
		return 0
	}
	return int(math.Ceil(float64(granule-preskip) / 48000))
}
