package agterm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/sanitize"
)

// Result statuses (the proto.Result statuses of the host job contract).
const (
	StatusOK        = "ok"
	StatusInbox     = "inbox"
	StatusNoSession = "no_session"
	StatusError     = "error"
)

// DefaultWorkspace is the agterm workspace new sessions from Telegram are
// created in (AGTERM_WORKSPACE overrides it).
const DefaultWorkspace = "hommy"

// Service runs the agterm session jobs on one host.
type Service struct {
	Ctl   Ctl
	Env   Env
	Clock Clock
	Queue *Queue
	// ClaudeCommand is the fixed command new sessions run (AgentCommandLine);
	// never taken from a job.
	ClaudeCommand string
	// Workspace is the agterm workspace new sessions are created in.
	Workspace string
	// ComposerWait bounds how long new_session waits for the first composer.
	ComposerWait time.Duration
	// NoteTyped is told which session a Telegram message is about to be typed
	// into (the hook relay maps the turn to it).
	NoteTyped func(msgID int64, session string)

	known   *knownBriefs
	spawned *spawnLog
}

// canonical resolves an id or unique prefix to the full session id: the live
// session's, else one the host remembers. ok reports a live session.
func (s *Service) canonical(sessions []Session, id string) (Session, bool) {
	if sess, ok := FindSession(sessions, id); ok {
		return sess, true
	}
	return Session{ID: s.known.resolve(id)}, false
}

// briefFor resolves a session's brief folder and remembers it, so a message to a
// session that has since ended still finds the folder (and its successor).
func (s *Service) briefFor(id, cwd string) string {
	if dir := s.Env.BriefDirFor(id, cwd); dir != "" {
		s.known.remember(id, dir)
		return dir
	}
	return s.known.lookup(id)
}

// NewService wires a Service for the local agterm.
func NewService(ctl Ctl, env Env, later func(Attempt)) *Service {
	q := &Queue{Ctl: ctl, Clock: RealClock, Path: filepath.Join(env.StateDir, "typing-queue.json"), Later: later}
	return &Service{Ctl: ctl, Env: env, Clock: RealClock, Queue: q, ClaudeCommand: DefaultClaudeCommand(), Workspace: DefaultWorkspace, ComposerWait: 2 * time.Minute,
		known:   &knownBriefs{path: filepath.Join(env.StateDir, "session-briefs.json")},
		spawned: &spawnLog{path: filepath.Join(env.StateDir, "new-sessions.json")}}
}

// DefaultClaudeCommand is the command line new sessions run when the host sets
// none: `claude` with skipped permissions, HOME passed on (AgentCommandLine).
func DefaultClaudeCommand() string {
	home, _ := os.UserHomeDir()
	return AgentCommandLine("claude", home, os.Getenv("CLAUDE_CONFIG_DIR"), true)
}

// AgentCommandLine is what agterm runs for a new session (master spec §3.2):
//
//	/bin/zsh -l -c 'env HOME=<home> [CLAUDE_CONFIG_DIR=<dir>] <agent> [--dangerously-skip-permissions]; exec /bin/zsh -l'
//
// agterm starts sessions with its own environment, so the host's HOME (and
// CLAUDE_CONFIG_DIR) go on the command line. agent is shell text (a path, maybe
// with arguments); the login zsh stays as the session's shell once it exits.
func AgentCommandLine(agent, home, claudeConfigDir string, skip bool) string {
	return AgentCommandLineArgs(agent, home, claudeConfigDir, skip)
}

// AgentCommandLineArgs is AgentCommandLine with extra arguments for the agent
// after the permission flag (each single-quoted), e.g. a first prompt.
func AgentCommandLineArgs(agent, home, claudeConfigDir string, skip bool, extra ...string) string {
	zsh := "/bin/zsh"
	if runtime.GOOS != "darwin" {
		if p, err := exec.LookPath("zsh"); err == nil {
			zsh = p
		}
	}
	inner := "env HOME=" + shQuote(home)
	if claudeConfigDir != "" {
		inner += " CLAUDE_CONFIG_DIR=" + shQuote(claudeConfigDir)
	}
	inner += " " + agent
	if skip {
		inner += " --dangerously-skip-permissions"
	}
	for _, a := range extra {
		inner += " " + shQuote(a)
	}
	inner += "; exec " + zsh + " -l"
	return zsh + " -l -c " + shQuote(inner)
}

