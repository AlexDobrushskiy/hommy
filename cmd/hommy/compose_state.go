package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
)

// runComposeState implements `hommy compose-state --target ID [--json]`.
// Exit codes: 0 ready, 1 not ready (draft/menu/working/unknown), 2 error.
func runComposeState(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("compose-state", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", os.Getenv("AGTERM_SESSION_ID"), "agterm session id or unique prefix")
	asJSON := fs.Bool("json", false, "print the reading as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *target == "" {
		_, _ = fmt.Fprintln(stderr, "compose-state: --target is required")
		return 2
	}
	ctl, err := agterm.NewExecCtl()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "compose-state:", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sessions, err := ctl.Sessions(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "compose-state:", err)
		return 2
	}
	s, ok := agterm.FindSession(sessions, *target)
	if !ok {
		_, _ = fmt.Fprintf(stderr, "compose-state: no such session: %s\n", *target)
		return 2
	}
	c, _, err := agterm.Observe(ctx, ctl, s)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "compose-state:", err)
		return 2
	}
	if *asJSON {
		out := struct {
			Session string `json:"session"`
			Name    string `json:"name"`
			agterm.Composer
		}{s.ID, s.Name, c}
		b, _ := json.Marshal(out)
		_, _ = fmt.Fprintln(stdout, string(b))
	} else {
		_, _ = fmt.Fprintln(stdout, c.State)
	}
	if c.State == agterm.Ready {
		return 0
	}
	return 1
}
