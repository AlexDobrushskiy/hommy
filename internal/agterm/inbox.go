package agterm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// InboxRecord is one line of a session's inbox.jsonl: the full text the owner sent,
// written before any typing so nothing is lost when the composer is busy or the
// typed line is capped.
type InboxRecord struct {
	ID      string  `json:"id"` // tg-<telegram message id> (reactions/buttons: tg-<msg>-r<n> / -b<n>)
	EventID string  `json:"event_id,omitempty"`
	MsgID   int64   `json:"msg_id,omitempty"` // the message an ack (🫡) goes on`
	TS      float64 `json:"ts"`
	From    string  `json:"from"`
	Via     string  `json:"via"`
	Kind    string  `json:"kind"`
	Host    string  `json:"host,omitempty"`
	Session string  `json:"session,omitempty"`
	Text    string  `json:"text"`
	Quote   string  `json:"quote,omitempty"`
	ReplyTo int64   `json:"reply_to,omitempty"`
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// itemID is an inbox item's id: the one given, else tg-<msgID>.
func itemID(id string, msgID int64) string {
	if id != "" {
		return id
	}
	return fmt.Sprintf("tg-%d", msgID)
}

// InboxPath is <brief dir>/inbox.jsonl, else the per-session fallback file.
func (e Env) InboxPath(briefDir, sessionID string) string {
	if briefDir != "" {
		return filepath.Join(briefDir, "inbox.jsonl")
	}
	name := unsafeName.ReplaceAllString(sessionID, "_")
	if name == "" {
		name = "unknown-session"
	}
	return filepath.Join(e.FallbackInbox, name+".jsonl")
}

// AppendInbox appends rec (fsynced) unless a record with the same event id (or,
// without one, the same id) is already there. Returns whether it appended.
func AppendInbox(path string, rec InboxRecord) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var seen struct {
			ID      string `json:"id"`
			EventID string `json:"event_id"`
		}
		if json.Unmarshal(sc.Bytes(), &seen) != nil {
			continue
		}
		if (rec.EventID != "" && seen.EventID == rec.EventID) || (rec.EventID == "" && seen.EventID == "" && seen.ID == rec.ID) {
			return false, nil
		}
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return false, err
	}
	if err := f.Sync(); err != nil {
		return false, err
	}
	return true, nil
}

// AckedPath is the inbox's ack file: inbox.jsonl → inbox.acked.jsonl.
func AckedPath(inbox string) string { return strings.TrimSuffix(inbox, ".jsonl") + ".acked.jsonl" }

// Acked reports whether the inbox's ack file has item id.
func Acked(inbox, id string) bool {
	found := false
	eachLine(AckedPath(inbox), func(b []byte) {
		var a struct {
			ID string `json:"id"`
		}
		if !found && json.Unmarshal(b, &a) == nil && a.ID == id {
			found = true
		}
	})
	return found
}

// Unacked counts the inbox's items that have no line in its ack file (a
// missing inbox has none).
func Unacked(inbox string) int {
	acked := map[string]bool{}
	eachLine(AckedPath(inbox), func(b []byte) {
		var a struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(b, &a) == nil && a.ID != "" {
			acked[a.ID] = true
		}
	})
	n, seen := 0, map[string]bool{}
	eachLine(inbox, func(b []byte) {
		var r struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(b, &r) == nil && r.ID != "" && !acked[r.ID] && !seen[r.ID] {
			seen[r.ID] = true
			n++
		}
	})
	return n
}

func eachLine(path string, fn func([]byte)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		fn(sc.Bytes())
	}
}
