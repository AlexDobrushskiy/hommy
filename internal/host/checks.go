package host

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
)

// VaultCheck is the heartbeat's vault state: ok (exists, readable by this
// process, has CLAUDE.md), missing, or denied (TCC or permissions).
func VaultCheck(dir string) string {
	f, err := os.Open(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "missing"
	case err != nil:
		return "denied"
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, os.ErrNotExist) {
		if errors.Is(err, fs.ErrPermission) {
			return "denied"
		}
	}
	b, err := os.Open(filepath.Join(dir, "CLAUDE.md"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "missing"
	case err != nil:
		return "denied"
	}
	b.Close()
	return "ok"
}

// agtermCheck answers ok when agtermctl reaches agterm, else down.
func agtermCheck(ctx context.Context) string {
	p, err := agterm.FindAgtermctl()
	if err != nil {
		return "down"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, p, "version").Run() != nil {
		return "down"
	}
	return "ok"
}

func checksFunc(cfg Config) func(ctx context.Context) map[string]string {
	return func(ctx context.Context) map[string]string {
		return map[string]string{"vault": VaultCheck(cfg.VaultDir), "agterm": agtermCheck(ctx)}
	}
}
