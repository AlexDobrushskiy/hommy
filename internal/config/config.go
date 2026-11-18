package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Config is one instance's resolved settings: process env > file > default.
type Config struct {
	Inst Instance
	Path string // the config file

	lines []string          // the file as read (comments and order kept on write)
	file  map[string]string // key → value from the file
	env   func(string) (string, bool)
	// HostNameFn finds the Mac's LocalHostName (tests replace it).
	HostNameFn func() string
}

// ErrUnknownKey is returned by Set for a key not in the table.
var ErrUnknownKey = errors.New("unknown setting")

// Load reads the instance's config file (path "" = HOMMY_CONFIG, else the default).
// A missing file is not an error: every key then has its default.
func Load(inst Instance, path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("HOMMY_CONFIG")
	}
	if path == "" {
		path = inst.ConfigFile()
	}
	c := &Config{Inst: inst, Path: ExpandHome(path, inst.Home), file: map[string]string{}, env: os.LookupEnv,
		HostNameFn: LocalHostName}
	b, err := os.ReadFile(c.Path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		c.lines = append(c.lines, line)
		if k, v, ok := parseLine(line); ok {
			c.file[k] = v
		}
	}
	return c, sc.Err()
}

// WithEnv replaces the process environment lookup (tests).
func (c *Config) WithEnv(fn func(string) (string, bool)) *Config { c.env = fn; return c }

