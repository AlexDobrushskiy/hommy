// Package hook is `hommy hook <event>`: the global Claude Code / Codex
// hooks. Each run reads the hook's JSON from stdin, adds what only the
// session's environment knows (agterm id, claude.ai bridge id) and hands it to
// the local host's hookrelay over a unix socket. It never writes stdout, always
// exits 0 and gives up within a hard timeout, so it can't block or change a
// session; when the host isn't listening the request is spooled to a file the
// host picks up later.
package hook

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Events, one subcommand each.
const (
	EventSessionStart     = "session-start"
	EventUserPromptSubmit = "user-prompt-submit"
	EventStop             = "stop"
	EventNotify           = "notify"
)

// Agents.
const (
	AgentClaude = "claude"
	AgentCodex  = "codex"
)

// Events is every hook subcommand.
var Events = []string{EventSessionStart, EventUserPromptSubmit, EventStop, EventNotify}

// Request is one hook run, as the host receives it.
type Request struct {
	Event    string          `json:"event"`
	Agent    string          `json:"agent"`
	Input    json.RawMessage `json:"input"` // the hook's stdin JSON as given (Codex notify: its argv JSON)
	AgtermID string          `json:"agterm_id,omitempty"`
	BridgeID string          `json:"bridge_id,omitempty"`
	At       time.Time       `json:"at"`
	PID      int             `json:"pid"`
	// SystemPrompt: a UserPromptSubmit whose prompt the agent made itself (a
	// background task's <task-notification>), not one a person typed.
	SystemPrompt bool `json:"system_prompt,omitempty"`
}

// Config is where the hook reports (the instance's host data dir and socket).
type Config struct {
	Socket string // HOOK_SOCKET: the host's hookrelay socket
	Spool  string // HOOK_SPOOL: dir for requests the host couldn't take
	Log    string // HOOK_LOG: one line per run
	Mode   string // HOOK_MODE (informational)
}

// ConfigPath is a hook env file (tests and manual runs).
func ConfigPath(home string) string { return filepath.Join(home, ".config/hommy/hook.env") }

// LoadConfig reads the hook env file; ok is false when there is none (the
// hooks are then no-ops).
func LoadConfig(path string) (Config, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, false
	}
	defer f.Close()
	var c Config
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "HOOK_SOCKET":
			c.Socket = v
		case "HOOK_SPOOL":
			c.Spool = v
		case "HOOK_LOG":
			c.Log = v
		case "HOOK_MODE":
			c.Mode = v
		}
	}
	return c, c.Socket != ""
}

// Timeout bounds a whole hook run (Claude Code waits for it before going on).
const Timeout = 3 * time.Second

// maxInput caps stdin (a Stop input carries the final answer).
const maxInput = 8 << 20

// Run is one hook invocation. It returns what happened, for the log; the
// caller always exits 0 and prints nothing.
//
// Only a Telegram-driven turn's content leaves the session: its prompt hook
// leaves a marker for the session (TurnsDir), and Stop / notify do nothing
// without one. A local prompt reports just the session (agterm id, claude.ai
// bridge id, cwd) for the hub's links, and isn't spooled unless it ends a
// Telegram turn.
func Run(ctx context.Context, cfg Config, agent, event string, stdin io.Reader, getenv func(string) string, now func() time.Time) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout-500*time.Millisecond)
	defer cancel()
	in, err := readInput(ctx, stdin)
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(in)) == 0 || !json.Valid(in) {
		return "", errors.New("stdin is not JSON")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(in, &fields) != nil {
		return "", errors.New("stdin is not a JSON object")
	}
	str := func(k string) string {
		var s string
		json.Unmarshal(fields[k], &s)
		return s
	}
	key := str("session_id")
	if key == "" {
		key = agent + ":" + getenv("AGTERM_SESSION_ID")
	}
	marker := filepath.Join(TurnsDir(cfg), markerName(key))
	inTurn := fileExists(marker)
	keep := true // spool it when the host can't take it
	system := false
	switch event {
	case EventSessionStart:
		keep = false
	case EventUserPromptSubmit:
		if TelegramMsgID(str("prompt")) != 0 {
			if err := writeMarker(marker, now()); err != nil {
				return "", fmt.Errorf("turn marker: %w", err)
			}
			break
		}
		// Before the first Stop, a background task notification continues the
		// Telegram turn. After that Stop its marker is gone: new wakes are local.
		if inTurn && !promptFromPerson(str("transcript_path"), str("prompt_id"), str("prompt")) {
			system = true
		} else if inTurn {
			os.Remove(marker)
		}
		keep = inTurn // it continues or ends a Telegram turn at the host
		delete(fields, "prompt")
		in, _ = json.Marshal(fields)
	case EventStop, EventNotify:
		if !inTurn {
			return "local turn", nil
		}
		if _, ok := fields["last_assistant_message"]; event == EventStop && !ok {
			// Read the answer now: by the time a spooled Stop is replayed the
			// transcript may hold a later turn.
			fields["last_assistant_message"], _ = json.Marshal(finalText(agent, str("transcript_path")))
			in, _ = json.Marshal(fields)
		}
	}
	req := Request{Event: event, Agent: agent, Input: in, AgtermID: getenv("AGTERM_SESSION_ID"),
		BridgeID: getenv("CLAUDE_CODE_BRIDGE_SESSION_ID"), At: now(), PID: os.Getpid(), SystemPrompt: system}
	body, _ := json.Marshal(req)
	if err := Post(ctx, cfg.Socket, body); err == nil {
		if event == EventStop {
			os.Remove(marker) // the host accepted the answer durably
		}
		return "sent", nil
	} else if errors.Is(err, errRefused) {
		return "", err
	} else if !keep {
		return "dropped (" + err.Error() + ")", nil
	} else if cfg.Spool == "" {
		return "", fmt.Errorf("host: %w (no spool dir)", err)
	} else if serr := spool(cfg.Spool, body, req.At); serr != nil {
		return "", fmt.Errorf("host: %v; spool: %w", err, serr)
	} else {
		if event == EventStop {
			os.Remove(marker) // replay owns the answer now; later wakes stay local
		}
		return "spooled (" + err.Error() + ")", nil
	}
}

