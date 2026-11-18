package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// countClaudeProcesses counts running Claude Code processes (executable named claude).
func countClaudeProcesses(ctx context.Context) int {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "comm=").Output()
	if err != nil {
		return -1
	}
	return countClaude(string(out))
}

func countClaude(psComm string) int {
	n := 0
	for _, line := range strings.Split(psComm, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if filepath.Base(line) == "claude" || strings.Contains(line, "/claude/versions/") {
			n++
		}
	}
	return n
}

// agentFolders lists up to max subdirectory names of dir, newest first.
func agentFolders(dir string, max int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type d struct {
		name string
		mod  time.Time
	}
	var ds []d
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		ds = append(ds, d{e.Name(), info.ModTime()})
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].mod.After(ds[j].mod) })
	var out []string
	for i := 0; i < len(ds) && i < max; i++ {
		out = append(out, ds[i].name)
	}
	return out
}
