// Command hommy is the Telegram-to-Claude-Code bridge. Phase 01 ships only `hommy version`.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/AlexDobrushskiy/hommy/internal/version"
)

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintf(stdout, "hommy %s\n", version.String())
		return 0
	}
	fmt.Fprintln(stderr, "usage: hommy version")
	return 2
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
