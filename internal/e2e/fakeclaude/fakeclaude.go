// Package fakeclaude is the `claude` stub (docs/testing.md §6): it takes the flags the host passes to
// `claude -p --output-format stream-json …`, reads the prompt from stdin (or the argument) and answers in Claude
// Code's output shapes. Modes by prompt prefix: "echo: X" → X; "tts: X" → X plus <tts>X</tts>; "slow: S" → partial
// text for S seconds; "fail:" → exit 1; anything else → "ok". `--version` and `auth status` answer like a logged-in
// Claude Code; `plugin …` and `mcp …` run the real binary.
package fakeclaude

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Version is what `--version` reports (HOMMY_STUB_VERSION overrides it).
const Version = "2.1.323"

// Model is the model name the stub's events carry.
const Model = "claude-stub"

// Env is everything Run touches, so tests can drive it in-process.
type Env struct {
	Args           []string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	Cwd            string
	Now            func() time.Time
	After          func(time.Duration) <-chan time.Time
	// Exec replaces the process with the real claude (plugin/mcp); default syscall.Exec.
	Exec func(path string, argv []string, env []string) error
	Self string // the stub's own executable, skipped when looking for the real claude
}

// SlowTick is how often `slow:` emits partial text.
const SlowTick = 500 * time.Millisecond

type opts struct {
	print, verbose, partial bool
	format                  string
	resume, sessionID       string
	prompt                  []string
}

var valueFlags = map[string]bool{
	"--output-format": true, "--input-format": true, "--resume": true, "-r": true, "--session-id": true,
	"--append-system-prompt": true, "--system-prompt": true, "--model": true, "--settings": true,
	"--permission-mode": true, "--allowedTools": true, "--allowed-tools": true, "--disallowedTools": true,
	"--disallowed-tools": true, "--mcp-config": true, "--add-dir": true, "--max-turns": true, "--fallback-model": true,
	"--agents": true, "--setting-sources": true, "--plugin-dir": true, "--append-system-prompt-file": true,
	"--system-prompt-file": true, "--max-budget-usd": true, "--name": true,
}

func parse(args []string) (opts, error) {
	o := opts{format: "text"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		if valueFlags[name] {
			if !hasVal {
				if i+1 >= len(args) {
					return o, fmt.Errorf("error: option '%s' argument missing", name)
				}
				i++
				val = args[i]
			}
			switch name {
			case "--output-format":
				o.format = val
			case "--resume", "-r":
				o.resume = val
			case "--session-id":
				o.sessionID = val
			}
			continue
		}
		switch a {
		case "-p", "--print":
			o.print = true
		case "--verbose":
			o.verbose = true
		case "--include-partial-messages":
			o.partial = true
		default:
			if strings.HasPrefix(a, "-") {
				continue // a boolean flag the stub doesn't need (--dangerously-skip-permissions, …)
			}
			o.prompt = append(o.prompt, a)
		}
	}
	switch o.format {
	case "text", "json", "stream-json":
	default:
		return o, fmt.Errorf("error: option '--output-format <format>' argument '%s' is invalid. Allowed choices are text, json, stream-json", o.format)
	}
	if o.format == "stream-json" && o.print && !o.verbose {
		return o, errors.New("Error: When using --print, --output-format=stream-json requires --verbose")
	}
	return o, nil
}

func (e *Env) defaults() {
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.After == nil {
		e.After = time.After
	}
	if e.Exec == nil {
		e.Exec = syscall.Exec
	}
	if e.Cwd == "" {
		e.Cwd, _ = os.Getwd()
	}
}

func (e *Env) stateDir() string {
	if d := e.Getenv("HOMMY_STUB_STATE"); d != "" {
		return d
	}
	return filepath.Join(e.Getenv("HOME"), ".hommy-stub")
}

// logArgv appends {"at","argv","cwd"} to the argv log; never the environment.
func (e *Env) logArgv() {
	path := e.Getenv("HOMMY_STUB_LOG")
	if path == "" {
		path = filepath.Join(e.stateDir(), "argv.jsonl")
	}
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(map[string]any{"at": e.Now().UTC().Format(time.RFC3339Nano), "argv": e.Args, "cwd": e.Cwd})
	_, _ = f.Write(append(b, '\n'))
}

