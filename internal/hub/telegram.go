package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/ping"
)

// Telegram is what the hub needs from the Bot API (faked in tests).
type Telegram interface {
	Send(ctx context.Context, chatID int64, text string, replyTo int) (int, error)
	SendVoice(ctx context.Context, chatID int64, ogg []byte, replyTo int) (int, error)
	React(ctx context.Context, chatID int64, msgID int, emoji string) error
	// Draft shows (or updates) a live draft with Telegram's stop button.
	Draft(ctx context.Context, chatID int64, draftID int64, text string) error
	Typing(ctx context.Context, chatID int64) error
	Download(ctx context.Context, fileID string) ([]byte, error)
	SetCommands(ctx context.Context, cmds []models.BotCommand) error
	Ping(ctx context.Context) error
	// Me is the bot's username (getMe).
	Me(ctx context.Context) (string, error)

	// SendText sends text with entities (labels go in code entities, so a
	// path like ~/x never becomes a /x command).
	SendText(ctx context.Context, chatID int64, text string, entities []models.MessageEntity, replyTo int) (int, error)
	// SendBare sends without a reply keyboard: Telegram can't edit a message
	// that carries one, and the pinned line is edited on every switch.
	SendBare(ctx context.Context, chatID int64, text string, entities []models.MessageEntity) (int, error)
	EditText(ctx context.Context, chatID int64, msgID int, text string, entities []models.MessageEntity) error
	Pin(ctx context.Context, chatID int64, msgID int) error
	Unpin(ctx context.Context, chatID int64, msgID int) error
	Delete(ctx context.Context, chatID int64, msgID int) error
	SetMenuButton(ctx context.Context, chatID int64, text, url string) error
	// SendOut sends with options (no link preview, an inline keyboard instead
	// of the reply keyboard).
	SendOut(ctx context.Context, chatID int64, o Out) (int, error)
	// SetKeyboard sets where every sent message's reply keyboard comes from
	// (nil markup = none), so the target indicators never drift.
	SetKeyboard(fn func(chatID int64) models.ReplyMarkup)

	// SendPing sends an agent's message (see SendOpts).
	SendPing(ctx context.Context, chatID int64, text string, o SendOpts) (int, error)
	// SetButtons puts inline buttons on a message sent without a reply keyboard.
	SetButtons(ctx context.Context, chatID int64, msgID int, rows [][]ping.Button) error
	// AnswerCallback stops the button's spinner on the phone, with a short toast.
	AnswerCallback(ctx context.Context, queryID, text string) error
}

// SendOpts says how SendPing sends.
type SendOpts struct {
	Silent bool // no notification sound
	Bare   bool // no reply keyboard: inline buttons are set right after
	// Inline is sent with the message instead of the reply keyboard (buttons
	// that don't need the message id, like "Open").
	Inline [][]ping.Button
}

func (t *botTelegram) SendPing(ctx context.Context, chatID int64, text string, o SendOpts) (int, error) {
	p := &bot.SendMessageParams{ChatID: chatID, Text: text, DisableNotification: o.Silent}
	switch {
	case len(o.Inline) > 0:
		p.ReplyMarkup = inlineKeyboard(o.Inline)
	case !o.Bare:
		p.ReplyMarkup = t.keyboard(chatID)
	}
	m, err := t.b.SendMessage(ctx, p)
	if err != nil {
		return 0, err
	}
	return m.ID, nil
}

func (t *botTelegram) SetButtons(ctx context.Context, chatID int64, msgID int, rows [][]ping.Button) error {
	_, err := t.b.EditMessageReplyMarkup(ctx, &bot.EditMessageReplyMarkupParams{ChatID: chatID, MessageID: msgID,
		ReplyMarkup: inlineKeyboard(rows)})
	return err
}

