package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/AlexDobrushskiy/hommy/internal/vault"
)

const vaultUsage = "usage: hommy vault init PATH | hommy vault add-recipes PATH"

// runVault: `hommy vault init PATH` (exit 3 when PATH is not empty) and
// `hommy vault add-recipes PATH` (never overwrites; prints the routing rows).
func runVault(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, vaultUsage)
		return 2
	}
	dir, err := filepath.Abs(args[1])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	switch args[0] {
	case "init":
		n, err := vault.Init(dir)
		if errors.Is(err, vault.ErrNotEmpty) {
			fmt.Fprintf(stderr, "vault: %s is not empty (hommy vault add-recipes %s adds what it lacks)\n", dir, dir)
			return 3
		}
		if err != nil {
			fmt.Fprintln(stderr, "vault:", err)
			return 1
		}
		fmt.Fprintf(stdout, "vault: initialized %s (%d files)\n", dir, n)
		return 0
	case "add-recipes":
		added, rows, err := vault.AddRecipes(dir)
		if err != nil {
			fmt.Fprintln(stderr, "vault:", err)
			return 1
		}
		for _, p := range added {
			fmt.Fprintf(stdout, "added %s\n", p)
		}
		if len(added) == 0 {
			fmt.Fprintln(stdout, "nothing to add: every template file is there")
		}
		fmt.Fprintln(stdout, "routing rows for your kernel (AGENTS.md or CLAUDE.md), add the ones it lacks:")
		for _, r := range rows {
			fmt.Fprintln(stdout, r)
		}
		return 0
	}
	fmt.Fprintln(stderr, vaultUsage)
	return 2
}