// Run is the stub's main; it returns the exit code.
func Run(e Env) int {
	e.defaults()
	e.logArgv()
	args := e.Args
	if len(args) > 0 {
		switch args[0] {
		case "--version", "-v":
			v := e.Getenv("HOMMY_STUB_VERSION")
			if v == "" {
				v = Version
			}
			fmt.Fprintf(e.Stdout, "%s (Claude Code)\n", v)
			return 0
		case "auth":
			return e.auth(args[1:])
		case "plugin", "plugins", "mcp":
			return e.forward()
		}
	}
	o, err := parse(args)
	if err != nil {
		fmt.Fprintln(e.Stderr, err)
		return 1
	}
	prompt := strings.Join(o.prompt, " ")
	if prompt == "" && e.Stdin != nil {
		b, _ := io.ReadAll(e.Stdin)
		prompt = string(b)
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" && o.print {
		fmt.Fprintln(e.Stderr, "Error: Input must be provided either through stdin or as a prompt argument when using --print")
		return 1
	}
	sid, turns, err := e.session(o)
	if err != nil {
		fmt.Fprintln(e.Stderr, err)
		return 1
	}
	return e.answer(o, prompt, sid, turns)
}

func (e *Env) auth(args []string) int {
	if len(args) == 0 || args[0] != "status" {
		fmt.Fprintln(e.Stderr, "the stub answers only `auth status`")
		return 1
	}
	for _, a := range args[1:] {
		if a == "--text" {
			fmt.Fprintln(e.Stdout, "Auth token: CLAUDE_CODE_OAUTH_TOKEN")
			return 0
		}
	}
	dir := filepath.Join(e.Getenv("HOME"), ".claude")
	if d := e.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		dir = d
	}
	b, _ := json.MarshalIndent(map[string]any{"loggedIn": true, "authMethod": "oauth_token", "apiProvider": "firstParty",
		"analyticsDisabled": false, "projectsDirectory": filepath.Join(dir, "projects"), "configDirectory": dir,
		"loginExpiresAt": nil}, "", "  ")
	fmt.Fprintln(e.Stdout, string(b))
	return 0
}