func parseLine(line string) (k, v string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	k, v, ok = strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`), true
}

// Exists reports whether the config file exists.
func (c *Config) Exists() bool { _, err := os.Stat(c.Path); return err == nil }

// FileValue is the value written in the file (ok=false when absent).
func (c *Config) FileValue(key string) (string, bool) { v, ok := c.file[key]; return v, ok }

// Get resolves key: process env, then the file, then the default.
func (c *Config) Get(key string) string {
	if v, ok := c.env(key); ok && v != "" {
		return v
	}
	if v, ok := c.file[key]; ok && v != "" {
		return v
	}
	if k, ok := keyIndex[key]; ok && k.Default != nil {
		return k.Default(c)
	}
	return ""
}

// Path resolves a path-valued key with ~ expanded.
func (c *Config) GetPath(key string) string { return ExpandHome(c.Get(key), c.Inst.Home) }

// PortBase is PORT_BASE as a number (8470 when unreadable: validation reports it).
func (c *Config) PortBase() int {
	n, err := strconv.Atoi(c.Get("PORT_BASE"))
	if err != nil {
		return 8470
	}
	return n
}

// Port is PORT_BASE + off.
func (c *Config) Port(off int) string { return strconv.Itoa(c.PortBase() + off) }

// Unknown lists file keys that are not settings (logged as `unknown setting KEY`).
func (c *Config) Unknown() []string {
	var out []string
	for k := range c.file {
		if _, ok := keyIndex[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// Validate checks every key's resolved value; the first error names the key.
func (c *Config) Validate() error {
	for _, k := range Keys {
		if k.Check == nil {
			continue
		}
		if err := k.Check(c, c.Get(k.Name)); err != nil {
			return fmt.Errorf("%s: %w", k.Name, err)
		}
	}
	return nil
}

// Export logs unknown keys, validates, and puts every resolved setting into the
// process environment, so the daemons' env readers see file values and the
// instance's defaults. Variables already set win.
func (c *Config) Export() error {
	for _, k := range c.Unknown() {
		log.Printf("unknown setting %s", k)
	}
	if err := c.Validate(); err != nil {
		return err
	}
	for _, k := range Keys {
		if _, set := c.env(k.Name); set {
			continue
		}
		v := c.Get(k.Name)
		if k.Path {
			v = ExpandHome(v, c.Inst.Home)
		}
		os.Setenv(k.Name, v)
	}
	os.Setenv("HOMMY_INSTANCE", c.Inst.Name)
	return nil
}

// Set validates and writes key=value into the file (0600).
func (c *Config) Set(key, value string) error {
	k, ok := keyIndex[key]
	if !ok {
		return fmt.Errorf("%w %s", ErrUnknownKey, key)
	}
	if k.Check != nil {
		if err := k.Check(c, value); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	c.file[key] = value
	return c.write(key, value, false)
}

// SetUnchecked writes a key the table does not know (tests of `unknown setting`).
func (c *Config) SetUnchecked(key, value string) error {
	c.file[key] = value
	return c.write(key, value, false)
}

// Unset removes key from the file.
func (c *Config) Unset(key string) error {
	delete(c.file, key)
	return c.write(key, "", true)
}

func (c *Config) write(key, value string, remove bool) error {
	var out []string
	done := false
	for _, line := range c.lines {
		if k, _, ok := parseLine(line); ok && k == key {
			if !remove && !done {
				out = append(out, key+"="+value)
				done = true
			}
			continue
		}
		out = append(out, line)
	}
	if !remove && !done {
		out = append(out, key+"="+value)
	}
	c.lines = out
	return c.Save()
}

// Save writes the file atomically with mode 0600 (its dir 0700).
func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o700); err != nil {
		return err
	}
	body := strings.Join(c.lines, "\n")
	if body != "" {
		body += "\n"
	}
	tmp := c.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.Path)
}

// List is every known key with its resolved value and where it came from.
func (c *Config) List() [][3]string {
	var out [][3]string
	for _, k := range Keys {
		src := "default"
		if v, ok := c.env(k.Name); ok && v != "" {
			src = "env"
		} else if v, ok := c.file[k.Name]; ok && v != "" {
			src = "file"
		}
		out = append(out, [3]string{k.Name, c.Get(k.Name), src})
	}
	return out
}

// Services names the services a key change restarts.
func Services(key string) []string {
	if k, ok := keyIndex[key]; ok {
		return k.Services
	}
	return nil
}

// LocalHostName is the Mac's LocalHostName, lowercased to [a-z0-9-].
func LocalHostName() string {
	out, err := exec.Command("/usr/sbin/scutil", "--get", "LocalHostName").Output()
	name := strings.TrimSpace(string(out))
	if err != nil || name == "" {
		name, _ = os.Hostname()
		name, _, _ = strings.Cut(name, ".")
	}
	return SanitizeHostName(name)
}

var nonHost = regexp.MustCompile(`[^a-z0-9-]+`)

// SanitizeHostName lowercases and keeps [a-z0-9-] (max 32, as host names in targets).
func SanitizeHostName(s string) string {
	s = nonHost.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	if s == "" {
		s = "mac"
	}
	return s
}

// --- validation helpers ---

var hostNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func checkListen(_ *Config, v string) error {
	if v == "" {
		return errors.New("empty")
	}
	h, p, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("%q is not host:port", v)
	}
	if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q: bad port", v)
	}
	if h != "" && net.ParseIP(h) == nil && h != "localhost" {
		return fmt.Errorf("%q: host must be an IP address", v)
	}
	return nil
}

func checkOptionalListen(c *Config, v string) error {
	if v == "" {
		return nil
	}
	return checkListen(c, v)
}

func checkURL(schemes ...string) func(*Config, string) error {
	return func(_ *Config, v string) error {
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || !slices.Contains(schemes, u.Scheme) {
			return fmt.Errorf("%q: want a %s URL", v, strings.Join(schemes, "|"))
		}
		return nil
	}
}

func checkDuration(_ *Config, v string) error {
	if _, err := time.ParseDuration(v); err != nil {
		return fmt.Errorf("%q is not a duration (e.g. 2m, 500ms)", v)
	}
	return nil
}

func checkInt(lo, hi int) func(*Config, string) error {
	return func(_ *Config, v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return fmt.Errorf("%q: want a number %d…%d", v, lo, hi)
		}
		return nil
	}
}

func checkOneOf(vals ...string) func(*Config, string) error {
	return func(_ *Config, v string) error {
		if !slices.Contains(vals, v) {
			return fmt.Errorf("%q: want one of %s", v, strings.Join(vals, ", "))
		}
		return nil
	}
}

func checkAbsPath(_ *Config, v string) error {
	if v == "" {
		return errors.New("empty")
	}
	if !strings.HasPrefix(v, "/") && v != "~" && !strings.HasPrefix(v, "~/") {
		return fmt.Errorf("%q: want an absolute path or ~/…", v)
	}
	return nil
}

func checkOptionalAbsPath(c *Config, v string) error {
	if v == "" {
		return nil
	}
	return checkAbsPath(c, v)
}

func checkHostName(_ *Config, v string) error {
	if !hostNameRe.MatchString(v) {
		return fmt.Errorf("%q: want [a-z0-9-], 1–32 chars", v)
	}
	return nil
}

func checkTarget(_ *Config, v string) error {
	kind, rest, ok := strings.Cut(v, "@")
	if !ok || !slices.Contains([]string{"vault", "p", "session", "new"}, kind) {
		return fmt.Errorf("%q: want vault@<host> · p@<host>:<dir> · session@<host>:<id> · new@<host>:<dir>:<interactive|-p>", v)
	}
	host, _, _ := strings.Cut(rest, ":")
	return checkHostName(nil, host)
}

func checkTTSURL(c *Config, v string) error {
	if strings.EqualFold(v, "off") {
		return nil
	}
	return checkURL("http", "https")(c, v)
}

func checkMiniAppURL(_ *Config, v string) error {
	if v == "" {
		return nil
	}
	if !strings.HasPrefix(v, "https://") {
		return fmt.Errorf("%q: must be https:// (Telegram requires it)", v)
	}
	return nil
}

func checkChatID(_ *Config, v string) error {
	if v == "" {
		return nil
	}
	if _, err := strconv.ParseInt(v, 10, 64); err != nil {
		return fmt.Errorf("%q is not a chat id", v)
	}
	return nil
}
