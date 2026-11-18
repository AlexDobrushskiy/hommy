package agterm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultNewSessionRoots are the project roots new sessions may start in,
// besides the briefs dir (always allowed). NEW_SESSION_ROOTS overrides them.
var DefaultNewSessionRoots = []string{"~/Projects", "~/src"}

// DefaultPinnedFolders are the folders the picker offers first. Each is allowed
// as it is (with what's below it) but not scanned for sub-folders.
// NEW_SESSION_PINNED overrides them (the config sets it to the vault).
var DefaultPinnedFolders = []string{"~/hommy-vault"}

// ParseRoots reads a comma-separated root list (~ expanded); empty means
// DefaultNewSessionRoots. Relative entries are dropped.
func ParseRoots(s, home string) []string { return parsePaths(s, home, DefaultNewSessionRoots) }

// ParsePinned reads NEW_SESSION_PINNED like ParseRoots; empty means DefaultPinnedFolders.
func ParsePinned(s, home string) []string { return parsePaths(s, home, DefaultPinnedFolders) }

func parsePaths(s, home string, def []string) []string {
	items := strings.Split(s, ",")
	if strings.TrimSpace(s) == "" {
		items = def
	}
	var out []string
	for _, it := range items {
		p := expandTilde(strings.TrimSpace(it), home)
		if filepath.IsAbs(p) {
			out = append(out, filepath.Clean(p))
		}
	}
	return out
}

// NewSessionDir resolves the folder a new session (interactive or -p) starts
// in. It must resolve, symlinks included, to one of roots or below it; it may
// not exist yet, and create makes it (mkdir -p). Roots that don't exist are
// ignored.
func NewSessionDir(home string, roots []string, dir string, create bool) (string, error) {
	dir = strings.TrimSpace(dir)
	p := expandTilde(dir, home)
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("folder must be absolute or start with ~/: %q", dir)
	}
	real, exists, err := resolvePartial(filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("folder %s: %w", dir, err)
	}
	allowed := false
	for _, root := range roots {
		rr, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if real == rr || within(rr, real) {
			allowed = true
			break
		}
	}
	if !allowed {
		list := strings.Join(TildeAll(home, ExistingRoots(roots)), ", ")
		if list == "" {
			list = "none exist on this host"
		}
		return "", fmt.Errorf("folder %s is outside the allowed folders (%s)", dir, list)
	}
	if exists {
		if st, err := os.Stat(real); err != nil || !st.IsDir() {
			return "", fmt.Errorf("%s is not a folder", dir)
		}
		return real, nil
	}
	if create {
		if err := os.MkdirAll(real, 0o755); err != nil {
			return "", fmt.Errorf("folder %s: %w", dir, err)
		}
	}
	return real, nil
}

// resolvePartial resolves symlinks in p's deepest existing ancestor and
// appends the rest; exists reports whether p itself exists.
func resolvePartial(p string) (string, bool, error) {
	var rest []string
	cur := p
	for {
		if _, err := os.Lstat(cur); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false, errors.New("no existing ancestor")
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
	real, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", false, err
	}
	return filepath.Join(append([]string{real}, rest...)...), len(rest) == 0, nil
}

func expandTilde(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// Tilde shows p relative to home as ~/….
func Tilde(home, p string) string {
	if p == home {
		return "~"
	}
	if within(home, p) {
		return "~/" + strings.TrimPrefix(p, home+string(filepath.Separator))
	}
	return p
}

// TildeAll maps Tilde over ps.
func TildeAll(home string, ps []string) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, Tilde(home, p))
	}
	return out
}

// ExistingRoots keeps the roots that exist (as given, not resolved).
func ExistingRoots(roots []string) []string {
	var out []string
	for _, r := range roots {
		if st, err := os.Stat(r); err == nil && st.IsDir() {
			out = append(out, r)
		}
	}
	return out
}

// newSessionRoots is ~/agent-briefs plus the configured project roots and pinned folders.
func (e Env) newSessionRoots() []string {
	return append(append([]string{e.BriefsRoot}, e.Roots...), e.Pinned...)
}

// ProjectFolder is a sub-folder of a project root, offered by the picker.
type ProjectFolder struct {
	Dir     string    `json:"dir"`
	Name    string    `json:"name"`
	Updated time.Time `json:"updated"`
}

// maxProjects bounds the project folders a heartbeat lists.
const maxProjects = 16

// projects lists the pinned folders that exist, then the newest sub-folders
// of the project roots.
func (s *Service) projects() []ProjectFolder {
	var pinned, out []ProjectFolder
	seen := map[string]bool{}
	for _, dir := range ExistingRoots(s.Env.Pinned) {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		pinned = append(pinned, ProjectFolder{Dir: dir, Name: filepath.Base(dir)})
	}
	for _, root := range ExistingRoots(s.Env.Roots) {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || seen[filepath.Join(root, e.Name())] {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, ProjectFolder{Dir: filepath.Join(root, e.Name()), Name: e.Name(), Updated: info.ModTime().UTC()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	out = append(pinned, out...)
	if len(out) > maxProjects {
		out = out[:maxProjects]
	}
	return out
}
