package main

import (
	"context"
	"io"
	"slices"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/hook"
)

// runHook is `hommy hook <event>`: a hook run never
// prints and always exits 0, soon. Where it reports comes from the instance's
// config: HOOK_SOCKET, and <HOST_DATA_DIR>/hookspool and hook.log.
func runHook(g globals, args []string, stdin io.Reader) int {
	watchdog := time.AfterFunc(hook.Timeout+time.Second, func() { exit(0) })
	defer watchdog.Stop()
	if len(args) == 0 || !slices.Contains(hook.Events, args[0]) {
		return 0
	}
	event, agent := args[0], hook.AgentClaude
	inst, err := config.FromEnv(g.instance)
	if err != nil {
		return 0
	}
	c, err := config.Load(inst, g.config)
	if err != nil || !c.Exists() {
		return 0
	}
	data := c.GetPath("HOST_DATA_DIR")
	cfg := hook.Config{Socket: c.GetPath("HOOK_SOCKET"), Spool: hook.SpoolDir(data), Log: hook.LogPath(data)}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), hook.Timeout)
	defer cancel()
	what, err := hook.Run(ctx, cfg, agent, event, stdin, getenv, time.Now)
	who := agent
	if id := getenv("AGTERM_SESSION_ID"); id != "" {
		who += " session=" + id
	}
	if err != nil {
		hook.Logf(cfg.Log, "%s %s: error after %s: %v", who, event, time.Since(start).Round(time.Millisecond), err)
	} else {
		hook.Logf(cfg.Log, "%s %s: %s in %s", who, event, what, time.Since(start).Round(time.Millisecond))
	}
	return 0
}
