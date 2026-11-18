package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/textx"
)

// StreamOptions configures one streamed `claude -p` run.
type StreamOptions struct {
	Bin                string
	Dir                string
	AppendSystemPrompt string
	Env                []string      // nil = CleanEnv(os.Environ())
	KillGrace          time.Duration // SIGTERM → SIGKILL on cancel; 0 = 5 s
	OnStart            func(pid int) // the process group id (= pid) once started
	OnProgress         func(Progress)
}

// Progress is the run's state so far: a status line and the text of the
// assistant message being written.
type Progress struct {
	Status string
	Text   string
}

// StreamResult is a finished run. SessionID is set as soon as claude reports
// it, so a failed or cancelled run still has one.
type StreamResult struct {
	Text      string
	SessionID string
	CostUSD   float64
}

// CleanEnv is the run environment rule (master spec §3.2): a run started from
// inside a Claude session would otherwise inherit its identity, so the
// session-identity variables go (CLAUDECODE, CLAUDE_CODE_ENTRYPOINT,
// CLAUDE_CODE_SESSION_*, CLAUDE_CODE_BRIDGE_SESSION_ID, every other
// CLAUDE_CODE_*), but auth stays: CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY and
// CLAUDE_CONFIG_DIR sign the run in. Everything else, PATH included, stays.
func CleanEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if keepAuth[k] {
			out = append(out, kv)
			continue
		}
		if strings.HasPrefix(k, "CLAUDE_CODE_") || k == "CLAUDECODE" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// keepAuth are the CLAUDE_CODE_* / auth variables a run keeps.
var keepAuth = map[string]bool{"CLAUDE_CODE_OAUTH_TOKEN": true, "ANTHROPIC_API_KEY": true, "CLAUDE_CONFIG_DIR": true}

// ClaudeVars lists the CLAUDE* variable names in env (for the run log).
func ClaudeVars(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "CLAUDE") {
			out = append(out, k)
		}
	}
	return out
}

// Stream runs `claude -p --output-format stream-json` in opts.Dir, reporting
// progress as it goes. The process runs in its own process group; when ctx
// ends the whole group gets SIGTERM, then SIGKILL after KillGrace, and the
// error wraps context.Cause(ctx). A --resume of a vanished session is retried
// once without it.
func Stream(ctx context.Context, opts StreamOptions, sessionID, prompt string) (StreamResult, error) {
	res, err := streamOnce(ctx, opts, sessionID, prompt)
	if err != nil && ctx.Err() == nil && sessionID != "" && strings.Contains(strings.ToLower(err.Error()), "no conversation found") {
		return streamOnce(ctx, opts, "", prompt)
	}
	return res, err
}

func streamOnce(ctx context.Context, opts StreamOptions, sessionID, prompt string) (StreamResult, error) {
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--dangerously-skip-permissions"}
	if opts.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.AppendSystemPrompt)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	grace := opts.KillGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	env := opts.Env
	if env == nil {
		env = CleanEnv(os.Environ())
	}

	p := newParser(opts.Dir)
	var mu sync.Mutex // the parser is fed by exec's copy goroutine and read here after Wait
	lw := &lineWriter{fn: func(line []byte) {
		mu.Lock()
		changed := p.feed(line)
		pr := p.progress()
		mu.Unlock()
		if changed && opts.OnProgress != nil {
			opts.OnProgress(pr)
		}
	}}
	var stderr bytes.Buffer

	cmd := exec.Command(opts.Bin, args...)
	cmd.Dir = opts.Dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout = lw
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A grandchild holding stdout open must not keep Wait hanging after a kill.
	cmd.WaitDelay = grace + 2*time.Second

	if err := cmd.Start(); err != nil {
		return StreamResult{}, fmt.Errorf("claude failed to start: %w", err)
	}
	pgid := cmd.Process.Pid
	if opts.OnStart != nil {
		opts.OnStart(pgid)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var werr error
	groups := []int{pgid}
	select {
	case werr = <-waited:
	case <-ctx.Done():
		// Tool commands run in groups of their own: take them along, and
		// remember them, since they are no longer findable once claude is gone.
		groups = TreeGroups(pgid)
		SignalGroups(groups, syscall.SIGTERM)
		select {
		case werr = <-waited:
		case <-time.After(grace):
			groups = union(groups, TreeGroups(pgid))
			SignalGroups(groups, syscall.SIGKILL)
			werr = <-waited
		}
	}
	// Whatever the run left behind goes too.
	SignalGroups(groups, syscall.SIGKILL)
	lw.flush()

	mu.Lock()
	defer mu.Unlock()
	res := StreamResult{Text: p.result, SessionID: p.sessionID, CostUSD: p.cost}
	if ctx.Err() != nil {
		res.Text = p.text
		return res, fmt.Errorf("claude run ended: %w", context.Cause(ctx))
	}
	if werr != nil {
		return res, fmt.Errorf("claude failed: %v: %s", werr, textx.Tail(stderr.String(), 500))
	}
	if !p.done {
		return res, fmt.Errorf("claude ended without a result: %s", textx.Tail(stderr.String(), 500))
	}
	if p.isError {
		return res, fmt.Errorf("claude error: %s", textx.Tail(p.result, 500))
	}
	return res, nil
}

