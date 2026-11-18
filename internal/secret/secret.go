// Package secret keeps hommy's secrets in the macOS Keychain as generic
// passwords (service = the instance's, account = the item), through
// /usr/bin/security. Values go in over stdin (`security -i`), never argv.
package secret

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Items (master spec §3.4).
const (
	TelegramToken   = "telegram-bot-token"
	HostKey         = "host-key"
	HubHostKeys     = "hub-host-keys"
	GeminiKey       = "gemini-api-key"
	SigningPassword = "signing-keychain-password"
)

// ErrNotFound: the item does not exist.
var ErrNotFound = errors.New("not in the keychain")

// Store is one instance's Keychain service, optionally pinned to a keychain file.
type Store struct {
	Service  string
	Keychain string        // "" = the user's default keychain / search list
	Bin      string        // "" = /usr/bin/security
	Timeout  time.Duration // per call; 0 = 15 s (a call never waits on a dialog forever)
	Env      []string      // nil = the process env
}

func (s Store) bin() string {
	if s.Bin != "" {
		return s.Bin
	}
	return "/usr/bin/security"
}

func (s Store) run(stdin string, args ...string) ([]byte, error) {
	t := s.Timeout
	if t == 0 {
		t = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.bin(), args...)
	if s.Env != nil {
		cmd.Env = s.Env
	}
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("security %s: timed out (a locked keychain?)", args[0])
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if strings.Contains(msg, "could not be found") || exitCode(err) == 44 {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("security %s: %s", args[0], firstLine(msg, err))
	}
	return out.Bytes(), nil
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func firstLine(msg string, err error) string {
	if msg == "" {
		return err.Error()
	}
	l, _, _ := strings.Cut(msg, "\n")
	return l
}

func (s Store) kcArgs() []string {
	if s.Keychain == "" {
		return nil
	}
	return []string{s.Keychain}
}

// Get returns the item's value.
func (s Store) Get(item string) (string, error) {
	args := append([]string{"find-generic-password", "-s", s.Service, "-a", item, "-w"}, s.kcArgs()...)
	out, err := s.run("", args...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// Exists reports whether the item is present (no value is read).
func (s Store) Exists(item string) bool {
	args := append([]string{"find-generic-password", "-s", s.Service, "-a", item}, s.kcArgs()...)
	_, err := s.run("", args...)
	return err == nil
}

// Set creates or replaces the item. The value travels hex-encoded on stdin.
func (s Store) Set(item, value string) error {
	if strings.ContainsAny(s.Service+item, " \"\n") {
		return fmt.Errorf("bad service or item name")
	}
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -X %s", s.Service, item, hex.EncodeToString([]byte(value)))
	if s.Keychain != "" {
		line += " " + quote(s.Keychain)
	}
	_, err := s.run(line+"\n", "-i")
	if err != nil {
		return err
	}
	// `security -i` exits 0 even when a command fails: read it back.
	got, err := s.Get(item)
	if err != nil {
		return fmt.Errorf("keychain write of %s did not stick: %w", item, err)
	}
	if got != value {
		return fmt.Errorf("keychain write of %s did not stick", item)
	}
	return nil
}

// Delete removes the item (missing is not an error).
func (s Store) Delete(item string) error {
	args := append([]string{"delete-generic-password", "-s", s.Service, "-a", item}, s.kcArgs()...)
	_, err := s.run("", args...)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func quote(p string) string { return `"` + strings.ReplaceAll(p, `"`, `\"`) + `"` }

// HostKeys parses hub-host-keys: `name key` entries, one per line (commas also separate).
func HostKeys(v string) map[string]string {
	keys := map[string]string{}
	for _, line := range strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == ',' }) {
		f := strings.Fields(line)
		if len(f) == 2 && !strings.HasPrefix(f[0], "#") {
			keys[f[0]] = f[1]
		}
	}
	return keys
}

// FormatHostKeys is the inverse of HostKeys (sorted, comma-separated: one line).
func FormatHostKeys(keys map[string]string) string {
	var names []string
	for n := range keys {
		names = append(names, n)
	}
	sortStrings(names)
	var parts []string
	for _, n := range names {
		parts = append(parts, n+" "+keys[n])
	}
	return strings.Join(parts, ",")
}

func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// NewKey is a random 48-char hex key (host keys).
func NewKey() (string, error) {
	b := make([]byte, 24)
	f, err := os.Open("/dev/urandom")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
