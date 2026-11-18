// Command hommy-e2e runs the test fakes (docs/testing.md): faketg, fakespeech, fakeclaude (also when called as
// claude-stub or claude) and static-site.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/e2e/fakeclaude"
	"github.com/AlexDobrushskiy/hommy/internal/e2e/fakespeech"
	"github.com/AlexDobrushskiy/hommy/internal/e2e/faketg"
	"github.com/AlexDobrushskiy/hommy/internal/e2e/staticsite"
)

const usageText = `usage:
  hommy-e2e faketg --addr HOST:PORT (--token-file F | --token T) [--state-file F]
  hommy-e2e fakespeech --addr HOST:PORT [--fixtures DIR]
  hommy-e2e static-site --addr HOST:PORT [--dir DIR]
  hommy-e2e fakeclaude [claude args…]   (also as claude-stub / claude)`

func main() {
	os.Exit(run(os.Args, os.Stdin, os.Stdout, os.Stderr))
}

func run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch filepath.Base(argv[0]) {
	case "claude-stub", "claude":
		return stub(argv[1:], stdin, stdout, stderr)
	}
	if len(argv) < 2 {
		fmt.Fprintln(stderr, usageText)
		return 2
	}
	cmd, args := argv[1], argv[2:]
	switch cmd {
	case "fakeclaude":
		return stub(args, stdin, stdout, stderr)
	case "faketg", "fakespeech", "static-site":
		h, addr, err := handler(cmd, args, stderr)
		if err != nil {
			if !errors.Is(err, flag.ErrHelp) {
				fmt.Fprintf(stderr, "hommy-e2e %s: %v\n", cmd, err)
			}
			return 2
		}
		if err := serve(addr, h, cmd, stderr); err != nil {
			fmt.Fprintf(stderr, "hommy-e2e %s: %v\n", cmd, err)
			return 1
		}
		return 0
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, usageText)
		return 0
	}
	fmt.Fprintln(stderr, usageText)
	return 2
}

func stub(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return fakeclaude.Run(fakeclaude.Env{Args: args, Stdin: stdin, Stdout: stdout, Stderr: stderr, Self: fakeclaude.LookSelf()})
}

func handler(cmd string, args []string, stderr io.Writer) (http.Handler, string, error) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "", "listen address, e.g. 127.0.0.1:4711")
	token := fs.String("token", "", "faketg: the bot token to accept")
	tokenFile := fs.String("token-file", "", "faketg: a file holding the bot token")
	stateFile := fs.String("state-file", "", "faketg: keeps the id counters across restarts")
	fixtures := fs.String("fixtures", "e2e/fixtures/audio", "fakespeech: fixture audio with .txt transcripts")
	dir := fs.String("dir", "e2e/fixtures/static", "static-site: the directory to serve")
	if err := fs.Parse(args); err != nil {
		return nil, "", err
	}
	if *addr == "" {
		return nil, "", errors.New("--addr is required")
	}
	switch cmd {
	case "faketg":
		tok := *token
		if *tokenFile != "" {
			b, err := os.ReadFile(*tokenFile)
			if err != nil {
				return nil, "", err
			}
			tok = strings.TrimSpace(string(b))
		}
		if tok == "" {
			return nil, "", errors.New("a token is required (--token-file or --token)")
		}
		return faketg.New(faketg.Options{Token: tok, StateFile: *stateFile}), *addr, nil
	case "fakespeech":
		s, err := fakespeech.New(fakespeech.Options{FixturesDir: *fixtures})
		if err != nil {
			return nil, "", err
		}
		fmt.Fprintf(stderr, "fakespeech: %d fixture transcripts from %s\n", s.Fixtures(), *fixtures)
		return s, *addr, nil
	default:
		h, err := staticsite.Handler(*dir)
		return h, *addr, err
	}
}

// serve listens until SIGTERM/SIGINT, then shuts down.
func serve(addr string, h http.Handler, name string, stderr io.Writer) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	fmt.Fprintf(stderr, "%s: listening on http://%s\n", name, ln.Addr())
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return nil
}
