package agterm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// spawnRecord is how far a new_session job got, by event id, so a job the hub
// re-pushes after a host restart resumes it instead of starting a second session.
type spawnRecord struct {
	BriefDir string            `json:"brief_dir"`
	Session  string            `json:"session,omitempty"`
	Cwd      string            `json:"cwd"`
	Slug     string            `json:"slug"`
	Typing   bool              `json:"typing,omitempty"` // the composer was ready and typing the first message began
	Result   *NewSessionResult `json:"result,omitempty"` // set once the job finished
	At       time.Time         `json:"at"`
}

const spawnKeep = 7 * 24 * time.Hour

type spawnLog struct {
	path string
	mu   sync.Mutex
}

func (l *spawnLog) load() map[string]spawnRecord {
	m := map[string]spawnRecord{}
	if data, err := os.ReadFile(l.path); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

func (l *spawnLog) get(eventID string) (spawnRecord, bool) {
	if eventID == "" {
		return spawnRecord{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.load()[eventID]
	return r, ok
}

// put stores rec under eventID (fsynced), dropping records older than a week.
func (l *spawnLog) put(eventID string, rec spawnRecord, now time.Time) error {
	if eventID == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.load()
	for id, r := range m {
		if now.Sub(r.At) > spawnKeep {
			delete(m, id)
		}
	}
	rec.At = now
	m[eventID] = rec
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	return WriteFileAtomic(l.path, data, 0o600)
}
