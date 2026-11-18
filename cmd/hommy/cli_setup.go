package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/doctor"
	"github.com/AlexDobrushskiy/hommy/internal/setup"
)

func probes(c *config.Config) doctor.Probes {
	m := newManager(c, io.Discard)
	return doctor.RealProbes(c,
		func(name string) (bool, bool) {
			st, err := m.Status(name)
			if err != nil || len(st) != 1 {
				return false, false
			}
			return st[0].Loaded, st[0].Running
		},
		func(name string) bool { _, err := os.Stat(c.Inst.PlistPath(name)); return err == nil })
}

func runDoctor(c *config.Config, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	deep := fs.Bool("deep", false, "add live speech round trips (transcribe a sample, synthesize ok)")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		return 2
	}
	d := doctor.Doctor{Cfg: c, P: probes(c), Deep: *deep}
	if doctor.Print(stdout, d.Run(context.Background())) {
		return 1
	}
	return 0
}

func runSetup(c *config.Config, g globals, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var o setup.Options
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&o.NonInteractive, "non-interactive", false, "never prompt; a missing required value exits 2")
	fs.StringVar(&o.PortBase, "port-base", "", "first of the instance's ports (default 8470)")
	fs.StringVar(&o.HostName, "host-name", "", "this Mac's host name (default: its LocalHostName)")
	fs.BoolVar(&o.TokenStdin, "token-stdin", false, "read the bot token from stdin")
	fs.StringVar(&o.TelegramAPI, "telegram-api", "", "Bot API base URL (default https://api.telegram.org)")
	fs.StringVar(&o.Vault, "vault", "", "the vault folder (default ~/hommy-vault)")
	fs.StringVar(&o.Permissions, "permissions", "", "skip (default) or ask")
	fs.StringVar(&o.Speech, "speech", "", "local (default), external or off")
	fs.StringVar(&o.STTURL, "stt-url", "", "speech-to-text base URL (--speech external)")
	fs.StringVar(&o.TTSURL, "tts-url", "", "text-to-speech base URL (--speech external)")
	fs.StringVar(&o.WhisperModel, "whisper-model", "", "large-v3-turbo (default), small or medium")
	fs.StringVar(&o.ClaudeBin, "claude-bin", "", "path of claude (default: found on PATH)")
	fs.BoolVar(&o.NoPairWait, "no-pair-wait", false, "skip pairing (hommy pair later)")
	fs.StringVar(&o.PluginSource, "plugin-source", "", "the hommy plugin marketplace: a local path or owner/repo (default "+setup.DefaultPluginSource+")")
	fs.BoolVar(&o.SeedContext, "seed-context", false, "open an agterm session that runs the retainer interview into the vault")
	fs.BoolVar(&o.DryRun, "dry-run", false, "with --seed-context: print the session command and first message only")
	fs.StringVar(&o.AgentCommand, "agent-command", "", "what agent sessions run (default: CLAUDE_BIN)")
	fs.StringVar(&o.AgtermWorkspace, "agterm-workspace", "", "the agterm workspace of agent sessions (default hommy)")
	fs.StringVar(&o.Join, "join", "", "host-only install joining the hub at this ws:// URL")
	fs.StringVar(&o.Name, "name", "", "this host's name (--join)")
	fs.BoolVar(&o.KeyStdin, "key-stdin", false, "read the host key from stdin (--join)")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		return 2
	}
	for _, v := range []struct{ name, val string }{{"--permissions", o.Permissions}, {"--speech", o.Speech}} {
		ok := map[string][]string{"--permissions": {"", "skip", "ask"}, "--speech": {"", "local", "external", "off"}}[v.name]
		found := false
		for _, x := range ok {
			found = found || x == v.val
		}
		if !found {
			fmt.Fprintf(stderr, "%s %q: not a choice\n", v.name, v.val)
			return 2
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	m := newManager(c, stdout)
	e := setup.Env{
		Cfg: c, Store: store(c), In: stdin, Out: stdout, Probes: probes(c),
		Install: func(restart bool, names ...string) (int, error) {
			before := m.Changes
			err := m.InstallRestart(restart, names...)
			return m.Changes - before, err
		},
		Exec:          execCmd,
		PrepareSpeech: func(ctx context.Context) (int, error) { return setup.LocalSpeech(ctx, c, stdout) },
		Warm:          func(ctx context.Context) error { return setup.Warm(ctx, c, stdout) },
		NewSession: func(ctx context.Context, so agterm.NewSessionOpts) (string, error) {
			ctl, err := agterm.NewExecCtl()
			if err != nil {
				return "", err
			}
			return ctl.NewSession(ctx, so)
		},
		Pair: func(ctx context.Context) error {
			cl, err := hubClient(c)
			if err != nil {
				return err
			}
			if pair(ctx, c, cl, pairOpts{}, stdout, stderr) != 0 {
				return errors.New("not paired (hommy pair resumes)")
			}
			return nil
		},
		Ready: func(ctx context.Context, hubRole bool) error { return waitReady(ctx, c, hubRole) },
		Doctor: func(ctx context.Context) []doctor.Result {
			d := doctor.Doctor{Cfg: c, P: probes(c)}
			return d.Run(ctx)
		},
	}
	if err := setup.Run(ctx, e, o); err != nil {
		fmt.Fprintln(stderr, "setup:", err)
		if errors.Is(err, setup.ErrUsage) {
			return 2
		}
		return 1
	}
	return 0
}

// waitReady waits (≤ 30 s) for the hub's /healthz and this host's first heartbeat.
func waitReady(ctx context.Context, c *config.Config, hubRole bool) error {
	cl, err := hubClient(c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	name := c.Get("HOST_NAME")
	for {
		qctx, qcancel := context.WithTimeout(ctx, 3*time.Second)
		st, err := cl.APIStatus(qctx)
		qcancel()
		if err == nil {
			for _, h := range st.Hosts {
				if h.Name == name && h.Online && h.Checks != nil {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("host %s not online yet", name)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// execCmd runs a setup helper command (≤ 15 min: a plugin install clones from GitHub, the browsers' install.sh runs
// npm and its smoke test) and returns its combined output.
func execCmd(ctx context.Context, stdin, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
