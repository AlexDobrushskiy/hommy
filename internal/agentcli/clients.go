package agentcli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/ping"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- hub (host API) ----

// hubHTTP and hubAttemptTimeout are variables for tests.
var (
	hubHTTP           = http.DefaultClient
	hubAttemptTimeout = 20 * time.Second
)

// errUnreachable: the hub could not be reached at all, so nothing was sent.
var errUnreachable = errors.New("hub unreachable")

// hubError is an answer from the hub that isn't success.
type hubError struct {
	Status int
	Msg    string
	Sent   bool // some messages went out before it failed
}

func (e *hubError) Error() string { return fmt.Sprintf("hub: HTTP %d: %s", e.Status, e.Msg) }

// hubPost POSTs JSON to the hub's host API. A connection that could not be
// made is errUnreachable; anything after that may have reached the hub.
func hubPost(ctx context.Context, c Config, path string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase()+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("X-Hommy-Host", c.HostName)
	var connected atomic.Bool // a connection to the hub existed: the request may have reached it
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}))
	resp, err := hubHTTP.Do(req)
	if err != nil {
		var op *net.OpError
		if !connected.Load() || (errors.As(err, &op) && op.Op == "dial") {
			return fmt.Errorf("%w: %v", errUnreachable, err)
		}
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var r proto.SendResponse
		msg := string(bytes.TrimSpace(data))
		if json.Unmarshal(data, &r) == nil && r.Error != "" {
			msg = r.Error
		}
		return &hubError{Status: resp.StatusCode, Msg: msg, Sent: len(r.MessageIDs) > 0}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// maybeSentError: a request reached the hub but no answer came back, so the
// hub may have sent it. Never sent again around the hub.
type maybeSentError struct{ err error }

func (e *maybeSentError) Error() string {
	return "the hub got the request but didn't answer (it may have been sent; not sending it again): " + e.err.Error()
}

// hubRetry runs post until it succeeds or the hub answers for good (4xx / a
// partial send). Lost answers and 5xx are retried with the same request
// (idempotent per id). errUnreachable comes back only when no attempt reached
// the hub, the one case where sending around it can't duplicate.
func hubRetry(ctx context.Context, sleep func(time.Duration), post func(ctx context.Context) error) error {
	var err error
	reached := false
	for _, wait := range []time.Duration{0, time.Second, 3 * time.Second} {
		if wait > 0 {
			sleep(wait)
		}
		actx, cancel := context.WithTimeout(ctx, hubAttemptTimeout)
		err = post(actx)
		cancel()
		var he *hubError
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errUnreachable):
			if !reached {
				return err
			}
		case errors.As(err, &he):
			if he.Status < 500 || he.Sent {
				return err
			}
			reached = true // a 5xx may follow a send whose Telegram answer was lost
		default:
			reached = true
		}
	}
	if reached {
		return &maybeSentError{err}
	}
	return err
}

// ---- Bot API (direct) ----

// botError is a Bot API failure, worded as send.py worded it.
type botError struct{ msg string }

func (e *botError) Error() string { return e.msg }

func botCall(ctx context.Context, c Config, method string, body any, out any) error {
	b, _ := json.Marshal(body)
	return botPost(ctx, c, method, "application/json", b, out)
}

func botPost(ctx context.Context, c Config, method, contentType string, b []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BotAPI+"/bot"+c.Token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return &botError{err.Error()}
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) && ue.Unwrap() != nil {
			err = ue.Unwrap() // drop the URL (it carries the token)
		}
		return &botError{err.Error()}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(data, &r)
	if resp.StatusCode != http.StatusOK {
		return &botError{fmt.Sprintf("HTTP %d: %s", resp.StatusCode, r.Description)}
	}
	if !r.OK {
		return &botError{r.Description}
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

type directOpts struct {
	silent bool
}

// botSend is one sendMessage, posted exactly as send.py posted it (form
// fields included); returns the message id.
func botSend(ctx context.Context, c Config, text string, o directOpts) (int, error) {
	form := url.Values{"chat_id": {c.Chat}, "text": {text}, "disable_web_page_preview": {"false"}}
	if o.silent {
		form.Set("disable_notification", "true")
	}
	var m struct {
		ID int `json:"message_id"`
	}
	if err := botPost(ctx, c, "sendMessage", "application/x-www-form-urlencoded", []byte(form.Encode()), &m); err != nil {
		return 0, err
	}
	return m.ID, nil
}

func botButtons(ctx context.Context, c Config, msgID int, labels []string) error {
	return botCall(ctx, c, "editMessageReplyMarkup", map[string]any{"chat_id": c.Chat, "message_id": msgID,
		"reply_markup": map[string]any{"inline_keyboard": ping.Keyboard(msgID, labels)}}, nil)
}

// ---- spool ----

// writeSpool stores an entry for the host to forward (atomic, 0600).
func writeSpool(c Config, s proto.SpoolEntry) error {
	if err := os.MkdirAll(c.SpoolDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.SpoolDir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(c.SpoolDir, s.ID+".json"))
}
