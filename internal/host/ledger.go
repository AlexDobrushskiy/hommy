package host

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// ledgerKeep is how long a finished job's result is kept for re-acks.
const ledgerKeep = 30 * 24 * time.Hour

// ledgerEntry is one line: a finished job's result, or (Begun) the start of
// an at-most-once job, optionally with the process group it runs in.
type ledgerEntry struct {
	EventID string       `json:"event_id"`
	At      time.Time    `json:"at"`
	Result  proto.Result `json:"result"`
	Begun   bool         `json:"begun,omitempty"`
	PGID    int          `json:"pgid,omitempty"`
}

// Ledger is the host's durable record of finished jobs, keyed by event_id.
// A job re-pushed after it finished gets its stored result back instead of
// running again. Append-only JSON lines, fsynced per entry.
type Ledger struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	broken  bool // the last write failed: reopen before the next
	entries map[string]ledgerEntry
	orphans []ledgerEntry // begun by an earlier process, never finished
}

// OpenLedger loads path (creating it) and drops entries older than ledgerKeep.
func OpenLedger(path string, now time.Time) (*Ledger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &Ledger{path: path, entries: map[string]ledgerEntry{}}
	begun := map[string]ledgerEntry{}
	var order []string
	stale := 0
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			var e ledgerEntry
			if json.Unmarshal(sc.Bytes(), &e) != nil || e.EventID == "" {
				stale++ // a torn last line from a crash mid-write
				continue
			}
			if e.Begun {
				stale++ // markers are dropped by the rewrite below
				if _, finished := l.entries[e.EventID]; finished {
					continue
				}
				if _, seen := begun[e.EventID]; !seen {
					order = append(order, e.EventID)
				}
				if prev, ok := begun[e.EventID]; ok && e.PGID == 0 {
					e.PGID = prev.PGID
				}
				begun[e.EventID] = e
				continue
			}
			delete(begun, e.EventID)
			if now.Sub(e.At) > ledgerKeep {
				stale++
				continue
			}
			l.entries[e.EventID] = e
		}
		for _, id := range order {
			if e, ok := begun[id]; ok {
				l.orphans = append(l.orphans, e)
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("read ledger: %w", err)
		}
	}
	if stale > 0 {
		if err := l.rewrite(); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	l.f = f
	return l, nil
}

func (l *Ledger) rewrite() error {
	tmp := l.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, e := range l.entries {
		b, _ := json.Marshal(e)
		w.Write(append(b, '\n'))
	}
	// Unfinished runs keep their marker until their "interrupted" result is written.
	for _, e := range l.orphans {
		b, _ := json.Marshal(e)
		w.Write(append(b, '\n'))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, l.path)
}

// Orphans are the jobs an earlier process began and never finished.
func (l *Ledger) Orphans() []ledgerEntry { return l.orphans }

// Begin durably records that an at-most-once job is starting.
func (l *Ledger) Begin(eventID string, now time.Time) error {
	return l.append(ledgerEntry{EventID: eventID, At: now, Begun: true})
}

// SetPGID records the process group a begun job runs in (killed if the host
// dies before the job finishes).
func (l *Ledger) SetPGID(eventID string, pgid int, now time.Time) error {
	return l.append(ledgerEntry{EventID: eventID, At: now, Begun: true, PGID: pgid})
}

// Get returns the stored result for eventID.
func (l *Ledger) Get(eventID string) (proto.Result, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[eventID]
	return e.Result, ok
}

// Put records a finished job durably (fsync) before it is acked to the hub.
func (l *Ledger) Put(eventID string, r proto.Result, now time.Time) error {
	return l.append(ledgerEntry{EventID: eventID, At: now, Result: r})
}

func (l *Ledger) append(e ledgerEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.broken {
		if err := l.reopen(); err != nil {
			return err
		}
	}
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		l.broken = true
		return err
	}
	if err := l.f.Sync(); err != nil {
		l.broken = true
		return err
	}
	if !e.Begun {
		l.entries[e.EventID] = e
	}
	return nil
}

// reopen replaces the file handle after a failed write, ending any torn line
// so the next entry starts on a line of its own.
func (l *Ledger) reopen() error {
	l.f.Close()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, st.Size()-1); err != nil || last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				f.Close()
				return err
			}
		}
	}
	l.f, l.broken = f, false
	return nil
}

// Close closes the ledger file.
func (l *Ledger) Close() error { return l.f.Close() }