// Post hands one request to the host's hookrelay socket.
func Post(ctx context.Context, socket string, body []byte) error {
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://hookrelay/hook", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return fmt.Errorf("%w: HTTP %d", errRefused, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hookrelay: HTTP %d", resp.StatusCode)
	}
	return nil
}

// promptFromPerson reports whether a person typed this prompt, or the agent
// made it (Claude Code: a background task's <task-notification>, absorbed
// mid-turn or starting a turn of its own). Claude Code writes the prompt's
// origin into the transcript before the hook runs: a user entry with this
// prompt id and text, or a queued_command attachment with this prompt. A
// notification absorbed mid-turn carries the turn's prompt id (whose entry is
// the person's prompt) and its attachment comes after the hook, so the text
// must match too. Without a match (another agent, an older version) the text
// decides.
func promptFromPerson(transcript, promptID, prompt string) bool {
	var e struct {
		Type     string `json:"type"`
		PromptID string `json:"promptId"`
		Message  struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Origin     *struct{ Kind string } `json:"origin"`
		Attachment struct {
			Type   string                 `json:"type"`
			Prompt string                 `json:"prompt"`
			Origin *struct{ Kind string } `json:"origin"`
		} `json:"attachment"`
	}
	if transcript != "" {
		lines := tailLines(transcript, 2<<20)
		for i := len(lines) - 1; i >= 0; i-- {
			e.Origin, e.Attachment.Origin, e.Message.Content = nil, nil, nil
			if json.Unmarshal(lines[i], &e) != nil {
				continue
			}
			if e.Type == "user" && e.Origin != nil && promptID != "" && e.PromptID == promptID &&
				strings.TrimSpace(contentText(e.Message.Content)) == strings.TrimSpace(prompt) {
				return e.Origin.Kind == "human"
			}
			if e.Type == "attachment" && e.Attachment.Type == "queued_command" && e.Attachment.Origin != nil &&
				e.Attachment.Prompt == prompt {
				return e.Attachment.Origin.Kind == "human"
			}
		}
	}
	return !strings.HasPrefix(strings.TrimSpace(prompt), "<task-notification>")
}

// contentText is a transcript message's text: a string, or its text blocks.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct{ Type, Text string }
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// errRefused: the host will never take this request (not worth spooling).
var errRefused = errors.New("hookrelay refused the request")

// readInput reads the hook's stdin, giving up when ctx ends (a session must
// not wait on a stdin that never closes).
func readInput(ctx context.Context, stdin io.Reader) ([]byte, error) {
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(stdin, maxInput))
		ch <- res{b, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("read stdin: %w", r.err)
		}
		return r.b, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("read stdin: %w", ctx.Err())
	}
}

// TurnsDir holds one marker per session whose current turn came from
// Telegram, beside the host's socket.
func TurnsDir(cfg Config) string { return filepath.Join(filepath.Dir(cfg.Socket), "hookturns") }

func markerName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

// keepMarker: a Telegram turn's marker outlives the host's turn (turnMaxAge).
const keepMarker = 2 * turnMaxAge

func writeMarker(path string, now time.Time) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if old, err := os.ReadDir(dir); err == nil { // drop markers of sessions long gone
		for _, e := range old {
			if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > keepMarker {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return os.WriteFile(path, nil, 0o600)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// spool writes the request where the host's relay picks it up (in name order).
func spool(dir string, body []byte, at time.Time) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%020d-%d.json", at.UnixNano(), os.Getpid())
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := writeSync(tmp, body); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

func writeSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// maxLog is where the hook log is cut back to its newest half.
const maxLog = 1 << 20

// Logf appends one line to the hook log (best effort).
func Logf(path, format string, args ...any) {
	if path == "" {
		return
	}
	if st, err := os.Stat(path); err == nil && st.Size() > maxLog {
		if b, err := os.ReadFile(path); err == nil {
			b = b[len(b)/2:]
			if i := bytes.IndexByte(b, '\n'); i >= 0 {
				b = b[i+1:]
			}
			os.WriteFile(path, b, 0o600)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s pid=%s %s\n", time.Now().Format(time.RFC3339Nano), strconv.Itoa(os.Getpid()), fmt.Sprintf(format, args...))
}
