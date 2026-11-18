// Command hommy is the Telegram-to-Claude-Code bridge: a hub that talks to the
// Telegram Bot API and hosts that run Claude Code sessions in agterm.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/AlexDobrushskiy/hommy/internal/agentcli"
	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/host"
	"github.com/AlexDobrushskiy/hommy/internal/hub"
	"github.com/AlexDobrushskiy/hommy/internal/version"
)

const usage = `usage: hommy [--instance NAME] [--config FILE] <command> [args]

  setup [flags]                      install, configure and pair (hommy setup --help)
  pair [--reset] [--owner-id N] [--status] [--no-wait]
  doctor [--deep]                    check every piece; a fix line under each failure
  config get|set|unset|list|path [KEY] [VALUE]
  config permissions ask|skip        agent sessions ask before tools, or skip the prompts
  service install|uninstall [--purge]|start|stop|restart|status [hub|host|speech|all]
  hub host-key add NAME              a key for another Mac's host (hommy setup --join)
  hub | host                         the daemons (launchd runs them)
  send [--agent SLUG …] TEXT         message the owner (agent sessions)
  inbox list|ack|pending|wait        replies to an agent session
  hook session-start|user-prompt-submit|stop|notify
  compose-state --target ID [--json] | agterm-job TYPE
  version

Exit codes: 0 ok, 1 failed check / not ready, 2 usage, 3 refused.`

// globals are the flags every subcommand takes.
type globals struct {
	instance string
	config   string
}

// exit and getenv are seams for tests.
var (
	exit   = os.Exit
	getenv = os.Getenv
)

func stringsReader(s string) io.Reader { return strings.NewReader(s) }

// splitGlobals takes --instance/--config (either form, anywhere before `--`)
// out of args.
func splitGlobals(args []string) (globals, []string, error) {
	var g globals
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		for _, f := range []struct {
			name string
			dst  *string
		}{{"--instance", &g.instance}, {"--config", &g.config}} {
			if a == f.name {
				if i+1 >= len(args) {
					return g, nil, fmt.Errorf("%s needs a value", f.name)
				}
				*f.dst = args[i+1]
				i++
				a = ""
				break
			}
			if v, ok := strings.CutPrefix(a, f.name+"="); ok {
				*f.dst = v
				a = ""
				break
			}
		}
		if a != "" {
			rest = append(rest, a)
		}
	}
	return g, rest, nil
}

// load is the instance's config (not exported to the env).
func (g globals) load() (*config.Config, error) {
	inst, err := config.FromEnv(g.instance)
	if err != nil {
		return nil, err
	}
	return config.Load(inst, g.config)
}

func main() {
	exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	g, args, err := splitGlobals(argv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version":
		fmt.Fprintf(stdout, "hommy %s\n", version.String())
		return 0
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, usage)
		return 0
	case "hook":
		return runHook(g, rest, stdin)
	case "hub", "host", "config", "pair", "service", "doctor", "setup", "compose-state", "send", "inbox", "agterm-job":
	default:
		fmt.Fprintln(stderr, usage)
		return 2
	}
	c, err := g.load()
	if err != nil {
		fmt.Fprintln(stderr, "hommy:", err)
		return 2
	}
	switch cmd {
	case "hub":
		if len(rest) > 0 && rest[0] == "host-key" {
			return runHostKey(c, rest[1:], stdout, stderr)
		}
		return daemon(func(ctx context.Context) error { return hub.Main(ctx, c) })
	case "host":
		return daemon(func(ctx context.Context) error { return host.Main(ctx, c) })
	case "config":
		return runConfig(c, rest, stdout, stderr)
	case "pair":
		return runPair(c, rest, stdout, stderr)
	case "service":
		return runService(c, rest, stdout, stderr)
	case "doctor":
		return runDoctor(c, rest, stdout, stderr)
	case "setup":
		return runSetup(c, g, rest, stdin, stdout, stderr)
	}
	// Agent-session commands read the instance's settings through the env.
	if err := c.Export(); err != nil {
		fmt.Fprintln(stderr, "hommy: config:", err)
		return 2
	}
	agentcli.Instance = func() (agentcli.Config, error) { return sendConfig(c) }
	switch cmd {
	case "compose-state":
		return runComposeState(rest, stdout, stderr)
	case "send":
		return agentcli.Send(rest, stdin, stdout, stderr)
	case "inbox":
		return agentcli.Inbox(rest, stdout, stderr)
	case "agterm-job":
		return runAgtermJob(rest, stdin, stdout, stderr)
	}
	fmt.Fprintln(stderr, usage)
	return 2
}

// daemon runs a daemon until SIGINT/SIGTERM; an error exits 1.
func daemon(fn func(context.Context) error) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := fn(ctx); err != nil {
		log.Print(err)
		return 1
	}
	return 0
}