// realClaude is HOMMY_CLAUDE_BIN, else the first `claude` on PATH that is not this stub.
func (e *Env) realClaude() (string, error) {
	if p := e.Getenv("HOMMY_CLAUDE_BIN"); p != "" {
		return p, nil
	}
	self, _ := filepath.EvalSymlinks(e.Self)
	for _, dir := range filepath.SplitList(e.Getenv("PATH")) {
		p := filepath.Join(dir, "claude")
		r, err := filepath.EvalSymlinks(p)
		if err != nil || r == self {
			continue
		}
		if st, err := os.Stat(r); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", errors.New("claude-stub: no real claude to run `plugin`/`mcp` (set HOMMY_CLAUDE_BIN)")
}

func (e *Env) forward() int {
	p, err := e.realClaude()
	if err != nil {
		fmt.Fprintln(e.Stderr, err)
		return 1
	}
	argv := append([]string{p}, e.Args...)
	if err := e.Exec(p, argv, os.Environ()); err != nil {
		fmt.Fprintln(e.Stderr, "claude-stub:", err)
		return 1
	}
	return 0
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// session returns the session id (kept along a --resume chain) and its turn count, recorded under the state dir.
func (e *Env) session(o opts) (string, int, error) {
	dir := filepath.Join(e.stateDir(), "sessions")
	sid := o.sessionID
	turns := 0
	if o.resume != "" {
		b, err := os.ReadFile(filepath.Join(dir, filepath.Base(o.resume)))
		if err != nil {
			return "", 0, fmt.Errorf("No conversation found with session ID: %s", o.resume)
		}
		sid = o.resume
		turns, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if sid == "" {
		sid = newUUID()
	}
	turns++
	if err := os.MkdirAll(dir, 0o700); err == nil {
		_ = os.WriteFile(filepath.Join(dir, sid), []byte(strconv.Itoa(turns)+"\n"), 0o600)
	}
	return sid, turns, nil
}

type emitter struct {
	e   *Env
	o   opts
	sid string
	err error
}

func (m *emitter) event(v map[string]any) {
	if m.o.format != "stream-json" || m.err != nil {
		return
	}
	if _, ok := v["session_id"]; !ok {
		v["session_id"] = m.sid
	}
	v["uuid"] = newUUID()
	b, _ := json.Marshal(v)
	_, m.err = m.e.Stdout.Write(append(b, '\n'))
}

func (m *emitter) stream(ev map[string]any, msgID string) {
	if m.o.partial {
		m.event(map[string]any{"type": "stream_event", "event": ev, "parent_tool_use_id": nil, "api_message_id": msgID})
	}
}

func usageJSON(out int) map[string]any {
	return map[string]any{"input_tokens": 1, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": out}
}

func (e *Env) answer(o opts, prompt, sid string, turns int) int {
	start := e.Now()
	m := &emitter{e: e, o: o, sid: sid}
	tools := []string{"Task", "Bash", "Edit", "Glob", "Grep", "Read", "Write", "WebFetch", "WebSearch"}
	m.event(map[string]any{"type": "system", "subtype": "init", "cwd": e.Cwd, "tools": tools, "mcp_servers": []any{},
		"model": Model, "permissionMode": "default", "slash_commands": []any{}, "apiKeySource": "none",
		"claude_code_version": Version, "output_style": "default", "agents": []any{}, "skills": []any{}, "plugins": []any{}})

	lower := strings.ToLower(prompt)
	if strings.HasPrefix(lower, "fail:") {
		fmt.Fprintln(e.Stderr, "API Error: stub failure requested ("+strings.TrimSpace(prompt[5:])+")")
		return 1
	}
	msgID := "msg_stub_" + strings.ReplaceAll(newUUID(), "-", "")[:20]
	m.stream(map[string]any{"type": "message_start", "message": map[string]any{"model": Model, "id": msgID,
		"type": "message", "role": "assistant", "content": []any{}, "stop_reason": nil, "usage": usageJSON(1)}}, msgID)
	m.stream(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}, msgID)
	delta := func(t string) {
		m.stream(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": t}}, msgID)
	}

	var text string
	switch {
	case strings.HasPrefix(lower, "echo:"):
		text = strings.TrimSpace(prompt[5:])
		delta(text)
	case strings.HasPrefix(lower, "tts:"):
		x := strings.TrimSpace(prompt[4:])
		text = x + "\n<tts>" + x + "</tts>"
		delta(x)
		delta("\n<tts>" + x + "</tts>")
	case strings.HasPrefix(lower, "slow:"):
		secs, err := strconv.ParseFloat(strings.TrimSpace(prompt[5:]), 64)
		if err != nil || secs < 0 {
			secs = 1
		}
		end := start.Add(time.Duration(secs * float64(time.Second)))
		for i := 1; ; i++ {
			part := fmt.Sprintf("step %d. ", i)
			text += part
			delta(part)
			if !e.Now().Before(end) {
				break
			}
			<-e.After(min(SlowTick, end.Sub(e.Now())))
		}
		text = strings.TrimSpace(text)
	default:
		text = "ok"
		delta(text)
	}
	m.stream(map[string]any{"type": "content_block_stop", "index": 0}, msgID)
	m.event(map[string]any{"type": "assistant", "message": map[string]any{"model": Model, "id": msgID, "type": "message",
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": nil,
		"usage": usageJSON(len(text) / 4)}, "parent_tool_use_id": nil})
	m.stream(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": usageJSON(len(text) / 4)}, msgID)
	m.stream(map[string]any{"type": "message_stop"}, msgID)

	dur := e.Now().Sub(start).Milliseconds()
	result := map[string]any{"type": "result", "subtype": "success", "is_error": false, "duration_ms": dur,
		"duration_api_ms": dur, "num_turns": turns, "result": text, "stop_reason": "end_turn", "session_id": sid,
		"total_cost_usd": 0, "usage": usageJSON(len(text) / 4), "permission_denials": []any{}, "terminal_reason": "completed"}
	switch o.format {
	case "stream-json":
		m.event(result)
	case "json":
		result["uuid"] = newUUID()
		b, _ := json.Marshal(result)
		_, m.err = e.Stdout.Write(append(b, '\n'))
	default:
		_, m.err = fmt.Fprintln(e.Stdout, text)
	}
	if m.err != nil {
		return 1
	}
	return 0
}

// LookSelf returns the running executable's path (for Env.Self).
func LookSelf() string {
	p, err := os.Executable()
	if err != nil {
		p, _ = exec.LookPath(os.Args[0])
	}
	return p
}
