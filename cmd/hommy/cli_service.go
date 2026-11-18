package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/pairing"
	"github.com/AlexDobrushskiy/hommy/internal/service"
)

const serviceUsage = `usage: hommy service install|uninstall [--purge]|start|stop|restart|status [hub|host|speech|all]`

func newManager(c *config.Config, out io.Writer) *service.Manager {
	exe, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return &service.Manager{Cfg: c, Exe: exe, Out: out}
}

// serviceLoaded reports whether the instance's service is loaded in launchd.
func serviceLoaded(c *config.Config, name string) bool {
	st, err := newManager(c, io.Discard).Status(name)
	return err == nil && len(st) == 1 && st[0].Loaded
}

func runService(c *config.Config, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, serviceUsage)
		return 2
	}
	verb, rest := args[0], args[1:]
	purge := false
	var names []string
	for _, a := range rest {
		switch a {
		case "--purge":
			if verb != "uninstall" {
				fmt.Fprintln(stderr, serviceUsage)
				return 2
			}
			purge = true
		case "hub", "host", "speech", "all":
			names = append(names, a)
		default:
			fmt.Fprintln(stderr, serviceUsage)
			return 2
		}
	}
	m := newManager(c, stdout)
	var err error
	switch verb {
	case "install":
		err = m.Install(names...)
	case "uninstall":
		err = m.Uninstall(purge, names...)
	case "start":
		if err = m.Start(names...); err == nil {
			err = settle(c, names)
		}
	case "stop":
		err = m.Stop(names...)
	case "restart":
		if err = m.Restart(names...); err == nil {
			err = settle(c, names)
		}
	case "status":
		st, serr := m.Status(names...)
		if serr != nil {
			err = serr
			break
		}
		down := false
		for _, s := range st {
			state := "not installed"
			switch {
			case s.Running:
				state = fmt.Sprintf("running (pid %d)", s.PID)
			case s.Loaded:
				state = "loaded, not running"
				down = true
			case s.Installed:
				state = "installed, not loaded"
				down = true
			}
			fmt.Fprintf(stdout, "%-7s %-28s %s\n", s.Name, s.Label, state)
		}
		if down {
			return 1
		}
		return 0
	default:
		fmt.Fprintln(stderr, serviceUsage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "service:", err)
		return 1
	}
	return 0
}

// settle waits (≤ 20 s in all) until a (re)started hub answers /healthz and
// this Mac's host is online at the hub again, so the next command finds them up.
func settle(c *config.Config, names []string) error {
	all := len(names) == 0
	has := func(n string) bool { return all || slices.Contains(names, n) || slices.Contains(names, "all") }
	base, err := pairing.BaseURL(c.Get("HUB_URL"))
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if has("hub") && serviceLoaded(c, "hub") {
		for {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					break
				}
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("the hub did not come up within 20 s (see %s)", filepath.Join(c.Inst.LogDir("hub"), "err.log"))
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	// A restarted hub drops every host; a restarted host reconnects: either way, wait for this host.
	if (has("host") || has("hub")) && serviceLoaded(c, "host") {
		started := time.Now()
		cl, err := hubClient(c)
		if err != nil {
			return nil
		}
		name := c.Get("HOST_NAME")
		for {
			qctx, qcancel := context.WithTimeout(ctx, 3*time.Second)
			st, err := cl.APIStatus(qctx)
			qcancel()
			if err == nil {
				for _, h := range st.Hosts {
					if h.Name == name && h.Online && h.LastSeen != nil && !h.LastSeen.Before(started.Add(-time.Second)) {
						return nil
					}
				}
			}
			select {
			case <-ctx.Done():
				return nil // the hub may be elsewhere or down: the host keeps retrying on its own
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return nil
}