func inlineKeyboard(rows [][]ping.Button) *models.InlineKeyboardMarkup {
	kb := &models.InlineKeyboardMarkup{}
	for _, r := range rows {
		var row []models.InlineKeyboardButton
		for _, b := range r {
			btn := models.InlineKeyboardButton{Text: b.Text, CallbackData: b.Data}
			if b.WebApp != nil {
				btn = models.InlineKeyboardButton{Text: b.Text, WebApp: &models.WebAppInfo{URL: b.WebApp.URL}}
			}
			row = append(row, btn)
		}
		kb.InlineKeyboard = append(kb.InlineKeyboard, row)
	}
	return kb
}

func (t *botTelegram) AnswerCallback(ctx context.Context, queryID, text string) error {
	_, err := t.b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: queryID, Text: text})
	return err
}

// botTelegram is the real Bot API client.
type botTelegram struct {
	b               *bot.Bot
	token           string
	downloadTimeout time.Duration // a whole file download, body included

	kbMu sync.Mutex
	kb   func(chatID int64) models.ReplyMarkup
}

func (t *botTelegram) SetKeyboard(fn func(chatID int64) models.ReplyMarkup) {
	t.kbMu.Lock()
	t.kb = fn
	t.kbMu.Unlock()
}

// keyboard is the reply keyboard every message to chatID carries.
func (t *botTelegram) keyboard(chatID int64) models.ReplyMarkup {
	t.kbMu.Lock()
	fn := t.kb
	t.kbMu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(chatID)
}

func replyParams(replyTo int) *models.ReplyParameters {
	if replyTo == 0 {
		return nil
	}
	return &models.ReplyParameters{MessageID: replyTo, AllowSendingWithoutReply: true}
}

func (t *botTelegram) Send(ctx context.Context, chatID int64, text string, replyTo int) (int, error) {
	return t.SendText(ctx, chatID, text, nil, replyTo)
}

func (t *botTelegram) SendText(ctx context.Context, chatID int64, text string, entities []models.MessageEntity, replyTo int) (int, error) {
	m, err := t.b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text, Entities: entities,
		ReplyParameters: replyParams(replyTo), ReplyMarkup: t.keyboard(chatID)})
	if err != nil {
		return 0, err
	}
	return m.ID, nil
}

// Out is a message with options.
type Out struct {
	Text      string
	Entities  []models.MessageEntity
	ReplyTo   int
	NoPreview bool                         // no link preview card
	Inline    *models.InlineKeyboardMarkup // nil = the reply keyboard every message carries
}

func (t *botTelegram) SendOut(ctx context.Context, chatID int64, o Out) (int, error) {
	p := &bot.SendMessageParams{ChatID: chatID, Text: o.Text, Entities: o.Entities, ReplyParameters: replyParams(o.ReplyTo)}
	if o.NoPreview {
		off := true
		p.LinkPreviewOptions = &models.LinkPreviewOptions{IsDisabled: &off}
	}
	if o.Inline != nil {
		p.ReplyMarkup = o.Inline
	} else {
		p.ReplyMarkup = t.keyboard(chatID)
	}
	m, err := t.b.SendMessage(ctx, p)
	if err != nil {
		return 0, err
	}
	return m.ID, nil
}

func (t *botTelegram) SendBare(ctx context.Context, chatID int64, text string, entities []models.MessageEntity) (int, error) {
	m, err := t.b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text, Entities: entities})
	if err != nil {
		return 0, err
	}
	return m.ID, nil
}

func (t *botTelegram) Unpin(ctx context.Context, chatID int64, msgID int) error {
	_, err := t.b.UnpinChatMessage(ctx, &bot.UnpinChatMessageParams{ChatID: chatID, MessageID: msgID})
	return err
}

func (t *botTelegram) EditText(ctx context.Context, chatID int64, msgID int, text string, entities []models.MessageEntity) error {
	_, err := t.b.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: chatID, MessageID: msgID, Text: text, Entities: entities})
	return err
}

func (t *botTelegram) Pin(ctx context.Context, chatID int64, msgID int) error {
	_, err := t.b.PinChatMessage(ctx, &bot.PinChatMessageParams{ChatID: chatID, MessageID: msgID, DisableNotification: true})
	return err
}

