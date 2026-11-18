package agentcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/ping"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// SendUsage is send.py's doc plus the agent flags.
const SendUsage = `Send the owner a Telegram message via the hommy bot.

Usage: hommy send "<text>"            text as an argument
       hommy send - < file             text from stdin
       hommy send --silent "<text>"    no notification sound
       hommy send -- "--text"          "--" ends the options (text starting with a dash)
       hommy send --agent SLUG [--dir DIR] [--buttons "A|B|Answer…"] [--ask] [--json] "<text>"

-h / --help prints this (exit 0). Any other unknown -x / --xyz is a usage error
(exit 2) and sends nothing.

Config: the instance (--instance; its config file and Keychain): through the
hub, which records the message so the owner's replies, reactions and buttons
reach the agent's session; the Bot API directly when the hub is down. Or an env
file (--env-file / $HOMMY_SEND_ENV: TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_ID, HUB_URL…).
Text is sent verbatim (newlines kept), split at Telegram's 4096-char limit. Exits
non-zero on failure; never prints the token.`

type sendArgs struct {
	text, envFile, agent, dir, buttons string
	silent, ask, json                  bool
}

// errHelp: -h / --help asked for the usage.
var errHelp = errors.New("help")

// usageError is an unknown option: exit 2, nothing sent.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// flagHead: a dash or two, then a letter.
var flagHead = regexp.MustCompile(`^--?[A-Za-z]`)

// flagLike: flagHead, then no whitespace ("-h", "--xyz", "--xyz=1"). "-"
// (stdin), "- item", "-5" and "-- not a flag" are text. Whitespace is Python's
// (send.py's \S), so both refuse the same arguments.
func flagLike(arg string) bool {
	return flagHead.MatchString(arg) && !strings.ContainsFunc(arg, pySpace)
}

// parseSendArgs takes flags anywhere (send.py did); the first other argument is
// the text. A flag-shaped argument that isn't one of ours is a usage error, never
// text; "--" ends the options, so a message may still start with dashes.
func parseSendArgs(args []string) (sendArgs, bool, error) {
	var a sendArgs
	var pos []string
	var unknown error
	help := false
	takes := map[string]*string{"--env-file": &a.envFile, "--agent": &a.agent, "--dir": &a.dir, "--buttons": &a.buttons}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		name, val, hasVal := strings.Cut(arg, "=")
		if dst, ok := takes[name]; ok {
			if !hasVal {
				if i+1 >= len(args) {
					return a, false, fmt.Errorf("%s needs a value", name)
				}
				i++
				val = args[i]
			}
			*dst = val
			continue
		}
		switch arg {
		case "--silent":
			a.silent = true
		case "--ask":
			a.ask = true
		case "--json":
			a.json = true
		case "-h", "--help":
			help = true
		default:
			if !flagLike(arg) {
				pos = append(pos, arg)
			} else if unknown == nil {
				unknown = usageError{fmt.Sprintf("unknown option %s (to send text starting with a dash: send -- %q)", name, arg)}
			}
		}
	}
	switch {
	case help:
		return a, false, errHelp
	case unknown != nil:
		return a, false, unknown
	case len(pos) == 0:
		return a, false, nil
	}
	a.text = pos[0]
	return a, true, nil
}

// pyStrip is Python's str.strip(): Unicode whitespace plus the \x1c–\x1f separators.
func pyStrip(s string) string {
	return strings.TrimFunc(s, pySpace)
}

// pySpace is Python's str.isspace() for one character.
func pySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// sendResult is the --json / --agent output.
type sendResult struct {
	OK         bool   `json:"ok"`
	Via        string `json:"via"` // hub | direct
	ChatID     int64  `json:"chat_id"`
	MessageIDs []int  `json:"message_ids"`
	Spooled    bool   `json:"spooled,omitempty"`
	Warning    string `json:"warning,omitempty"`
}

// sender is Send's surroundings (tests swap them).
type sender struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	env    func(string) string
	cwd    func() (string, error)
	sleep  func(time.Duration)
	now    func() time.Time
	briefs agterm.Env
}

// Send is `hommy send`; exit codes as send.py (0 ok, 1 anything else).
func Send(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	env, _ := agterm.DefaultEnv("", "")
	return sender{stdin: stdin, stdout: stdout, stderr: stderr, env: os.Getenv, cwd: os.Getwd,
		sleep: time.Sleep, now: time.Now, briefs: env}.run(context.Background(), args)
}

func (s sender) fail(format string, a ...any) int {
	fmt.Fprintf(s.stderr, "error: "+format+"\n", a...)
	return 1
}

