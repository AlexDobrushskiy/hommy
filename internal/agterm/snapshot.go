package agterm

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SessionInfo is one agterm session in the host heartbeat.
type SessionInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Workspace string `json:"workspace"`
	Cwd       string `json:"cwd,omitempty"`
	Title     string `json:"title,omitempty"`
	Agent     Agent  `json:"agent,omitempty"` // claude | codex | "" (shell / other program)
	State     State  `json:"state,omitempty"`
	BriefDir  string `json:"brief_dir,omitempty"`
	Unacked   int    `json:"unacked,omitempty"` // inbox items the agent hasn't acked
}

// AgentFolder is one brief folder under ~/agent-briefs.
type AgentFolder struct {
	Dir         string    `json:"dir"`
	Name        string    `json:"name"`
	ActiveAgent string    `json:"active_agent,omitempty"`
	Live        bool      `json:"live"`
	Status      string    `json:"status,omitempty"`
	Paused      bool      `json:"paused,omitempty"`
	Unacked     int       `json:"unacked,omitempty"`
	Updated     time.Time `json:"updated"`
}

// Snapshot is the agterm part of a host heartbeat.
type Snapshot struct {
	Sessions []SessionInfo `json:"sessions"`
	Folders  []AgentFolder `json:"folders"`
	// New sessions: the host home, the folders a new session may start in (~ form)
	// and the newest project folders under them.
	Home     string          `json:"home,omitempty"`
	Roots    []string        `json:"roots,omitempty"`
	Projects []ProjectFolder `json:"projects,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// FolderMaxAge bounds which brief folders the heartbeat lists.
const FolderMaxAge = 30 * 24 * time.Hour

// Snapshot reads the agterm tree, classifies every Claude Code / Codex session's
// composer, and lists recent brief folders.
func (s *Service) Snapshot(ctx context.Context) Snapshot {
	var snap Snapshot
	sessions, err := s.Ctl.Sessions(ctx)
	if err != nil {
		snap.Error = err.Error()
	}
	live := map[string]bool{}
	for _, sess := range sessions {
		live[strings.ToUpper(sess.ID)] = true
		info := SessionInfo{ID: sess.ID, Name: sess.Name, Workspace: sess.Workspace, Cwd: sess.Cwd, Title: sess.Title}
		if len(sess.Foreground) > 0 {
			if c, _, err := Observe(ctx, s.Ctl, sess); err == nil {
				info.Agent = c.Agent
				if c.Agent != "" || c.State == Menu { // a dialog can hide which agent it is
					info.State = c.State
				}
			} else {
				info.State = Unknown // unread, not "no agent"
			}
		}
		if info.Agent != "" {
			info.BriefDir = s.briefFor(sess.ID, sess.Cwd)
			info.Unacked = Unacked(s.Env.InboxPath(info.BriefDir, sess.ID))
		}
		snap.Sessions = append(snap.Sessions, info)
	}
	snap.Folders = s.folders(live)
	snap.Home = s.Env.Home
	// ~/agent-briefs counts even before it exists: a new session creates it
	snap.Roots = TildeAll(s.Env.Home, append(append([]string{s.Env.BriefsRoot}, ExistingRoots(s.Env.Roots)...), ExistingRoots(s.Env.Pinned)...))
	snap.Projects = s.projects()
	return snap
}

func (s *Service) folders(live map[string]bool) []AgentFolder {
	entries, _ := os.ReadDir(s.Env.BriefsRoot)
	cutoff := s.now().Add(-FolderMaxAge)
	var out []AgentFolder
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(s.Env.BriefsRoot, e.Name())
		st, err := os.Stat(filepath.Join(dir, "state.json"))
		if err != nil || st.ModTime().Before(cutoff) {
			continue
		}
		f := AgentFolder{Dir: dir, Name: e.Name(), ActiveAgent: ActiveAgent(dir), Updated: st.ModTime().UTC(), Status: stateStatus(dir),
			Unacked: Unacked(filepath.Join(dir, "inbox.jsonl"))}
		f.Live = f.ActiveAgent != "" && live[strings.ToUpper(f.ActiveAgent)]
		if _, err := os.Stat(filepath.Join(dir, "PAUSE")); err == nil {
			f.Paused = true
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// stateStatus reads state.json's top-level "status" when it is a string.
func stateStatus(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return ""
	}
	obj, err := parseOrdered(data)
	if err != nil {
		return ""
	}
	raw, ok := obj.vals["status"]
	if !ok || len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	return strings.Trim(string(raw), `"`)
}
