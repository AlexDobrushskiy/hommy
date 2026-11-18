package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/agentcli"
	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/secret"
)

const configUsage = `usage: hommy config path
       hommy config list
       hommy config get KEY
       hommy config set KEY VALUE      validates, writes, restarts the services that read KEY
       hommy config unset KEY
       hommy config permissions ask|skip`

func runConfig(c *config.Config, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, configUsage)
		return 2
	}
	need := map[string]int{"path": 0, "list": 0, "get": 1, "set": 2, "unset": 1, "permissions": 1}
	n, ok := need[args[0]]
	if !ok || len(args)-1 != n {
		fmt.Fprintln(stderr, configUsage)
		return 2
	}
	switch args[0] {
	case "path":
		fmt.Fprintln(stdout, c.Path)
	case "list":
		for _, kv := range c.List() {
			fmt.Fprintf(stdout, "%s=%s\t# %s\n", kv[0], kv[1], kv[2])
		}
		for _, k := range c.Unknown() {
			fmt.Fprintf(stdout, "# unknown setting %s (ignored)\n", k)
		}
	case "get":
		if !config.Known(args[1]) {
			if v, ok := c.FileValue(args[1]); ok {
				fmt.Fprintln(stdout, v)
				return 0
			}
			fmt.Fprintf(stderr, "unknown setting %s\n", args[1])
			return 2
		}
		fmt.Fprintln(stdout, c.Get(args[1]))
	case "set":
		return configSet(c, args[1], args[2], stdout, stderr)
	case "unset":
		if err := c.Unset(args[1]); err != nil {
			fmt.Fprintln(stderr, "config:", err)
			return 1
		}
		fmt.Fprintf(stdout, "unset %s\n", args[1])
		return restartFor(c, args[1], stdout, stderr)
	case "permissions":
		mode := args[1]
		if mode != "ask" && mode != "skip" {
			fmt.Fprintln(stderr, configUsage)
			return 2
		}
		return configSet(c, "AGENT_PERMISSIONS", mode, stdout, stderr)
	}
	return 0
}

func configSet(c *config.Config, key, value string, stdout, stderr io.Writer) int {
	err := c.Set(key, value)
	if errors.Is(err, config.ErrUnknownKey) {
		// Written anyway (a newer hommy may know it); every daemon logs and ignores it.
		if err := c.SetUnchecked(key, value); err != nil {
			fmt.Fprintln(stderr, "config:", err)
			return 1
		}
		fmt.Fprintf(stderr, "warning: unknown setting %s: written, ignored by this hommy\n", key)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "config: refused: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "%s=%s\n", key, value)
	return restartFor(c, key, stdout, stderr)
}

// restartFor restarts the installed services that read key.
func restartFor(c *config.Config, key string, stdout, stderr io.Writer) int {
	var names []string
	for _, s := range config.Services(key) {
		if serviceLoaded(c, s) {
			names = append(names, s)
		}
	}
	if len(names) == 0 {
		return 0
	}
	if err := newManager(c, stdout).Restart(names...); err != nil {
		fmt.Fprintln(stderr, "restart:", err)
		return 1
	}
	if err := settle(c, names); err != nil {
		fmt.Fprintln(stderr, "restart:", err)
		return 1
	}
	return 0
}

// store is the instance's Keychain service.
func store(c *config.Config) secret.Store { return secret.Store{Service: c.Inst.KeychainService()} }

// sendConfig is `hommy send`'s config from the instance (agentcli.Instance).
func sendConfig(c *config.Config) (agentcli.Config, error) {
	st := store(c)
	// only the direct fallback needs it; a --join host has none and sends through the hub
	tok, err := st.Get(secret.TelegramToken)
	if err != nil && !errors.Is(err, secret.ErrNotFound) {
		return agentcli.Config{}, fmt.Errorf("telegram-bot-token: %v", err)
	}
	chat := c.Get("OWNER_CHAT_ID")
	if chat == "" { // paired from the phone without `hommy pair` waiting: ask the hub once
		if cl, err := hubClient(c); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			st, err := cl.Status(ctx)
			cancel()
			if err == nil && st.Paired {
				saveOwner(c, st, io.Discard)
				chat = c.Get("OWNER_CHAT_ID")
			}
		}
	}
	if chat == "" {
		return agentcli.Config{}, fmt.Errorf("not paired yet: run hommy pair")
	}
	out := agentcli.Config{Path: c.Path, Token: tok, Chat: chat, HubURL: c.Get("HUB_URL"), HostName: c.Get("HOST_NAME"),
		SpoolDir: c.GetPath("SPOOL_DIR"), BotAPI: strings.TrimSuffix(c.Get("TELEGRAM_API_URL"), "/")}
	out.ChatID, _ = strconv.ParseInt(chat, 10, 64)
	if out.Key, err = st.Get(secret.HostKey); err != nil {
		return agentcli.Config{}, fmt.Errorf("host-key: %v (run hommy setup)", err)
	}
	return out, nil
}

// hostKeyCmd: `hommy hub host-key add NAME`.
func runHostKey(c *config.Config, args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "add" {
		fmt.Fprintln(stderr, "usage: hommy hub host-key add NAME")
		return 2
	}
	name := args[1]
	if config.SanitizeHostName(name) != name {
		fmt.Fprintf(stderr, "host name %q: want [a-z0-9-], 1–32 chars\n", name)
		return 2
	}
	st := store(c)
	cur, err := st.Get(secret.HubHostKeys)
	if err != nil && !errors.Is(err, secret.ErrNotFound) {
		fmt.Fprintln(stderr, "keychain:", err)
		return 1
	}
	keys := secret.HostKeys(cur)
	if _, ok := keys[name]; ok {
		fmt.Fprintf(stderr, "host %s already has a key\n", name)
		return 3
	}
	k, err := secret.NewKey()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	keys[name] = k
	if err := st.Set(secret.HubHostKeys, secret.FormatHostKeys(keys)); err != nil {
		fmt.Fprintln(stderr, "keychain:", err)
		return 1
	}
	fmt.Fprintln(stdout, k)
	fmt.Fprintf(stderr, "key for host %s (shown once). On that Mac: printf %%s KEY | hommy setup --join ws://<this Mac>:%s/ws --name %s --key-stdin\n",
		name, c.Port(0), name)
	if l := c.Get("HUB_LISTEN"); strings.HasPrefix(l, "127.") || strings.HasPrefix(l, "localhost") {
		fmt.Fprintf(stderr, "the hub listens on %s only: for another Mac, hommy config set HUB_LISTEN <this Mac's LAN address>:%s\n", l, c.Port(0))
	}
	if serviceLoaded(c, "hub") {
		if err := newManager(c, io.Discard).Restart("hub"); err != nil {
			fmt.Fprintln(stderr, "restart hub:", err)
			return 1
		}
	}
	return 0
}
