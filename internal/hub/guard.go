package hub

import (
	"context"
	"fmt"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/ping"
)

// errNotOwner: a chat-addressed call to a chat that is not the paired
// owner's. It wraps ErrorForbidden so deliveries treat it as permanent.
var errNotOwner = fmt.Errorf("%w: not the paired owner's chat", bot.ErrorForbidden)

// errUnpaired: a bot-wide call (commands, callback answers) before pairing.
var errUnpaired = fmt.Errorf("%w: hub not paired", bot.ErrorForbidden)

// ownerGuard is the hub's only way to the Bot API: anything addressed to a
// chat other than the current owner's fails and sends nothing, so an unpaired
// hub (owner 0) sends nothing at all.
type ownerGuard struct {
	tg    Telegram
	owner func() int64 // the owner's chat (0 = unpaired)
}

func (g ownerGuard) ok(chatID int64) error {
	if o := g.owner(); o == 0 || chatID != o {
		return errNotOwner
	}
	return nil
}

func (g ownerGuard) paired() error {
	if g.owner() == 0 {
		return errUnpaired
	}
	return nil
}

func (g ownerGuard) Send(ctx context.Context, chatID int64, text string, replyTo int) (int, error) {
	if err := g.ok(chatID); err != nil {
		return 0, err
	}
	return g.tg.Send(ctx, chatID, text, replyTo)
}

func (g ownerGuard) SendVoice(ctx context.Context, chatID int64, ogg []byte, replyTo int) (int, error) {
	if err := g.ok(chatID); err != nil {
		return 0, err
	}
	return g.tg.SendVoice(ctx, chatID, ogg, replyTo)
}

func (g ownerGuard) React(ctx context.Context, chatID int64, msgID int, emoji string) error {
	if err := g.ok(chatID); err != nil {
		return err
	}
	return g.tg.React(ctx, chatID, msgID, emoji)
}

func (g ownerGuard) Draft(ctx context.Context, chatID int64, draftID int64, text string) error {
	if err := g.ok(chatID); err != nil {
		return err
	}
	return g.tg.Draft(ctx, chatID, draftID, text)
}

func (g ownerGuard) Typing(ctx context.Context, chatID int64) error {
	if err := g.ok(chatID); err != nil {
		return err
	}
	return g.tg.Typing(ctx, chatID)
}

func (g ownerGuard) Download(ctx context.Context, fileID string) ([]byte, error) {
	return g.tg.Download(ctx, fileID)
}

func (g ownerGuard) SetCommands(ctx context.Context, cmds []models.BotCommand) error {
	if err := g.paired(); err != nil {
		return err
	}
	return g.tg.SetCommands(ctx, cmds)
}

func (g ownerGuard) Ping(ctx context.Context) error { return g.tg.Ping(ctx) }

func (g ownerGuard) Me(ctx context.Context) (string, error) { return g.tg.Me(ctx) }

func (g ownerGuard) SendText(ctx context.Context, chatID int64, text string, entities []models.MessageEntity, replyTo int) (int, error) {
	if err := g.ok(chatID); err != nil {
		return 0, err
	}
	return g.tg.SendText(ctx, chatID, text, entities, replyTo)
}

func (g ownerGuard) SendBare(ctx context.Context, chatID int64, text string, entities []models.MessageEntity) (int, error) {
	if err := g.ok(chatID); err != nil {
		return 0, err
	}
	return g.tg.SendBare(ctx, chatID, text, entities)
}

func (g ownerGuard) EditText(ctx context.Context, chatID int64, msgID int, text string, entities []models.MessageEntity) error {
	if err := g.ok(chatID); err != nil {
		return err
	}
	return g.tg.EditText(ctx, chatID, msgID, text, entities)
}

func (g ownerGuard) Pin(ctx context.Context, chatID int64, msgID int) error {
	if err := g.ok(chatID); err != nil {
		return err
	}
	return g.tg.Pin(ctx, chatID, msgID)
}

func (g ownerGuard) Unpin(ctx context.Context, chatID int64, msgID int) error {
	if err := g.ok(chatID); err != nil {
		return err
	}
	return g.tg.Unpin(ctx, chatID, msgID)
}

func (g ownerGuard) Delete(ctx context.Context, chatID int64, msgID int) error {
	if err := g.ok(chatID); err != nil {
		return err
	}
	return g.tg.Delete(ctx, chatID, msgID)
}

func (g ownerGuard) SetMenuButton(ctx context.Context, chatID int64, text, url string) error {
	if err := g.ok(chatID); err != nil {
		return err
	}
	return g.tg.SetMenuButton(ctx, chatID, text, url)
}

func (g ownerGuard) SendOut(ctx context.Context, chatID int64, o Out) (int, error) {
	if err := g.ok(chatID); err != nil {
		return 0, err
	}
	return g.tg.SendOut(ctx, chatID, o)
}

func (g ownerGuard) SetKeyboard(fn func(chatID int64) models.ReplyMarkup) { g.tg.SetKeyboard(fn) }

func (g ownerGuard) SendPing(ctx context.Context, chatID int64, text string, o SendOpts) (int, error) {
	if err := g.ok(chatID); err != nil {
		return 0, err
	}
	return g.tg.SendPing(ctx, chatID, text, o)
}

func (g ownerGuard) SetButtons(ctx context.Context, chatID int64, msgID int, rows [][]ping.Button) error {
	if err := g.ok(chatID); err != nil {
		return err
	}
	return g.tg.SetButtons(ctx, chatID, msgID, rows)
}

func (g ownerGuard) AnswerCallback(ctx context.Context, queryID, text string) error {
	if err := g.paired(); err != nil {
		return err
	}
	return g.tg.AnswerCallback(ctx, queryID, text)
}
