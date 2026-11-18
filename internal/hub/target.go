package hub

import (
	"fmt"
	"regexp"
	"strings"
)

// Target kinds.
const (
	KindVault = "vault"
	KindP     = "p"
	KindNew   = "new" // a session created from the next message (phase 08)
)

// New-session modes.
const (
	ModeInteractive = "interactive"
	ModeP           = "-p"
)

// Target is where a message goes:
// vault@<host> · p@<host>:<dir> · session@<host>:<agterm id> · new@<host>:<dir>:<interactive|-p>.
type Target struct {
	Kind string
	Host string
	Dir  string // p and new: the dir (resolved and validated on the host); session: the agterm id
	Mode string // new: interactive | -p
}

var (
	hostRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

const targetUsage = "vault@<host> · p@<host>:<dir> · session@<host>:<id> · new@<host>:<dir>:<interactive|-p>"

// ParseTarget parses a target spec (see Target).
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	kind, rest, ok := strings.Cut(s, "@")
	if !ok {
		return Target{}, fmt.Errorf("target %q: want %s", s, targetUsage)
	}
	host, arg, _ := strings.Cut(rest, ":")
	if !hostRe.MatchString(host) {
		return Target{}, fmt.Errorf("bad host in %q", s)
	}
	switch kind {
	case KindVault:
		if arg != "" || strings.Contains(rest, ":") {
			return Target{}, fmt.Errorf("target %q: want vault@<host>", s)
		}
		return Target{Kind: KindVault, Host: host}, nil
	case KindP:
		dir, err := parseDir(s, arg)
		if err != nil {
			return Target{}, err
		}
		return Target{Kind: KindP, Host: host, Dir: dir}, nil
	case KindSession:
		return parseSessionTarget(s, rest)
	case KindNew:
		i := strings.LastIndex(arg, ":")
		if i < 0 {
			return Target{}, fmt.Errorf("target %q: want new@<host>:<dir>:<interactive|-p>", s)
		}
		mode := arg[i+1:]
		if mode != ModeInteractive && mode != ModeP {
			return Target{}, fmt.Errorf("target %q: mode must be interactive or -p", s)
		}
		dir, err := parseDir(s, arg[:i])
		if err != nil {
			return Target{}, err
		}
		return Target{Kind: KindNew, Host: host, Dir: dir, Mode: mode}, nil
	}
	return Target{}, fmt.Errorf("target %q: unknown kind %q (vault, p, session, new)", s, kind)
}

func parseDir(s, dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if !strings.HasPrefix(dir, "/") && dir != "~" && !strings.HasPrefix(dir, "~/") {
		return "", fmt.Errorf("target %q: dir must be absolute or start with ~/", s)
	}
	if strings.ContainsAny(dir, "\x00\n\r") {
		return "", fmt.Errorf("target %q: bad characters in dir", s)
	}
	return dir, nil
}

func (t Target) String() string {
	switch t.Kind {
	case KindP:
		return "p@" + t.Host + ":" + t.Dir
	case KindSession:
		return "session@" + t.Host + ":" + t.Dir
	case KindNew:
		return "new@" + t.Host + ":" + t.Dir + ":" + t.Mode
	}
	return t.Kind + "@" + t.Host
}