// shQuote single-quotes s for a POSIX shell.
func shQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:,@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (s *Service) now() time.Time { return s.Clock.Now() }

// ---- inbox_append ----

type InboxAppendRequest struct {
	Session  string `json:"session,omitempty"`
	BriefDir string `json:"brief_dir,omitempty"`
	MsgID    int64  `json:"msg_id"`
	Kind     string `json:"kind,omitempty"` // text (default) | voice | photo | reaction | button …
	Text     string `json:"text"`
	Quote    string `json:"quote,omitempty"`
	ReplyTo  int64  `json:"reply_to,omitempty"`
	EventID  string `json:"event_id,omitempty"`
	ID       string `json:"id,omitempty"` // inbox item id; default tg-<msg_id>
}

type InboxAppendResult struct {
	Status   string `json:"status"`
	Path     string `json:"path,omitempty"`
	Appended bool   `json:"appended"`
	Error    string `json:"error,omitempty"`
}

func (s *Service) inboxFor(sessions []Session, sessionID, briefDir string) (string, error) {
	if briefDir != "" {
		real, err := s.Env.ValidBriefDir(briefDir)
		if err != nil {
			return "", err
		}
		return real, nil
	}
	sess, _ := s.canonical(sessions, sessionID)
	return s.briefFor(sess.ID, sess.Cwd), nil
}

func (s *Service) InboxAppend(ctx context.Context, r InboxAppendRequest) InboxAppendResult {
	if r.Session == "" && r.BriefDir == "" {
		return InboxAppendResult{Status: StatusError, Error: "session or brief_dir is required"}
	}
	sessions, _ := s.Ctl.Sessions(ctx) // the inbox works without agterm
	if r.Session != "" {
		sess, _ := s.canonical(sessions, r.Session)
		r.Session = sess.ID
	}
	dir, err := s.inboxFor(sessions, r.Session, r.BriefDir)
	if err != nil {
		return InboxAppendResult{Status: StatusError, Error: err.Error()}
	}
	path := s.Env.InboxPath(dir, r.Session)
	ok, err := AppendInbox(path, s.record(r))
	if err != nil {
		return InboxAppendResult{Status: StatusError, Path: path, Error: err.Error()}
	}
	return InboxAppendResult{Status: StatusOK, Path: path, Appended: ok}
}

func (s *Service) record(r InboxAppendRequest) InboxRecord {
	kind := r.Kind
	if kind == "" {
		kind = "text"
	}
	return InboxRecord{
		ID: itemID(r.ID, r.MsgID), EventID: r.EventID, MsgID: r.MsgID, TS: float64(s.now().UnixMilli()) / 1000,
		From: "owner", Via: "telegram", Kind: kind, Host: s.Env.Host, Session: r.Session,
		Text: r.Text, Quote: r.Quote, ReplyTo: r.ReplyTo,
	}
}

// ---- type ----

type TypeRequest struct {
	Session string `json:"session"`
	MsgID   int64  `json:"msg_id"`
	Text    string `json:"text"`
	Quote   string `json:"quote,omitempty"`
	ReplyTo int64  `json:"reply_to,omitempty"`
	Kind    string `json:"kind,omitempty"`
	EventID string `json:"event_id,omitempty"`
	JobID   string `json:"-"` // set by the host from the job
	// ID is the inbox item id (default tg-<msg_id>); the typed line names it.
	ID string `json:"id,omitempty"`
	// BriefDir is the session's brief folder as the hub knows it (from the
	// agent's ping): used when the host can't find one, so a line for a session
	// that has ended still reaches its folder's inbox and successor.
	BriefDir string `json:"brief_dir,omitempty"`
}

