package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/pairing"
	"github.com/AlexDobrushskiy/hommy/internal/secret"
)

// hubClient is the hub's HTTP API with this Mac's host key.
func hubClient(c *config.Config) (pairing.Client, error) {
	base, err := pairing.BaseURL(c.Get("HUB_URL"))
	if err != nil {
		return pairing.Client{}, err
	}
	key, err := store(c).Get(secret.HostKey)
	if err != nil {
		return pairing.Client{}, fmt.Errorf("host-key: %v (run hommy setup)", err)
	}
	return pairing.Client{BaseURL: base, HostName: c.Get("HOST_NAME"), Key: key}, nil
}

func runPair(c *config.Config, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reset := fs.Bool("reset", false, "forget the owner (and a pending code), then make a new code")
	ownerID := fs.Int64("owner-id", 0, "bind this Telegram user id directly (no code)")
	status := fs.Bool("status", false, "print paired | pending | not paired")
	noWait := fs.Bool("no-wait", false, "print the code and return (the code stays valid)")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		return 2
	}
	cl, err := hubClient(c)
	if err != nil {
		fmt.Fprintln(stderr, "pair:", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return pair(ctx, c, cl, pairOpts{reset: *reset, ownerID: *ownerID, status: *status, noWait: *noWait}, stdout, stderr)
}

type pairOpts struct {
	reset   bool
	ownerID int64
	status  bool
	noWait  bool
}

// pair is `hommy pair` (setup's step 10 calls it too).
func pair(ctx context.Context, c *config.Config, cl pairing.Client, o pairOpts, stdout, stderr io.Writer) int {
	rctx, rcancel := context.WithTimeout(ctx, 15*time.Second)
	defer rcancel()
	switch {
	case o.status:
		st, err := cl.Status(rctx)
		if err != nil {
			fmt.Fprintln(stderr, "pair:", err)
			return 1
		}
		switch {
		case st.Paired:
			fmt.Fprintln(stdout, "paired")
			saveOwner(c, st, stderr)
		case st.Pending:
			fmt.Fprintf(stdout, "pending (code expires %s)\n", st.ExpiresAt.Local().Format(time.DateTime))
			return 1
		default:
			fmt.Fprintln(stdout, "not paired")
			return 1
		}
		return 0
	case o.ownerID != 0:
		st, err := cl.Bind(rctx, o.ownerID)
		if err != nil {
			fmt.Fprintln(stderr, "pair:", err)
			return 1
		}
		fmt.Fprintf(stdout, "paired with Telegram user %d\n", o.ownerID)
		saveOwner(c, st, stderr)
		return 0
	}
	if o.reset {
		if _, err := cl.Reset(rctx); err != nil {
			fmt.Fprintln(stderr, "pair:", err)
			return 1
		}
		if err := c.Unset("OWNER_CHAT_ID"); err != nil {
			fmt.Fprintln(stderr, "config:", err)
		}
		fmt.Fprintln(stdout, "owner forgotten")
	} else if st, err := cl.Status(rctx); err == nil && st.Paired {
		fmt.Fprintln(stdout, "already paired (hommy pair --reset to pair another account)")
		saveOwner(c, st, stderr)
		return 0
	}
	code, err := cl.New(rctx)
	if err != nil {
		fmt.Fprintln(stderr, "pair:", err)
		return 1
	}
	fmt.Fprintf(stdout, "code: %s\n", code.Code)
	fmt.Fprintf(stdout, "In Telegram, open %s and send: /start %s\n", code.Bot, code.Code)
	fmt.Fprintf(stdout, "or open %s (the code expires %s)\n", code.Link, code.ExpiresAt.Local().Format(time.DateTime))
	if o.noWait {
		return 0
	}
	fmt.Fprintln(stdout, "waiting for your /start … (Ctrl-C leaves the code valid; hommy pair --status checks later)")
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(stdout, "\nstill pending")
			return 1
		case <-tick.C:
		}
		qctx, qcancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := cl.Status(qctx)
		qcancel()
		if err != nil {
			continue
		}
		if st.Paired {
			fmt.Fprintln(stdout, "✅ paired")
			saveOwner(c, st, stderr)
			return 0
		}
		if !st.Pending {
			fmt.Fprintln(stdout, "the code expired: run hommy pair again")
			return 1
		}
	}
}

// saveOwner writes OWNER_CHAT_ID (the `send` direct fallback) when it changed.
func saveOwner(c *config.Config, st pairing.State, stderr io.Writer) {
	if st.OwnerChatID == 0 {
		return
	}
	v := strconv.FormatInt(st.OwnerChatID, 10)
	if cur, _ := c.FileValue("OWNER_CHAT_ID"); cur == v {
		return
	}
	if err := c.Set("OWNER_CHAT_ID", v); err != nil {
		fmt.Fprintln(stderr, "config:", err)
	}
}
