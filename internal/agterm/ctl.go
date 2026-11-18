package agterm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Session is one agterm session as `agtermctl tree --json` reports it.
type Session struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Cwd             string   `json:"cwd"`
	Title           string   `json:"title"`
	Status          string   `json:"status"`
	Foreground      []string `json:"foreground"`
	SplitForeground []string `json:"splitForeground"`
	Split           bool     `json:"split"`
	Realized        bool     `json:"realized"`
	Workspace       string   `json:"-"`
	WorkspaceID     string   `json:"-"`
	Window          string   `json:"-"`
}

// NewSessionOpts are the flags of `agtermctl session new`.
type NewSessionOpts struct {
	Workspace string
	Cwd       string
	Name      string
	Command   string
}

// Ctl is the part of agtermctl the host uses. ExecCtl runs the real binary.
type Ctl interface {
	Sessions(ctx context.Context) ([]Session, error)
	// Text returns the last `lines` lines of the session's primary pane.
	Text(ctx context.Context, s Session, lines int) (string, error)
	// Cursor returns the zero-based cursor column of the primary pane.
	Cursor(ctx context.Context, s Session) (int, error)
	// Type injects text into the primary pane.
	Type(ctx context.Context, s Session, text string) error
	NewSession(ctx context.Context, o NewSessionOpts) (string, error)
	// Close closes a session (by id only: never the session the owner is viewing).
	Close(ctx context.Context, s Session) error
}

const primaryPane = "left"

// ExecCtl drives agterm through the agtermctl CLI.
type ExecCtl struct {
	Path    string
	Timeout time.Duration
}

// FindAgtermctl locates agtermctl: $AGTERMCTL, PATH, then the install locations
// a launchd/systemd service (no login PATH) would miss.
func FindAgtermctl() (string, error) {
	if p := os.Getenv("AGTERMCTL"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("agtermctl"); err == nil {
		return p, nil
	}
	for _, p := range []string{
		"/opt/homebrew/bin/agtermctl",
		"/Applications/agterm.app/Contents/MacOS/agtermctl",
		"/usr/bin/agtermctl",
		"/usr/local/bin/agtermctl",
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("agtermctl not found (AGTERMCTL, PATH, /opt/homebrew/bin, /Applications/agterm.app, /usr/bin)")
}

// NewExecCtl returns a Ctl for the local agterm.
func NewExecCtl() (*ExecCtl, error) {
	p, err := FindAgtermctl()
	if err != nil {
		return nil, err
	}
	return &ExecCtl{Path: p, Timeout: 30 * time.Second}, nil
}

func (c *ExecCtl) run(ctx context.Context, stdin string, args ...string) (string, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Path, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errb.String())
		if detail == "" {
			detail = strings.TrimSpace(out.String())
		}
		return "", fmt.Errorf("agtermctl %s: %w: %s", args[0]+" "+safeArg(args, 1), err, detail)
	}
	return out.String(), nil
}

func safeArg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

type windowList struct {
	Result struct {
		Windows []struct {
			ID   string `json:"id"`
			Open bool   `json:"open"`
		} `json:"windows"`
	} `json:"result"`
}

type treeDoc struct {
	OK     bool `json:"ok"`
	Result struct {
		Tree struct {
			Workspaces []struct {
				ID       string    `json:"id"`
				Name     string    `json:"name"`
				Sessions []Session `json:"sessions"`
			} `json:"workspaces"`
		} `json:"tree"`
	} `json:"result"`
}

// Sessions lists the sessions of every open window.
func (c *ExecCtl) Sessions(ctx context.Context) ([]Session, error) {
	out, err := c.run(ctx, "", "window", "list", "--json")
	if err != nil {
		return nil, err
	}
	var wl windowList
	if err := json.Unmarshal([]byte(out), &wl); err != nil {
		return nil, fmt.Errorf("agtermctl window list: %w", err)
	}
	var all []Session
	for _, w := range wl.Result.Windows {
		if !w.Open || w.ID == "" {
			continue
		}
		out, err := c.run(ctx, "", "tree", "--json", "--window", w.ID)
		if err != nil {
			return nil, err
		}
		ss, err := ParseTree([]byte(out), w.ID)
		if err != nil {
			return nil, err
		}
		all = append(all, ss...)
	}
	return all, nil
}

// ParseTree flattens one window's `tree --json` into sessions.
func ParseTree(data []byte, window string) ([]Session, error) {
	var doc treeDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("agtermctl tree: %w", err)
	}
	var out []Session
	for _, ws := range doc.Result.Tree.Workspaces {
		for _, s := range ws.Sessions {
			s.Workspace, s.WorkspaceID, s.Window = ws.Name, ws.ID, window
			out = append(out, s)
		}
	}
	return out, nil
}

func windowArgs(s Session) []string {
	if s.Window == "" {
		return nil
	}
	return []string{"--window", s.Window}
}

func (c *ExecCtl) Text(ctx context.Context, s Session, lines int) (string, error) {
	args := append([]string{"session", "text", "--pane", primaryPane, "--target", s.ID, "--lines", strconv.Itoa(lines)}, windowArgs(s)...)
	return c.run(ctx, "", args...)
}

func (c *ExecCtl) Cursor(ctx context.Context, s Session) (int, error) {
	args := append([]string{"surface", "cursor", "--target", "surface:" + s.ID + ":" + primaryPane}, windowArgs(s)...)
	out, err := c.run(ctx, "", args...)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("surface cursor returned %q", strings.TrimSpace(out))
	}
	return n, nil
}

func (c *ExecCtl) Type(ctx context.Context, s Session, text string) error {
	args := append([]string{"session", "type", "--stdin", "--pane", primaryPane, "--target", s.ID}, windowArgs(s)...)
	_, err := c.run(ctx, text, args...)
	return err
}

func newSessionArgs(o NewSessionOpts) []string {
	return []string{"session", "new", "--workspace-name", o.Workspace, "--create-workspace",
		"--cwd", o.Cwd, "--name", o.Name, "--no-select", "--command", o.Command}
}

// NewSessionCommand is the agtermctl command line NewSession runs, shell-quoted (setup --dry-run prints it).
func NewSessionCommand(o NewSessionOpts) string {
	parts := []string{"agtermctl"}
	for _, a := range newSessionArgs(o) {
		parts = append(parts, shQuote(a))
	}
	return strings.Join(parts, " ")
}

func (c *ExecCtl) NewSession(ctx context.Context, o NewSessionOpts) (string, error) {
	out, err := c.run(ctx, "", newSessionArgs(o)...)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return "", errors.New("agtermctl session new printed no id")
	}
	return id, nil
}

func (c *ExecCtl) Close(ctx context.Context, s Session) error {
	if strings.TrimSpace(s.ID) == "" {
		return errors.New("close: no session id")
	}
	_, err := c.run(ctx, "", append([]string{"session", "close", "--target", s.ID}, windowArgs(s)...)...)
	return err
}

// FindSession resolves an id or unique id prefix (case-insensitive).
func FindSession(sessions []Session, id string) (Session, bool) {
	needle := strings.ToLower(strings.TrimSpace(id))
	if needle == "" {
		return Session{}, false
	}
	var match []Session
	for _, s := range sessions {
		sid := strings.ToLower(s.ID)
		if sid == needle {
			return s, true
		}
		if strings.HasPrefix(sid, needle) {
			match = append(match, s)
		}
	}
	if len(match) == 1 {
		return match[0], true
	}
	return Session{}, false
}