func (s sender) run(ctx context.Context, args []string) int {
	a, ok, err := parseSendArgs(args)
	var ue usageError
	switch {
	case errors.Is(err, errHelp):
		_, _ = fmt.Fprintln(s.stdout, SendUsage)
		return 0
	case errors.As(err, &ue):
		_, _ = fmt.Fprintf(s.stderr, "error: %v\n\n%s\n", err, SendUsage)
		return 2
	case err != nil:
		return s.fail("%v", err)
	}
	if !ok {
		fmt.Fprintln(s.stderr, SendUsage)
		return 1
	}
	text := a.text
	if text == "-" {
		b, err := io.ReadAll(s.stdin)
		if err != nil {
			return s.fail("stdin: %v", err)
		}
		text = string(b)
	}
	if !utf8.ValidString(text) {
		return s.fail("the message is not valid UTF-8") // send.py failed here too (before any send)
	}
	text = pyStrip(text)
	if text == "" {
		return s.fail("empty message")
	}
	cfg, err := LoadConfig(ConfigPath(a.envFile))
	if err != nil {
		return s.fail("%v", err)
	}
	req := proto.SendRequest{ID: newID(), Text: text, Silent: a.silent, Ask: a.ask}
	if req.Buttons, err = ping.ParseButtons(a.buttons); err != nil {
		return s.fail("%v", err)
	}
	if a.agent != "" {
		if req.Agent, err = s.agentOf(cfg, a); err != nil {
			return s.fail("%v", err)
		}
	} else if len(req.Buttons) > 0 || a.ask {
		return s.fail("--buttons and --ask need --agent")
	} else if cfg.Hub() {
		// a plain message from an agent session still names that session, so a
		// reply to it reaches the session (not the chat's current target)
		if req.Agent = s.sessionOf(cfg); req.Agent != nil {
			req.Plain = true
		}
	}
	jsonOut := a.json || a.agent != ""

	res := sendResult{OK: true, ChatID: cfg.ChatID}
	if cfg.Hub() {
		var hr proto.SendResponse
		err := hubRetry(ctx, s.sleep, func(ctx context.Context) error {
			return hubPost(ctx, cfg, "/api/send", req, &hr)
		})
		switch {
		case err == nil:
			res.Via, res.MessageIDs = "hub", hr.MessageIDs
			if hr.Warning != "" {
				res.Warning = hr.Warning
				fmt.Fprintf(s.stderr, "hommy send: %s\n", hr.Warning)
			}
			return s.print(res, jsonOut)
		case !errors.Is(err, errUnreachable): // the hub got it: never sent twice
			return s.fail("%v", err)
		}
		fmt.Fprintf(s.stderr, "hommy send: %v; sending directly\n", err)
	} else if req.Agent != nil {
		res.Warning = "no hub in " + cfg.Path + ": sent directly; the owner's replies, reactions and buttons can't reach this session " +
			"(--env-file / $HOMMY_SEND_ENV = a hommy host env file)"
		fmt.Fprintf(s.stderr, "hommy send: %s\n", res.Warning)
	}
	return s.direct(ctx, cfg, req, res, jsonOut)
}

// agentOf fills in who is sending: the agterm session from the env, its brief
// folder, and the Claude session link.
func (s sender) agentOf(cfg Config, a sendArgs) (*proto.Agent, error) {
	if !ping.SlugRe.MatchString(a.agent) {
		return nil, fmt.Errorf("--agent %q: use letters, digits, . _ -", a.agent)
	}
	ag := &proto.Agent{Slug: a.agent, Host: cfg.hostName(), Session: s.env("AGTERM_SESSION_ID")}
	if a.dir != "" {
		dir, err := s.briefs.ValidBriefDir(a.dir)
		if err != nil {
			return nil, fmt.Errorf("--dir: %v", err)
		}
		ag.BriefDir = dir
	} else {
		cwd, _ := s.cwd()
		if dir := s.briefs.BriefDirFor(ag.Session, cwd); dir != "" {
			if real, err := s.briefs.ValidBriefDir(dir); err == nil {
				ag.BriefDir = real
			}
		}
	}
	if ag.Session == "" && ag.BriefDir != "" {
		ag.Session = agterm.ActiveAgent(ag.BriefDir)
	}
	if ag.Session == "" {
		fmt.Fprintln(s.stderr, "hommy send: no agterm session (AGTERM_SESSION_ID unset, no active_agent in the brief folder): the owner's replies can't be routed back")
	}
	if id := s.env("CLAUDE_CODE_BRIDGE_SESSION_ID"); id != "" && !strings.ContainsAny(id, " /?#") {
		ag.Link = ping.LinkPrefix + id
	}
	return ag, nil
}