type TypeResult struct {
	Status  string `json:"status"`
	Session string `json:"session,omitempty"` // the session typed into (a successor after a handover)
	State   State  `json:"state,omitempty"`
	Inbox   string `json:"inbox,omitempty"`
	Queued  bool   `json:"queued,omitempty"` // retried every 60 s for 30 min
	Detail  string `json:"detail,omitempty"`
}

// Type appends the full text to the session's inbox, then types the sanitised
// line if the composer is ready; otherwise it stays queued for retries.
func (s *Service) Type(ctx context.Context, r TypeRequest) TypeResult {
	if r.Session == "" {
		return TypeResult{Status: StatusError, Detail: "session is required"}
	}
	if r.EventID == "" {
		r.EventID = fmt.Sprintf("tg-%d", r.MsgID)
	}
	sessions, err := s.Ctl.Sessions(ctx)
	if err != nil {
		sessions = nil // still record the message; typing is decided below
	}
	sess, _ := s.canonical(sessions, r.Session)
	r.Session = sess.ID
	briefDir := s.briefFor(sess.ID, sess.Cwd)
	if briefDir == "" && r.BriefDir != "" {
		if real, verr := s.Env.ValidBriefDir(r.BriefDir); verr == nil {
			briefDir = real
			s.known.remember(sess.ID, real)
		}
	}
	path := s.Env.InboxPath(briefDir, r.Session)
	if _, ierr := AppendInbox(path, s.record(InboxAppendRequest{
		Session: r.Session, MsgID: r.MsgID, Kind: r.Kind, Text: r.Text, Quote: r.Quote, ReplyTo: r.ReplyTo, EventID: r.EventID, ID: r.ID,
	})); ierr != nil {
		return TypeResult{Status: StatusError, Inbox: path, Detail: "inbox: " + ierr.Error()}
	}
	res := TypeResult{Inbox: path}
	if err != nil {
		res.Status, res.Detail = StatusError, "agterm: "+err.Error()
		return res
	}
	line := sanitize.LineID(r.Text, itemID(r.ID, r.MsgID), r.Quote)
	live, ok := ResolveLive(sessions, r.Session, briefDir)
	if !ok {
		res.Status, res.Detail = StatusNoSession, "no live session; kept in the inbox"
		if briefDir != "" {
			// A successor of this folder may start: the retry loop types it there.
			a, err := s.Queue.Submit(ctx, Pending{EventID: r.EventID, JobID: r.JobID, MsgID: r.MsgID,
				Session: r.Session, BriefDir: briefDir, Inbox: path, ItemID: itemID(r.ID, r.MsgID), Line: line})
			if err != nil {
				res.Status, res.Detail = StatusError, err.Error()
				return res
			}
			if a.Outcome == Blocked || a.Outcome == Gone {
				res.Queued, res.Detail = true, "no live session; kept in the inbox and typed into a successor of the folder"
				return res
			}
			// a successor started meanwhile and the queue already typed it (or failed to)
			return attemptResult(res, a)
		}
		return res
	}
	// lines still queued for an ended session of this folder go first
	isLive := func(id string) bool { _, ok := FindSession(sessions, id); return ok }
	if err := s.Queue.Adopt(briefDir, live.ID, isLive); err != nil {
		res.Status, res.Detail = StatusError, err.Error()
		return res
	}
	a, err := s.Queue.Submit(ctx, Pending{
		EventID: r.EventID, JobID: r.JobID, MsgID: r.MsgID, Session: live.ID, BriefDir: briefDir,
		Inbox: path, ItemID: itemID(r.ID, r.MsgID), Line: line,
	})
	if err != nil {
		res.Status, res.Detail = StatusError, err.Error()
		return res
	}
	return attemptResult(res, a)
}

// attemptResult maps the queue's attempt at a line onto the job's result.
func attemptResult(res TypeResult, a Attempt) TypeResult {
	res.Session, res.State, res.Detail = a.Session, a.State.State, a.Detail
	switch a.Outcome {
	case Delivered:
		res.Status = StatusOK
	case Unconfirmed:
		res.Status = StatusOK
	case Blocked, Gone:
		res.Status, res.Queued = StatusInbox, true
	case Handled: // the agent took it from the inbox already
		res.Status = StatusInbox
	default:
		res.Status = StatusError
	}
	return res
}

