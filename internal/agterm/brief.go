package agterm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Env holds the filesystem locations the jobs use; tests point them at temp dirs.
type Env struct {
	Home          string
	BriefsRoot    string   // AGENT_BRIEFS_DIR
	FallbackInbox string   // <state>/inbox (sessions with no brief folder)
	StateDir      string   // host-private state (pending typing retries)
	Roots         []string // project roots new sessions may use besides BriefsRoot (absolute)
	Pinned        []string // folders offered first and allowed as they are (absolute)
	Host          string
	Now           func() time.Time
}

// DefaultEnv uses the real home directory. stateDir is the host's own data dir.
func DefaultEnv(host, stateDir string) (Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Env{}, err
	}
	briefs := os.Getenv("AGENT_BRIEFS_DIR")
	if briefs == "" {
		briefs = filepath.Join(home, ".local/share/hommy/briefs")
	}
	inbox := filepath.Join(home, ".local/state/hommy/inbox")
	if d := os.Getenv("HOST_DATA_DIR"); d != "" {
		inbox = FallbackInboxFor(d)
	}
	return Env{
		Home:          home,
		BriefsRoot:    briefs,
		FallbackInbox: inbox,
		StateDir:      stateDir,
		Roots:         ParseRoots(os.Getenv("NEW_SESSION_ROOTS"), home),
		Pinned:        ParsePinned(os.Getenv("NEW_SESSION_PINNED"), home),
		Host:          host,
		Now:           time.Now,
	}, nil
}

// FallbackInboxFor is <state>/inbox for a host data dir <state>/host.
func FallbackInboxFor(hostDataDir string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(hostDataDir)), "inbox")
}

// activeKeys name the session that owns a brief folder in its state.json.
var activeKeys = []string{"active_agent", "active_watcher", "overseer"}

// ActiveAgent reads the owning agterm session id from <dir>/state.json: the
// first owner key set (the successor a handover names).
func ActiveAgent(dir string) string {
	if owners := folderOwners(dir); len(owners) > 0 {
		return owners[0]
	}
	return ""
}

// folderOwners lists every session id state.json names under an owner key, in
// activeKeys order.
func folderOwners(dir string) []string {
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil
	}
	var st map[string]json.RawMessage
	if json.Unmarshal(data, &st) != nil {
		return nil
	}
	var out []string
	for _, k := range activeKeys {
		var v string
		if raw, ok := st[k]; ok && json.Unmarshal(raw, &v) == nil && v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ownedBy: state.json names id under any owner key.
func ownedBy(dir, id string) bool {
	for _, o := range folderOwners(dir) {
		if strings.EqualFold(o, id) {
			return true
		}
	}
	return false
}

// handedOver reports whether <dir>/state.json lists id in handover_from (a
// string or a list of strings).
func handedOver(dir, id string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return false
	}
	var st struct {
		From json.RawMessage `json:"handover_from"`
	}
	if json.Unmarshal(data, &st) != nil || len(st.From) == 0 {
		return false
	}
	var one string
	if json.Unmarshal(st.From, &one) == nil {
		return strings.EqualFold(one, id)
	}
	var many []string
	if json.Unmarshal(st.From, &many) == nil {
		for _, m := range many {
			if strings.EqualFold(m, id) {
				return true
			}
		}
	}
	return false
}

// BriefDirFor finds a session's brief folder: the newest ~/agent-briefs/*/state.json
// naming it as its active agent (or in handover_from), else the brief folder its
// cwd is inside.
func (e Env) BriefDirFor(sessionID, cwd string) string {
	best, bestTime := "", time.Time{}
	entries, _ := os.ReadDir(e.BriefsRoot)
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		dir := filepath.Join(e.BriefsRoot, ent.Name())
		if sessionID == "" || (!ownedBy(dir, sessionID) && !handedOver(dir, sessionID)) {
			continue
		}
		st, err := os.Stat(filepath.Join(dir, "state.json"))
		if err == nil && st.ModTime().After(bestTime) {
			best, bestTime = dir, st.ModTime()
		}
	}
	if best != "" {
		return best
	}
	if cwd == "" {
		return ""
	}
	root, err := filepath.EvalSymlinks(e.BriefsRoot)
	if err != nil {
		return ""
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.Join(e.BriefsRoot, strings.Split(rel, string(filepath.Separator))[0])
}

// ErrPathRefused: a path escapes the folder it must stay in.
var ErrPathRefused = errors.New("path refused: it must stay inside the brief folder")

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ValidBriefDir returns the brief folder's real path; it must be a direct child
// of BriefsRoot after symlinks are resolved.
func (e Env) ValidBriefDir(dir string) (string, error) {
	root, err := filepath.EvalSymlinks(e.BriefsRoot)
	if err != nil {
		return "", fmt.Errorf("briefs root: %w", err)
	}
	real, err := filepath.EvalSymlinks(e.Expand(dir))
	if err != nil {
		return "", fmt.Errorf("brief folder: %w", err)
	}
	if filepath.Dir(real) != root {
		return "", ErrPathRefused
	}
	st, err := os.Stat(real)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("brief folder %s is not a directory", dir)
	}
	return real, nil
}

// SafeJoin resolves rel under dir, following symlinks, and refuses anything that
// lands outside dir. The target must exist unless mustExist is false, in which
// case its parent must.
func SafeJoin(dir, rel string, mustExist bool) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.ContainsRune(rel, 0) {
		return "", ErrPathRefused
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(root, rel)
	if !within(root, joined) {
		return "", ErrPathRefused
	}
	real, err := filepath.EvalSymlinks(joined)
	if err != nil {
		if mustExist || !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent, perr := filepath.EvalSymlinks(filepath.Dir(joined))
		if perr != nil {
			return "", perr
		}
		real = filepath.Join(parent, filepath.Base(joined))
	}
	if !within(root, real) || real == root {
		return "", ErrPathRefused
	}
	return real, nil
}

// Expand turns a leading ~ into the home directory.
func (e Env) Expand(p string) string {
	if p == "~" {
		return e.Home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(e.Home, p[2:])
	}
	return p
}

// orderedObject is a JSON object that keeps its key order across a rewrite, so
// editing an agent's state.json leaves the rest of it as the agent wrote it.
type orderedObject struct {
	keys []string
	vals map[string]json.RawMessage
}

func parseOrdered(data []byte) (*orderedObject, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("state.json is not a JSON object")
	}
	o := &orderedObject{vals: map[string]json.RawMessage{}}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := kt.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if _, seen := o.vals[key]; !seen {
			o.keys = append(o.keys, key)
		}
		o.vals[key] = raw
	}
	return o, nil
}

func (o *orderedObject) set(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = raw
	return nil
}

func (o *orderedObject) marshal() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, k := range o.keys {
		kb, _ := json.Marshal(k)
		var vb bytes.Buffer
		if err := json.Indent(&vb, o.vals[k], "  ", "  "); err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "  %s: %s", kb, vb.Bytes())
		if i < len(o.keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.Bytes(), nil
}

// UpdateState sets keys in <dir>/state.json (created if missing), keeping the
// order and content of everything else.
func UpdateState(dir string, set map[string]any, order []string) error {
	path := filepath.Join(dir, "state.json")
	obj := &orderedObject{vals: map[string]json.RawMessage{}}
	if data, err := os.ReadFile(path); err == nil {
		if obj, err = parseOrdered(data); err != nil {
			return fmt.Errorf("state.json: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, k := range order {
		if err := obj.set(k, set[k]); err != nil {
			return err
		}
	}
	data, err := obj.marshal()
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o644)
}

// WriteFileAtomic writes via a synced temp file and rename.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
