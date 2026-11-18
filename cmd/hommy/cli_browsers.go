package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/AlexDobrushskiy/hommy/internal/browsers"
	"github.com/AlexDobrushskiy/hommy/internal/config"
)

// runBrowsers: `hommy browsers configure` writes the instance's hommy-browsers
// config.env into HB_HOME (install.sh stage 7; setup step 9 does the same) and
// prints `config.env: changed|unchanged` plus the ports and labels.
func runBrowsers(c *config.Config, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "configure" {
		fmt.Fprintln(stderr, "usage: hommy browsers configure")
		return 2
	}
	hb := browsers.Home(c)
	if _, err := os.Stat(filepath.Join(hb, "install.sh")); err != nil {
		fmt.Fprintf(stderr, "browsers: %s is not a hommy-browsers checkout (install.sh installs it)\n", hb)
		return 3
	}
	changed, err := browsers.WriteConfig(c, browsers.Node())
	if err != nil {
		fmt.Fprintln(stderr, "browsers:", err)
		return 1
	}
	state := "unchanged"
	if changed {
		state = "changed"
	}
	lanes, identity := browsers.Ports(c)
	prefix := "dev.hommy.browsers"
	if c.Inst.Name != "" {
		prefix = c.Inst.LabelPrefix() + ".browsers"
	}
	fmt.Fprintf(stdout, "config.env: %s (%s)\n", state, filepath.Join(hb, "config.env"))
	fmt.Fprintf(stdout, "ports: lanes %d, identity %d; labels %s.*\n", lanes, identity, prefix)
	return 0
}
