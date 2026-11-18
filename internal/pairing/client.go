package pairing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Code is a new pairing code (POST /api/pairing with {}).
type Code struct {
	Code      string    `json:"code"` // XXXX-XXXX
	ExpiresAt time.Time `json:"expires_at"`
	Bot       string    `json:"bot"`  // @name
	Link      string    `json:"link"` // https://t.me/<bot>?start=<code>
}

// State is GET /api/pairing (also the answer to a reset or a bind).
type State struct {
	Paired         bool       `json:"paired"`
	Pending        bool       `json:"pending"`
	ExpiresAt      *time.Time `json:"expires_at"`              // the pending code's; nil when none
	OwnerChatID    int64      `json:"owner_chat_id,omitempty"` // for OWNER_CHAT_ID (not a secret); 0 = unpaired
	OwnerFirstName string     `json:"owner_first_name,omitempty"`
}

// Request is the body of POST /api/pairing: {} = new code.
type Request struct {
	Reset   bool  `json:"reset,omitempty"`
	OwnerID int64 `json:"owner_id,omitempty"`
}

// Status is GET /api/status.
type Status struct {
	Version  string `json:"version"`
	Telegram string `json:"telegram"` // ok | stale | unauthorized
	Paired   bool   `json:"paired"`
	Owner    *Owner `json:"owner"`
	Hosts    []Host `json:"hosts"`
}

// Owner is the paired owner as /api/status shows it.
type Owner struct {
	FirstName string `json:"first_name"`
}

// Host is one host in /api/status.
type Host struct {
	Name     string            `json:"name"`
	Online   bool              `json:"online"`
	LastSeen *time.Time        `json:"last_seen"`
	Version  string            `json:"version"`
	Checks   map[string]string `json:"checks"`
}

// APIError is a non-2xx answer from the hub.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("hub: HTTP %d: %s", e.Status, e.Msg) }

// Client talks to the hub's HTTP API with a host key.
type Client struct {
	BaseURL  string // http://host:port (see BaseURL)
	HostName string // X-Hommy-Host
	Key      string // the host's key
	HTTP     *http.Client
}

// BaseURL turns HUB_URL (ws://host:port/ws) into the API base (http://host:port).
func BaseURL(hubURL string) (string, error) {
	u, err := url.Parse(hubURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "ws", "http":
		u.Scheme = "http"
	case "wss", "https":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("HUB_URL %q: want ws:// or wss://", hubURL)
	}
	if u.Host == "" {
		return "", fmt.Errorf("HUB_URL %q: no host", hubURL)
	}
	return u.Scheme + "://" + u.Host, nil
}

// New makes a new pairing code (a pending one is replaced).
func (c Client) New(ctx context.Context) (Code, error) {
	var out Code
	return out, c.do(ctx, http.MethodPost, "/api/pairing", Request{}, &out)
}

// Status is the pairing state.
func (c Client) Status(ctx context.Context) (State, error) {
	var out State
	return out, c.do(ctx, http.MethodGet, "/api/pairing", nil, &out)
}

// Reset clears the owner and any pending code.
func (c Client) Reset(ctx context.Context) (State, error) {
	var out State
	return out, c.do(ctx, http.MethodPost, "/api/pairing", Request{Reset: true}, &out)
}

// Bind makes Telegram user ownerID the owner without a code.
func (c Client) Bind(ctx context.Context, ownerID int64) (State, error) {
	if ownerID <= 0 {
		return State{}, fmt.Errorf("owner id must be positive")
	}
	var out State
	return out, c.do(ctx, http.MethodPost, "/api/pairing", Request{OwnerID: ownerID}, &out)
}

// APIStatus is the hub's /api/status.
func (c Client) APIStatus(ctx context.Context) (Status, error) {
	var out Status
	return out, c.do(ctx, http.MethodGet, "/api/status", nil, &out)
}

func (c Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("X-Hommy-Host", c.HostName)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &APIError{Status: resp.StatusCode, Msg: msg}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("hub: bad answer: %w", err)
	}
	return nil
}
