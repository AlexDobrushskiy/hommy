// Package agentcli is `hommy send` and `hommy inbox`: what agent sessions run.
package agentcli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is where messages go: the instance's config and Keychain (Instance),
// or an env file given with --env-file / $HOMMY_SEND_ENV:
//
//	TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_ID  — direct Bot API (always needed: the fallback)
//	HUB_URL, HOST_NAME, HOST_KEY_FILE     — send through the hub (a host's env file has them)
//	SPOOL_DIR                             — default ~/.local/state/hommy/spool
type Config struct {
	Path     string
	Token    string
	Chat     string // TELEGRAM_CHAT_ID as written (send.py sent it verbatim: @channel works)
	ChatID   int64  // numeric chat (0 when Chat isn't a number; needed with a hub)
	HubURL   string // ws://…/ws, as the host has it
	HostName string
	Key      string
	SpoolDir string
	BotAPI   string // TELEGRAM_API_URL (tests); default https://api.telegram.org
}

// Instance builds the Config from the instance (set by cmd/hommy: config file,
// Keychain token and host key, OWNER_CHAT_ID). Used when no env file is named.
var Instance func() (Config, error)

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func expand(p string) string {
	if p == "~" {
		return home()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home(), p[2:])
	}
	return p
}

// ConfigPath picks the env file.
func ConfigPath(flagPath string) string {
	switch {
	case flagPath != "":
		return expand(flagPath)
	case os.Getenv("HOMMY_SEND_ENV") != "":
		return expand(os.Getenv("HOMMY_SEND_ENV"))
	}
	return ""
}

// parseEnv reads KEY=VALUE lines the way send.py did.
func parseEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	env := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		env[strings.TrimSpace(k)] = strings.Trim(strings.Trim(strings.TrimSpace(v), `"`), `'`)
	}
	return env, sc.Err()
}

// LoadConfig reads the env file ("" = the instance).
func LoadConfig(path string) (Config, error) {
	if path == "" {
		if Instance == nil {
			return Config{}, fmt.Errorf("no config: run hommy setup, or pass --env-file")
		}
		return Instance()
	}
	env, err := parseEnv(path)
	if err != nil {
		return Config{}, fmt.Errorf("%s", errnoText(err))
	}
	c := Config{Path: path, Token: env["TELEGRAM_BOT_TOKEN"], HubURL: env["HUB_URL"], HostName: env["HOST_NAME"],
		SpoolDir: expand(env["SPOOL_DIR"]), BotAPI: strings.TrimSuffix(env["TELEGRAM_API_URL"], "/")}
	chat := env["TELEGRAM_CHAT_ID"]
	if c.Token == "" || chat == "" {
		return Config{}, fmt.Errorf("TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID missing in %s", path)
	}
	c.Chat = chat
	if c.ChatID, err = strconv.ParseInt(chat, 10, 64); err != nil && env["HUB_URL"] != "" {
		return Config{}, fmt.Errorf("TELEGRAM_CHAT_ID in %s is not a number", path)
	}
	if c.BotAPI == "" {
		c.BotAPI = "https://api.telegram.org"
	}
	if c.SpoolDir == "" {
		c.SpoolDir = expand("~/.local/state/hommy/spool")
	}
	if c.HubURL != "" {
		if kf := env["HOST_KEY_FILE"]; kf != "" {
			b, err := os.ReadFile(expand(kf))
			if err != nil {
				return Config{}, fmt.Errorf("HOST_KEY_FILE: %s", errnoText(err))
			}
			c.Key = strings.TrimSpace(string(b))
		}
		if c.Key == "" || c.HostName == "" {
			return Config{}, fmt.Errorf("HUB_URL in %s needs HOST_NAME and HOST_KEY_FILE", path)
		}
	}
	return c, nil
}

// BotID is the bot the token belongs to (its numeric prefix; 0 if unreadable).
func (c Config) BotID() int64 {
	id, _ := strconv.ParseInt(strings.SplitN(c.Token, ":", 2)[0], 10, 64)
	return id
}

// Hub reports whether messages go through the hub.
func (c Config) Hub() bool { return c.HubURL != "" }

// apiBase turns the host's ws://host:port/ws into http://host:port.
func (c Config) apiBase() string {
	u := c.HubURL
	switch {
	case strings.HasPrefix(u, "ws://"):
		u = "http://" + strings.TrimPrefix(u, "ws://")
	case strings.HasPrefix(u, "wss://"):
		u = "https://" + strings.TrimPrefix(u, "wss://")
	}
	return strings.TrimSuffix(strings.TrimSuffix(u, "/"), "/ws")
}

// hostName names this machine in the header: HOST_NAME, $HOMMY_HOST, a
// hommy host's env file, else the short hostname.
func (c Config) hostName() string {
	if c.HostName != "" {
		return c.HostName
	}
	if h := os.Getenv("HOMMY_HOST"); h != "" {
		return h
	}
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	return strings.ToLower(h)
}

// errnoText is Python's "[Errno 2] No such file or directory: 'x'"-free short form.
func errnoText(err error) string {
	if pe, ok := err.(*os.PathError); ok {
		return pe.Err.Error() + ": " + pe.Path
	}
	return err.Error()
}
