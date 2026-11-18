package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/envfile"
)

// Config is the host's setup, from env (optionally loaded from an env file).
type Config struct {
	Name           string        // HOST_NAME (the Mac's LocalHostName by default)
	HubURL         string        // HUB_URL, ws://127.0.0.1:8470/ws
	Key            string        // the Keychain item host-key
	DataDir        string        // HOST_DATA_DIR: ledger, logs
	VaultDir       string        // VAULT_DIR: where vault@<host> runs
	MediaDir       string        // MEDIA_DIR: photos/files from Telegram
	ClaudeBin      string        // CLAUDE_BIN
	ClaudeTimeout  time.Duration // CLAUDE_TIMEOUT
	AgentBriefsDir string        // AGENT_BRIEFS_DIR, for heartbeat folder names
	Home           string        // every path a job names must resolve under it
	HeartbeatEvery time.Duration // 0 = proto.HeartbeatEvery
	ProgressEvery  time.Duration // HOST_PROGRESS_EVERY: min gap between run_p progress events (0 = every change)
	KillGrace      time.Duration // CLAUDE_KILL_GRACE: SIGTERM → SIGKILL on cancel (0 = 5 s)
	SpoolDir       string        // SPOOL_DIR: what `hommy send` couldn't tell the hub ("" = off)
	HookSocket     string        // HOOK_SOCKET: the hookrelay socket ("" = <DataDir>/hook.sock)
	NewRoots       []string      // NEW_SESSION_ROOTS: project roots new sessions may use besides AgentBriefsDir
	NewPinned      []string      // NEW_SESSION_PINNED: folders the picker offers first, allowed as they are
	AgentCommand   string        // AGENT_COMMAND (shell text; absolute path of claude by default)
	SkipPerms      bool          // AGENT_PERMISSIONS=skip
	Workspace      string        // AGTERM_WORKSPACE
	ClaudeConfig   string        // CLAUDE_CONFIG_DIR of the host process (passed on to sessions)
}

// AgentCommandLine is what agterm runs for a new session (master spec §3.2).
func (c Config) AgentCommandLine() string {
	return agterm.AgentCommandLine(c.AgentCommand, c.Home, c.ClaudeConfig, c.SkipPerms)
}

// DefaultSpoolDir is where `hommy send` / `inbox ack` spool and the host flushes.
const DefaultSpoolDir = "~/.local/state/hommy/spool"

// KeyFunc reads the host key (the Keychain item host-key; tests replace it).
type KeyFunc func() (string, error)

func (c Config) ledgerPath() string { return filepath.Join(c.DataDir, "ledger.jsonl") }

// newSessionRoots: where a new session may start besides AgentBriefsDir.
func (c Config) newSessionRoots() []string {
	return append(append([]string{}, c.NewRoots...), c.NewPinned...)
}

func (c Config) progressEvery() time.Duration { return c.ProgressEvery }

func (c Config) heartbeatEvery() time.Duration {
	if c.HeartbeatEvery > 0 {
		return c.HeartbeatEvery
	}
	return 10 * time.Second
}

// LoadConfig reads the env (config.Export put the instance's settings there);
// the host key comes from key().
func LoadConfig(key KeyFunc) (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}
	dataDir := expandHome(envfile.Get("HOST_DATA_DIR", filepath.Join(home, ".local/state/hommy/host")), home)
	cfg := Config{
		Name:           os.Getenv("HOST_NAME"),
		HubURL:         os.Getenv("HUB_URL"),
		DataDir:        dataDir,
		VaultDir:       expandHome(envfile.Get("VAULT_DIR", filepath.Join(home, "hommy-vault")), home),
		MediaDir:       expandHome(envfile.Get("MEDIA_DIR", filepath.Join(dataDir, "media")), home),
		ClaudeBin:      envfile.Get("CLAUDE_BIN", "claude"),
		AgentBriefsDir: expandHome(envfile.Get("AGENT_BRIEFS_DIR", filepath.Join(home, ".local/share/hommy/briefs")), home),
		Home:           home,
		SpoolDir:       expandHome(envfile.Get("SPOOL_DIR", DefaultSpoolDir), home),
		HookSocket:     expandHome(os.Getenv("HOOK_SOCKET"), home),
		NewRoots:       agterm.ParseRoots(os.Getenv("NEW_SESSION_ROOTS"), home),
		NewPinned:      agterm.ParsePinned(os.Getenv("NEW_SESSION_PINNED"), home),
		SkipPerms:      envfile.Get("AGENT_PERMISSIONS", "skip") != "ask",
		Workspace:      envfile.Get("AGTERM_WORKSPACE", agterm.DefaultWorkspace),
		ClaudeConfig:   os.Getenv("CLAUDE_CONFIG_DIR"),
	}
	cfg.AgentCommand = envfile.Get("AGENT_COMMAND", cfg.ClaudeBin)
	if cfg.Name == "" || cfg.HubURL == "" {
		return Config{}, fmt.Errorf("HOST_NAME and HUB_URL are required")
	}
	if key == nil {
		return Config{}, fmt.Errorf("no host key source")
	}
	if cfg.Key, err = key(); err != nil {
		return Config{}, fmt.Errorf("host key (Keychain item host-key): %w; run hommy setup", err)
	}
	cfg.Key = strings.TrimSpace(cfg.Key)
	if len(cfg.Key) < 32 {
		return Config{}, fmt.Errorf("the host key is shorter than 32 chars")
	}
	for _, d := range []struct {
		dst      *time.Duration
		key, def string
	}{{&cfg.ClaudeTimeout, "CLAUDE_TIMEOUT", "30m"}, {&cfg.ProgressEvery, "HOST_PROGRESS_EVERY", "500ms"},
		{&cfg.KillGrace, "CLAUDE_KILL_GRACE", "5s"}} {
		v, err := time.ParseDuration(envfile.Get(d.key, d.def))
		if err != nil {
			return Config{}, fmt.Errorf("invalid %s: %w", d.key, err)
		}
		*d.dst = v
	}
	return cfg, nil
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}