// lineWriter splits a byte stream into lines.
type lineWriter struct {
	buf []byte
	fn  func([]byte)
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.fn(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	return len(b), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.fn(w.buf)
		w.buf = nil
	}
}

// parser folds stream-json events into the run's state.
type parser struct {
	dir       string
	sessionID string
	status    string
	text      string // the top-level assistant message being written
	newMsg    bool   // the next text delta starts a new message's text
	result    string
	cost      float64
	isError   bool
	done      bool
}

func newParser(dir string) *parser { return &parser{dir: dir, status: "⏳ starting"} }

func (p *parser) progress() Progress { return Progress{Status: p.status, Text: p.text} }

type streamLine struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	SessionID       string          `json:"session_id"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	Event           json.RawMessage `json:"event"`
	Message         struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

type streamEvent struct {
	Type         string `json:"type"`
	ContentBlock struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

// feed applies one line; true when the progress changed.
func (p *parser) feed(line []byte) bool {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return false
	}
	var l streamLine
	if json.Unmarshal(line, &l) != nil {
		return false
	}
	if l.SessionID != "" {
		p.sessionID = l.SessionID
	}
	before := p.progress()
	sub := l.ParentToolUseID != nil && *l.ParentToolUseID != "" // a subagent's events
	switch l.Type {
	case "system":
		if l.Subtype == "status" || l.Subtype == "init" {
			p.status = "💭 thinking"
		}
	case "stream_event":
		var e streamEvent
		if json.Unmarshal(l.Event, &e) != nil {
			break
		}
		switch e.Type {
		case "message_start":
			if !sub {
				p.newMsg = true
			}
		case "content_block_start":
			switch e.ContentBlock.Type {
			case "thinking", "redacted_thinking":
				p.status = "💭 thinking"
			case "tool_use", "server_tool_use":
				p.status = "⚙ " + e.ContentBlock.Name
			case "text":
				if !sub {
					p.status = "✍ writing"
				}
			}
		case "content_block_delta":
			if e.Delta.Type == "text_delta" && !sub {
				if p.newMsg {
					p.text, p.newMsg = "", false
				}
				p.text += e.Delta.Text
			}
		}
	case "assistant":
		// The complete message: a tool call's input is known now.
		for _, c := range l.Message.Content {
			if c.Type == "tool_use" || c.Type == "server_tool_use" {
				p.status = "⚙ " + c.Name
				if hint := toolHint(c.Input, p.dir); hint != "" {
					p.status += " · " + hint
				}
			}
		}
	case "user":
		p.status = "💭 thinking"
	case "result":
		p.done = true
		p.result, p.isError, p.cost = l.Result, l.IsError, l.TotalCostUSD
	}
	return p.progress() != before
}

// toolHint is a short description of a tool call's subject (file, command, query).
func toolHint(input json.RawMessage, dir string) string {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	for _, k := range []string{"file_path", "notebook_path", "path", "description", "pattern", "query", "url", "skill", "command", "prompt"} {
		s, ok := in[k].(string)
		if !ok || strings.TrimSpace(s) == "" {
			continue
		}
		if strings.HasSuffix(k, "path") {
			s = shortPath(s, dir)
		}
		s = strings.Join(strings.Fields(s), " ")
		return textx.Head(s, 48)
	}
	return ""
}

// shortPath shows a path relative to the run dir (or ~) when it is under it.
func shortPath(p, dir string) string {
	if dir != "" {
		if rel, err := filepath.Rel(dir, p); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
	}
	return p
}