// ---- peek ----

type PeekRequest struct {
	Session string `json:"session"`
	Lines   int    `json:"lines,omitempty"`
	Screen  bool   `json:"screen,omitempty"` // the screen as `agtermctl session text` shows it (composer kept)
}

type PeekResult struct {
	Status   string   `json:"status"`
	Session  string   `json:"session,omitempty"`
	Text     string   `json:"text,omitempty"`
	Composer Composer `json:"composer"`
	Error    string   `json:"error,omitempty"`
}

const maxPeekLines = 500

// Peek returns the last lines of the session's primary pane (blank tail rows
// between the transcript and the composer dropped).
func (s *Service) Peek(ctx context.Context, r PeekRequest) PeekResult {
	n := r.Lines
	if n <= 0 {
		n = 40
	}
	n = min(n, maxPeekLines)
	sessions, err := s.Ctl.Sessions(ctx)
	if err != nil {
		return PeekResult{Status: StatusError, Error: err.Error()}
	}
	sess, ok := FindSession(sessions, r.Session)
	if !ok {
		return PeekResult{Status: StatusNoSession, Error: "no such session"}
	}
	c, screen, err := Observe(ctx, s.Ctl, sess)
	if err != nil {
		return PeekResult{Status: StatusError, Session: sess.ID, Error: err.Error()}
	}
	if n > ScreenLines {
		if screen, err = s.Ctl.Text(ctx, sess, n+ScreenLines); err != nil {
			return PeekResult{Status: StatusError, Session: sess.ID, Error: err.Error()}
		}
	}
	lines := splitLines(screen)
	if r.Screen {
		for i := range lines {
			lines[i] = strings.TrimRight(lines[i], " ")
		}
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		return PeekResult{Status: StatusOK, Session: sess.ID, Text: strings.Join(lastN(lines, n), "\n"), Composer: c}
	}
	if c.Agent != "" {
		lines = aboveComposer(c.Agent, lines)
	}
	return PeekResult{Status: StatusOK, Session: sess.ID, Text: strings.Join(lastN(compactBlankRuns(lines), n), "\n"), Composer: c}
}

// aboveComposer drops the live composer and its footer (Claude: from the box's
// top rule; Codex: from the prompt row), leaving the transcript.
func aboveComposer(agent Agent, lines []string) []string {
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-BoxLines; i-- {
		if agent == Claude && claudePromptRE.MatchString(lines[i]) {
			if i > 0 && ruleRE.MatchString(lines[i-1]) {
				return lines[:i-1]
			}
			return lines
		}
		if agent == Codex && codexPromptRE.MatchString(lines[i]) {
			return lines[:i]
		}
	}
	return lines
}

// compactBlankRuns folds runs of blank rows into one.
func compactBlankRuns(lines []string) []string {
	var out []string
	blank := false
	for _, l := range lines {
		b := strings.TrimSpace(l) == ""
		if b && blank {
			continue
		}
		blank = b
		out = append(out, strings.TrimRight(l, " "))
	}
	return out
}

// ---- files / pause / done: brief-folder jobs ----

type BriefRequest struct {
	Session  string `json:"session,omitempty"`
	BriefDir string `json:"brief_dir,omitempty"`
	Path     string `json:"path,omitempty"` // files: relative to the brief folder; empty lists it
	Tail     int    `json:"tail,omitempty"` // files: last N lines
}

type FileEntry struct {
	Name  string    `json:"name"`
	Size  int64     `json:"size"`
	Dir   bool      `json:"dir,omitempty"`
	MTime time.Time `json:"mtime"`
}

