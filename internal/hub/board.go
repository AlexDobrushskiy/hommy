package hub

// The board (menu button "Agents"): host health, then every live agent
// session grouped needs-you first (at a prompt, unacked replies, buttons not
// answered), busy, idle.

import (
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

const (
	boardPingWindow    = 24 * time.Hour // pings older than this don't count on the board
	boardWaitingWindow = 6 * time.Hour  // a ⏸ hook report older than this is stale
)

type boardHost struct {
	Name     string `json:"name"`
	Online   bool   `json:"online"`
	SeenAgo  int64  `json:"seen_ago_s"` // -1 = never seen
	LastSeen int64  `json:"last_seen"`  // unix seconds, 0 = never
	Version  string `json:"version,omitempty"`
	Agterm   bool   `json:"agterm"`   // the host reports sessions
	Sessions int    `json:"sessions"` // live agent sessions
	Queue    int    `json:"queue"`    // jobs waiting for it
}

type boardSession struct {
	Target   string   `json:"target"`
	Label    string   `json:"label"`
	Name     string   `json:"name"`
	Host     string   `json:"host"`
	Agent    string   `json:"agent"`
	State    string   `json:"state"`
	Group    string   `json:"group"`             // need | busy | idle
	Reasons  []string `json:"reasons,omitempty"` // why it needs the owner
	Waiting  bool     `json:"waiting,omitempty"` // at a permission / input prompt
	Unacked  int      `json:"unacked"`
	Open     int      `json:"open_buttons"`      // pings with buttons not answered
	Buttons  []string `json:"buttons,omitempty"` // the newest such ping's buttons
	Link     string   `json:"link,omitempty"`
	Cwd      string   `json:"cwd,omitempty"`
	BriefDir string   `json:"brief_dir,omitempty"`
	Paused   bool     `json:"paused,omitempty"`
	Status   string   `json:"status,omitempty"` // the brief's state.json status
	Last     string   `json:"last,omitempty"`   // what to show under the name
	LastAt   int64    `json:"last_at,omitempty"`
}

type boardResp struct {
	Hosts    []boardHost    `json:"hosts"`
	Sessions []boardSession `json:"sessions"`
	Now      int64          `json:"now"`
}

var groupOrder = map[string]int{"need": 0, "busy": 1, "idle": 2}

func (h *Hub) boardAPI(chatID int64) any {
	now := h.now()
	resp := boardResp{Hosts: []boardHost{}, Sessions: []boardSession{}, Now: now.Unix()}
	pings, err := h.st.RecentPings(chatID, now.Add(-boardPingWindow))
	if err != nil {
		log.Printf("board pings: %v", err)
	}
	hooks, err := h.st.RecentHookReports(now.Add(-boardWaitingWindow))
	if err != nil {
		log.Printf("board hooks: %v", err)
	}
	for _, name := range h.hostNames() {
		row, _ := h.st.Host(name)
		bh := boardHost{Name: name, Online: h.connected(name), SeenAgo: -1, Version: row.Version}
		bh.Queue, _ = h.st.QueueDepth(name)
		if !row.LastSeen.IsZero() {
			bh.SeenAgo = int64(now.Sub(row.LastSeen) / time.Second)
			bh.LastSeen = row.LastSeen.Unix()
		}
		snap, ok := h.snapshot(name)
		bh.Agterm = ok
		if ok && bh.Online {
			links := h.hookLinks(name)
			for _, s := range snap.Sessions {
				if s.Agent == "" {
					continue
				}
				bh.Sessions++
				resp.Sessions = append(resp.Sessions, h.boardSession(name, s, snap.Folders, links, pings, hooks))
			}
		}
		resp.Hosts = append(resp.Hosts, bh)
	}
	sort.SliceStable(resp.Sessions, func(i, j int) bool {
		a, b := resp.Sessions[i], resp.Sessions[j]
		if groupOrder[a.Group] != groupOrder[b.Group] {
			return groupOrder[a.Group] < groupOrder[b.Group]
		}
		if a.LastAt != b.LastAt {
			return a.LastAt > b.LastAt
		}
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		return a.Name < b.Name
	})
	return resp
}

func (h *Hub) boardSession(host string, s agtermSession, folders []agtermFolder, links map[string]string,
	pings map[string]*SessionPings, hooks map[string]hookReports) boardSession {
	id := strings.ToUpper(s.ID)
	t := Target{Kind: KindSession, Host: host, Dir: id}
	b := boardSession{Target: t.String(), Label: sessionName(s) + " · " + host, Name: sessionName(s), Host: host,
		Agent: s.Agent, State: s.State, Unacked: s.Unacked, Link: links[id], Cwd: s.Cwd, BriefDir: s.BriefDir}
	if f, ok := folderOf(s.BriefDir, folders); ok {
		b.Paused, b.Status = f.Paused, f.Status
	}
	if p := pings[host+":"+id]; p != nil {
		b.Last, b.LastAt = p.Last, p.LastAt.Unix()
		b.Open, b.Buttons = p.Open, p.Buttons
	}
	hr := hooks[t.String()]
	switch {
	case s.State == "menu":
		b.Waiting = true
		b.Reasons = append(b.Reasons, "at a prompt")
	case hr.Waiting.After(hr.Answer) && s.State != "working":
		b.Waiting = true
		b.Reasons = append(b.Reasons, "waiting at the Mac")
		if hr.Waiting.Unix() > b.LastAt {
			b.LastAt = hr.Waiting.Unix()
		}
	}
	if b.Unacked > 0 {
		b.Reasons = append(b.Reasons, plural(b.Unacked, "unacked reply", "unacked replies"))
	}
	if b.Open > 0 {
		b.Reasons = append(b.Reasons, "buttons: "+strings.Join(b.Buttons, " · "))
	}
	switch {
	case len(b.Reasons) > 0:
		b.Group = "need"
	case s.State == "working":
		b.Group = "busy"
	default:
		b.Group = "idle"
	}
	return b
}

// folderOf finds a session's brief folder in the heartbeat's folder list.
func folderOf(briefDir string, folders []agtermFolder) (agtermFolder, bool) {
	if briefDir == "" {
		return agtermFolder{}, false
	}
	for _, f := range folders {
		if f.Dir == briefDir || f.Name == filepath.Base(briefDir) {
			return f, true
		}
	}
	return agtermFolder{}, false
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// hookReports are a session's newest ⏸ (waiting) still in effect and its
// newest answer. A ⏸ whose end (hook_resumed) is stored doesn't count.
type hookReports struct {
	Waiting, Answer time.Time
}

// RecentHookReports maps session targets to their newest hook reports since `since`.
func (s *Store) RecentHookReports(since time.Time) (map[string]hookReports, error) {
	rows, err := s.db.Query(`SELECT target, type, created_at, event_id FROM jobs
		WHERE type IN (?, ?, ?) AND target != '' AND created_at >= ?`,
		proto.EventHookWaiting, proto.EventHookAnswer, proto.EventHookResumed, ts(since))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type wait struct {
		at    time.Time
		event string
	}
	waits := map[string]wait{}
	ended := map[string]bool{}
	out := map[string]hookReports{}
	for rows.Next() {
		var target, typ, event string
		var at float64
		if err := rows.Scan(&target, &typ, &at, &event); err != nil {
			return nil, err
		}
		t := time.Unix(0, int64(at*1e9))
		switch typ {
		case proto.EventHookWaiting:
			if t.After(waits[target].at) {
				waits[target] = wait{t, event}
			}
		case proto.EventHookAnswer:
			if r := out[target]; t.After(r.Answer) {
				r.Answer = t
				out[target] = r
			}
		case proto.EventHookResumed:
			ended[event] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for target, w := range waits {
		if !ended[w.event+proto.ResumedSuffix] {
			r := out[target]
			r.Waiting = w.at
			out[target] = r
		}
	}
	return out, nil
}
