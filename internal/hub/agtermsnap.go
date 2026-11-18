package hub

import (
	"encoding/json"
	"strings"
	"time"
)

// agtermSnap is the agterm part of a host heartbeat (proto.Heartbeat.Agterm),
// as phase 04's host reports it (internal/agterm Snapshot). Decoded here so
// the hub depends on the wire shape only.
type agtermSnap struct {
	Sessions []agtermSession `json:"sessions"`
	Folders  []agtermFolder  `json:"folders"`
	Home     string          `json:"home,omitempty"`
	Roots    []string        `json:"roots,omitempty"` // where new sessions may start (~ form)
	Projects []agtermFolder  `json:"projects,omitempty"`
	Error    string          `json:"error,omitempty"`
}

type agtermSession struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Workspace string `json:"workspace"`
	Cwd       string `json:"cwd,omitempty"`
	Title     string `json:"title,omitempty"`
	Agent     string `json:"agent,omitempty"` // claude | codex | "" (shell / other program)
	State     string `json:"state,omitempty"` // ready | draft | menu | working | unknown
	BriefDir  string `json:"brief_dir,omitempty"`
	Unacked   int    `json:"unacked,omitempty"` // inbox items the agent hasn't acked
}

type agtermFolder struct {
	Dir         string    `json:"dir"`
	Name        string    `json:"name"`
	ActiveAgent string    `json:"active_agent,omitempty"`
	Live        bool      `json:"live"`
	Status      string    `json:"status,omitempty"`
	Paused      bool      `json:"paused,omitempty"`
	Unacked     int       `json:"unacked,omitempty"`
	Updated     time.Time `json:"updated"`
}

// snapshot is the host's latest agterm snapshot; false when the host reports
// none (no agterm data, or agterm couldn't be read).
func (h *Hub) snapshot(host string) (agtermSnap, bool) {
	var s agtermSnap
	row, err := h.st.Host(host)
	if err != nil || len(row.Heartbeat.Agterm) == 0 {
		return s, false
	}
	if json.Unmarshal(row.Heartbeat.Agterm, &s) != nil || s.Error != "" {
		return s, false
	}
	return s, true
}

// findSession resolves an agterm id or id prefix the way the host does
// (agterm.FindSession): an exact id, else the one session it prefixes.
// matches is how many sessions the id names (0 = none).
func findSession(sessions []agtermSession, id string) (s agtermSession, matches int) {
	needle := strings.ToLower(strings.TrimSpace(id))
	if needle == "" {
		return s, 0
	}
	for _, c := range sessions {
		sid := strings.ToLower(c.ID)
		if sid == needle {
			return c, 1
		}
		if strings.HasPrefix(sid, needle) {
			s = c
			matches++
		}
	}
	return s, matches
}

// liveSession finds a session target (full id or unique prefix) in its
// host's latest snapshot.
func (h *Hub) liveSession(t Target) (agtermSession, bool) {
	snap, ok := h.snapshot(t.Host)
	if !ok {
		return agtermSession{}, false
	}
	s, n := findSession(snap.Sessions, t.Dir)
	return s, n == 1
}

// sessionByName finds an agent session on host by its label name (the
// `🤖 name@host` header fallback).
func (h *Hub) sessionByName(host, name string) (agtermSession, bool) {
	snap, ok := h.snapshot(host)
	if !ok {
		return agtermSession{}, false
	}
	for _, s := range snap.Sessions {
		if s.Agent != "" && sessionName(s) == name {
			return s, true
		}
	}
	return agtermSession{}, false
}