type BriefResult struct {
	Status    string      `json:"status"`
	BriefDir  string      `json:"brief_dir,omitempty"`
	Path      string      `json:"path,omitempty"`
	Content   string      `json:"content,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
	Files     []FileEntry `json:"files,omitempty"`
	Error     string      `json:"error,omitempty"`
}

const maxFileBytes = 64 * 1024

func (s *Service) briefDir(ctx context.Context, r BriefRequest) (string, error) {
	if r.BriefDir != "" {
		return s.Env.ValidBriefDir(r.BriefDir)
	}
	if r.Session == "" {
		return "", errors.New("session or brief_dir is required")
	}
	sessions, _ := s.Ctl.Sessions(ctx)
	sess, _ := s.canonical(sessions, r.Session)
	dir := s.briefFor(sess.ID, sess.Cwd)
	if dir == "" {
		return "", fmt.Errorf("session %s has no brief folder", r.Session)
	}
	return s.Env.ValidBriefDir(dir)
}

// Files lists the brief folder or returns one file (whole, capped, or its tail).
// Paths must resolve inside the brief folder, symlinks included.
func (s *Service) Files(ctx context.Context, r BriefRequest) BriefResult {
	dir, err := s.briefDir(ctx, r)
	if err != nil {
		return BriefResult{Status: StatusError, Error: err.Error()}
	}
	res := BriefResult{Status: StatusOK, BriefDir: dir}
	if r.Path == "" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return BriefResult{Status: StatusError, BriefDir: dir, Error: err.Error()}
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			res.Files = append(res.Files, FileEntry{Name: e.Name(), Size: info.Size(), Dir: e.IsDir(), MTime: info.ModTime()})
		}
		return res
	}
	path, err := SafeJoin(dir, r.Path, true)
	if err != nil {
		return BriefResult{Status: StatusError, BriefDir: dir, Error: err.Error()}
	}
	res.Path = path
	content, truncated, err := readCapped(path, r.Tail)
	if err != nil {
		return BriefResult{Status: StatusError, BriefDir: dir, Path: path, Error: err.Error()}
	}
	res.Content, res.Truncated = content, truncated
	return res
}

func readCapped(path string, tail int) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	if st.IsDir() {
		return "", false, fmt.Errorf("%s is a directory", filepath.Base(path))
	}
	if tail > 0 {
		var lines []string
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			lines = append(lines, sc.Text())
			if len(lines) > tail {
				lines = lines[1:]
			}
		}
		out := strings.Join(lines, "\n")
		if len(out) > maxFileBytes {
			return out[len(out)-maxFileBytes:], true, sc.Err()
		}
		return out, false, sc.Err()
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return "", false, err
	}
	if len(data) > maxFileBytes {
		return string(data[:maxFileBytes]), true, nil
	}
	return string(data), false, nil
}

// Pause writes PAUSE in the brief folder.
func (s *Service) Pause(ctx context.Context, r BriefRequest) BriefResult {
	dir, err := s.briefDir(ctx, r)
	if err != nil {
		return BriefResult{Status: StatusError, Error: err.Error()}
	}
	path := filepath.Join(dir, "PAUSE")
	body := fmt.Sprintf("paused from Telegram at %s\n", s.now().UTC().Format(time.RFC3339))
	if err := WriteFileAtomic(path, []byte(body), 0o644); err != nil {
		return BriefResult{Status: StatusError, BriefDir: dir, Error: err.Error()}
	}
	return BriefResult{Status: StatusOK, BriefDir: dir, Path: path}
}

// Done marks the brief folder's state.json done.
func (s *Service) Done(ctx context.Context, r BriefRequest) BriefResult {
	dir, err := s.briefDir(ctx, r)
	if err != nil {
		return BriefResult{Status: StatusError, Error: err.Error()}
	}
	if err := UpdateState(dir, map[string]any{
		"status": "done", "done_at": s.now().UTC().Format(time.RFC3339), "done_by": "telegram",
	}, []string{"status", "done_at", "done_by"}); err != nil {
		return BriefResult{Status: StatusError, BriefDir: dir, Error: err.Error()}
	}
	return BriefResult{Status: StatusOK, BriefDir: dir, Path: filepath.Join(dir, "state.json")}
}
