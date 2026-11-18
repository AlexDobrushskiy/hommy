package hook

import "path/filepath"

// SocketPath is the host's hook relay socket inside its data dir.
func SocketPath(dataDir string) string { return filepath.Join(dataDir, "hook.sock") }

// SpoolDir holds a Telegram turn's hook requests while the host is down.
func SpoolDir(dataDir string) string { return filepath.Join(dataDir, "hookspool") }

// LogPath is the one-line-per-run hook log.
func LogPath(dataDir string) string { return filepath.Join(dataDir, "hook.log") }