// sessionOf is the agterm session a plain send runs in (nil outside one):
// $AGTERM_SESSION_ID, else the brief folder's active_agent. The slug names
// the folder (date prefix dropped) or the cwd.
func (s sender) sessionOf(cfg Config) *proto.Agent {
	ag := &proto.Agent{Host: cfg.hostName(), Session: s.env("AGTERM_SESSION_ID")}
	cwd, _ := s.cwd()
	if dir := s.briefs.BriefDirFor(ag.Session, cwd); dir != "" {
		if real, err := s.briefs.ValidBriefDir(dir); err == nil {
			ag.BriefDir = real
		}
	}
	if ag.Session == "" && ag.BriefDir != "" {
		ag.Session = agterm.ActiveAgent(ag.BriefDir)
	}
	if ag.Session == "" {
		return nil
	}
	name := filepath.Base(cwd)
	if ag.BriefDir != "" {
		name = datePrefix.ReplaceAllString(filepath.Base(ag.BriefDir), "")
	}
	ag.Slug = slugOf(name)
	return ag
}

var (
	datePrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-`)
	slugBad    = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// slugOf makes name a valid agent slug ("agent" when nothing is left).
func slugOf(name string) string {
	s := strings.TrimLeft(slugBad.ReplaceAllString(name, "-"), "._-")
	if len(s) > 64 {
		s = s[:64]
	}
	if !ping.SlugRe.MatchString(s) {
		return "agent"
	}
	return s
}

// direct sends straight to the Bot API (no hub configured, or it is down);
// with a hub configured, a spool entry lets the host tell the hub later.
func (s sender) direct(ctx context.Context, cfg Config, req proto.SendRequest, res sendResult, jsonOut bool) int {
	if cfg.Token == "" {
		return s.fail("the hub is not reachable and this Mac has no bot token (a joined host sends through the hub only)")
	}
	text, limit := req.Text, ping.PlainLimit
	buttons := req.Buttons
	if a := req.Agent; a != nil && !req.Plain {
		text, limit = ping.Text(a.Slug, a.Host, a.Link, text), ping.AgentLimit
		if req.Ask {
			buttons = ping.WithAnswer(buttons)
		}
	}
	parts := ping.Chunk(text, limit)
	res.Via = "direct"
	var sentTexts []string
	for i, part := range parts {
		o := directOpts{silent: req.Silent}
		last := i == len(parts)-1
		id, err := botSend(ctx, cfg, part, o)
		if err != nil {
			s.spoolSent(cfg, req, res.MessageIDs, sentTexts, nil)
			return s.fail("%v", err)
		}
		res.MessageIDs = append(res.MessageIDs, id)
		sentTexts = append(sentTexts, part)
		if !jsonOut {
			fmt.Fprintf(s.stdout, "sent message_id=%d\n", id)
		}
		if last && len(buttons) > 0 {
			if err := botButtons(ctx, cfg, id, buttons); err != nil {
				res.Warning = strings.TrimPrefix(res.Warning+"; buttons not attached: "+err.Error(), "; ")
				buttons = nil
			}
		}
	}
	res.Spooled = s.spoolSent(cfg, req, res.MessageIDs, sentTexts, buttons)
	if jsonOut {
		return s.print(res, true)
	}
	return 0
}

// spoolSent leaves the entry for the hub (only when one is configured).
func (s sender) spoolSent(cfg Config, req proto.SendRequest, ids []int, texts, buttons []string) bool {
	if !cfg.Hub() || len(ids) == 0 {
		return false
	}
	e := proto.SpoolEntry{ID: req.ID, Kind: proto.SpoolSent, Hub: cfg.HubURL, Bot: cfg.BotID(), ChatID: cfg.ChatID, Created: s.now().UTC(),
		MessageIDs: ids, Texts: texts, Agent: req.Agent, Buttons: buttons}
	if err := writeSpool(cfg, e); err != nil {
		fmt.Fprintf(s.stderr, "hommy send: spool: %v (replies to this message route by its header only)\n", err)
		return false
	}
	return true
}

func (s sender) print(res sendResult, jsonOut bool) int {
	if jsonOut {
		b, _ := json.Marshal(res)
		fmt.Fprintln(s.stdout, string(b))
		return 0
	}
	for _, id := range res.MessageIDs {
		fmt.Fprintf(s.stdout, "sent message_id=%d\n", id)
	}
	return 0
}