func (t *botTelegram) Delete(ctx context.Context, chatID int64, msgID int) error {
	_, err := t.b.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: chatID, MessageID: msgID})
	return err
}

func (t *botTelegram) SetMenuButton(ctx context.Context, chatID int64, text, url string) error {
	_, err := t.b.SetChatMenuButton(ctx, &bot.SetChatMenuButtonParams{ChatID: chatID,
		MenuButton: models.MenuButtonWebApp{Type: models.MenuButtonTypeWebApp, Text: text, WebApp: models.WebAppInfo{URL: url}}})
	return err
}

func (t *botTelegram) SendVoice(ctx context.Context, chatID int64, ogg []byte, replyTo int) (int, error) {
	m, err := t.b.SendVoice(ctx, &bot.SendVoiceParams{ChatID: chatID, ReplyParameters: replyParams(replyTo),
		ReplyMarkup: t.keyboard(chatID),
		Voice:       &models.InputFileUpload{Filename: "reply.ogg", Data: bytes.NewReader(ogg)}})
	if err != nil {
		return 0, err
	}
	return m.ID, nil
}

func (t *botTelegram) React(ctx context.Context, chatID int64, msgID int, emoji string) error {
	_, err := t.b.SetMessageReaction(ctx, &bot.SetMessageReactionParams{ChatID: chatID, MessageID: msgID,
		Reaction: []models.ReactionType{{Type: models.ReactionTypeTypeEmoji, ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: emoji}}}})
	return err
}

func (t *botTelegram) Draft(ctx context.Context, chatID int64, draftID int64, text string) error {
	_, err := t.b.SendMessageDraft(ctx, &bot.SendMessageDraftParams{ChatID: chatID,
		DraftID: strconv.FormatInt(draftID, 10), Text: text, CanStop: true})
	return err
}

func (t *botTelegram) Typing(ctx context.Context, chatID int64) error {
	_, err := t.b.SendChatAction(ctx, &bot.SendChatActionParams{ChatID: chatID, Action: models.ChatActionTyping})
	return err
}

// Download fetches a file within downloadTimeout: the event queue waits on it.
func (t *botTelegram) Download(ctx context.Context, fileID string) ([]byte, error) {
	if t.downloadTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.downloadTimeout)
		defer cancel()
	}
	b, err := t.download(ctx, fileID)
	if err != nil {
		return nil, errors.New(redact(err, t.token))
	}
	return b, nil
}

func (t *botTelegram) download(ctx context.Context, fileID string) ([]byte, error) {
	f, err := t.b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.b.FileDownloadLink(f), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (t *botTelegram) SetCommands(ctx context.Context, cmds []models.BotCommand) error {
	_, err := t.b.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: cmds})
	return err
}

func (t *botTelegram) Ping(ctx context.Context) error {
	_, err := t.Me(ctx)
	return err
}

func (t *botTelegram) Me(ctx context.Context) (string, error) {
	u, err := t.b.GetMe(ctx)
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// pollTimeout is getUpdates' long-poll wait.
const pollTimeout = 50 * time.Second

// poll is the getUpdates loop, until ctx ends. Updates go through h.intake in
// order, and the offset that confirms an update to Telegram moves past it only
// once intake has stored it (go-telegram/bot's own poller confirms a batch
// while it still sits in memory). A failed store is retried.
func (h *Hub) poll(ctx context.Context, apiURL, token string, allowed []string) error {
	client := &http.Client{Timeout: pollTimeout + 25*time.Second}
	var offset int64
	var errWait time.Duration
	for {
		if errWait > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(errWait):
			}
		}
		updates, retryAfter, err := getUpdates(ctx, client, apiURL, token, offset, allowed)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			h.telegramFailed(err)
			log.Printf("telegram: getUpdates: %s", redact(err, token))
			errWait = min(max(2*errWait, 100*time.Millisecond), 5*time.Second)
			if retryAfter > 0 {
				errWait = retryAfter
			}
			continue
		}
		errWait = 0
		h.markTelegramOK()
		for _, raw := range updates {
			u, err := decodeUpdate(raw)
			if u.ID == 0 {
				log.Printf("telegram: update without an id, skipped: %v: %s", err, raw)
				continue
			}
			if err != nil {
				log.Printf("telegram: update %d undecodable, skipped: %v: %s", u.ID, err, raw)
			} else {
				for attempt := 0; h.intake(u) != nil; attempt++ {
					if !h.sleepRetry(ctx, attempt) {
						return nil
					}
				}
			}
			offset = u.ID + 1
		}
	}
}

// getUpdates is one long-poll request; retryAfter is set on HTTP 429.
func getUpdates(ctx context.Context, client *http.Client, apiURL, token string, offset int64, allowed []string) ([]json.RawMessage, time.Duration, error) {
	body, _ := json.Marshal(map[string]any{"offset": offset, "timeout": int(pollTimeout / time.Second), "allowed_updates": allowed})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/bot"+token+"/getUpdates", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool              `json:"ok"`
		Result      []json.RawMessage `json:"result"`
		ErrorCode   int               `json:"error_code"`
		Description string            `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&r); err != nil {
		return nil, 0, fmt.Errorf("HTTP %d: %w", resp.StatusCode, err)
	}
	if !r.OK {
		err := fmt.Errorf("%d %s", r.ErrorCode, r.Description)
		if r.ErrorCode == http.StatusUnauthorized {
			err = fmt.Errorf("%w: %v", bot.ErrorUnauthorized, err)
		}
		return nil, time.Duration(r.Parameters.RetryAfter) * time.Second, err
	}
	return r.Result, 0, nil
}

// decodeUpdate decodes one update on its own; on failure the id is still read
// when possible, so the offset can move past it.
func decodeUpdate(raw json.RawMessage) (*models.Update, error) {
	raw = numericDraftID(raw)
	u := &models.Update{}
	err := json.Unmarshal(raw, u)
	if err == nil {
		return u, nil
	}
	var head struct {
		ID int64 `json:"update_id"`
	}
	json.Unmarshal(raw, &head)
	return &models.Update{ID: head.ID}, err
}

// numericDraftID turns stopped_message_generation.draft_id into a number:
// Telegram sends it as a string (the hub's drafts send it as one), while
// go-telegram's model wants an int, so the stop would be undecodable.
func numericDraftID(raw json.RawMessage) json.RawMessage {
	if !bytes.Contains(raw, []byte(`"stopped_message_generation"`)) {
		return raw
	}
	var u map[string]json.RawMessage
	if json.Unmarshal(raw, &u) != nil {
		return raw
	}
	var g map[string]json.RawMessage
	if json.Unmarshal(u["stopped_message_generation"], &g) != nil {
		return raw
	}
	var id string
	if json.Unmarshal(g["draft_id"], &id) != nil {
		return raw // already a number (or absent)
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return raw
	}
	g["draft_id"] = json.RawMessage(strconv.FormatInt(n, 10))
	u["stopped_message_generation"], _ = json.Marshal(g)
	out, err := json.Marshal(u)
	if err != nil {
		return raw
	}
	return out
}

// redact keeps the bot token (part of every Bot API URL) out of logs and chat.
func redact(err error, token string) string {
	if token == "" {
		return err.Error()
	}
	return strings.ReplaceAll(err.Error(), token, "<token>")
}

// newBotTelegram is the Bot API client the hub sends with.
func newBotTelegram(token, apiURL string) (*botTelegram, error) {
	b, err := bot.New(token, bot.WithServerURL(apiURL), bot.WithSkipGetMe(),
		bot.WithErrorsHandler(func(err error) { log.Printf("telegram: %s", redact(err, token)) }),
		bot.WithHTTPClient(65*time.Second, &http.Client{Timeout: 75 * time.Second}))
	if err != nil {
		return nil, err
	}
	return &botTelegram{b: b, token: token, downloadTimeout: 2 * time.Minute}, nil
}

// classify returns an authorized update's event identity, or ok=false for
// anything not from the owner in their private chat (an unpaired owner, zero,
// matches nothing). Every update kind is checked; rejects are dropped without
// a reply.
func classify(u *models.Update, o Owner) (e Event, ok bool, why string) {
	user, chat := o.UserID, o.ChatID
	privateFrom := func(c models.Chat, from *models.User) (bool, string) {
		switch {
		case !o.Paired():
			return false, "hub not paired"
		case from == nil:
			return false, "no sender"
		case from.ID != user:
			return false, fmt.Sprintf("user %d", from.ID)
		case c.Type != models.ChatTypePrivate || c.ID != chat:
			return false, fmt.Sprintf("chat %d type %q", c.ID, c.Type)
		}
		return true, ""
	}
	e.UpdateID = u.ID
	switch {
	case u.Message != nil && u.Message.PinnedMessage != nil && u.Message.From != nil && u.Message.From.IsBot &&
		o.Paired() && u.Message.Chat.Type == models.ChatTypePrivate && u.Message.Chat.ID == chat:
		// The "pinned a message" service line our own pin left in the owner's chat: it gets deleted.
		m := u.Message
		e.Kind, e.ChatID, e.MsgID = "pin_service", m.Chat.ID, m.ID
		e.EventID = fmt.Sprintf("tgp:%d:%d", m.Chat.ID, m.ID)
	case u.Message != nil:
		m := u.Message
		if ok, why := privateFrom(m.Chat, m.From); !ok {
			return e, false, "message from " + why
		}
		e.Kind, e.ChatID, e.MsgID = "message", m.Chat.ID, m.ID
		e.EventID = fmt.Sprintf("tg:%d:%d", m.Chat.ID, m.ID)
	case u.EditedMessage != nil:
		m := u.EditedMessage
		if ok, why := privateFrom(m.Chat, m.From); !ok {
			return e, false, "edit from " + why
		}
		e.Kind, e.ChatID, e.MsgID = "edited_message", m.Chat.ID, m.ID
		e.EventID = fmt.Sprintf("tge:%d", u.ID)
	case u.MessageReaction != nil:
		r := u.MessageReaction
		if ok, why := privateFrom(r.Chat, r.User); !ok {
			return e, false, "reaction from " + why
		}
		e.Kind, e.ChatID, e.MsgID = "message_reaction", r.Chat.ID, r.MessageID
		e.EventID = fmt.Sprintf("tgr:%d", u.ID)
	case u.CallbackQuery != nil:
		q := u.CallbackQuery
		if !o.Paired() || q.From.ID != user {
			return e, false, fmt.Sprintf("callback from user %d", q.From.ID)
		}
		if q.Message.Message != nil {
			if c := q.Message.Message.Chat; c.Type != models.ChatTypePrivate || c.ID != chat {
				return e, false, fmt.Sprintf("callback in chat %d", c.ID)
			}
			e.MsgID = q.Message.Message.ID
		}
		e.Kind, e.ChatID = "callback_query", chat
		e.EventID = "tgc:" + q.ID
	case u.StoppedMessageGeneration != nil:
		g := u.StoppedMessageGeneration // no sender field: the private chat is the user
		if !o.Paired() || g.Chat.Type != models.ChatTypePrivate || g.Chat.ID != chat {
			return e, false, fmt.Sprintf("stop in chat %d type %q", g.Chat.ID, g.Chat.Type)
		}
		e.Kind, e.ChatID = "stopped_message_generation", g.Chat.ID
		e.EventID = fmt.Sprintf("tgs:%d", u.ID)
	default:
		return e, false, "unhandled update kind"
	}
	raw, _ := json.Marshal(u)
	e.Raw = raw
	return e, true, ""
}
