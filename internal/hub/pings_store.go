package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Agent pings (phase 07): each message an agent sent, with who sent it, so
// replies, reactions and buttons on it route back to the agent's session.
const pingsSchema = `
CREATE TABLE IF NOT EXISTS pings(
  chat_id    INTEGER NOT NULL,
  msg_id     INTEGER NOT NULL,
  slug       TEXT NOT NULL,
  host       TEXT NOT NULL,
  session    TEXT NOT NULL DEFAULT '',
  brief_dir  TEXT NOT NULL DEFAULT '',
  text       TEXT NOT NULL,
  body       TEXT NOT NULL DEFAULT '',
  buttons    TEXT NOT NULL DEFAULT '',
  created_at REAL NOT NULL,
  PRIMARY KEY(chat_id, msg_id));
`

// Ping is one message an agent sent.
type Ping struct {
	ChatID   int64
	MsgID    int
	Slug     string
	Host     string
	Session  string // agterm session id ("" = unknown)
	BriefDir string
	Text     string   // the message as sent (header included)
	Body     string   // what the agent wrote (for quotes)
	Buttons  []string // inline button labels (on this message)
}

// migratePings adds answered_at (phase 09: the board's "buttons not answered
// yet") to an older pings table.
func migratePings(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pings') WHERE name='answered_at'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE pings ADD COLUMN answered_at REAL`); err != nil {
			return err
		}
	}
	_, err := db.Exec(`CREATE INDEX IF NOT EXISTS pings_recent ON pings(created_at)`)
	return err
}

// MarkPingAnswered records that the owner answered an agent message (a button, a
// reply or a reaction); a message that isn't a ping is ignored.
func (s *Store) MarkPingAnswered(chatID int64, msgID int, now time.Time) error {
	_, err := s.db.Exec(`UPDATE pings SET answered_at=? WHERE chat_id=? AND msg_id=? AND answered_at IS NULL`, ts(now), chatID, msgID)
	return err
}

// SessionPings is what the board knows about one session's recent pings.
type SessionPings struct {
	Last     string    // the newest ping's body
	LastAt   time.Time // when it was sent
	Open     int       // pings with buttons the owner hasn't answered
	OpenText string    // the newest such ping's body
	Buttons  []string  // and its buttons
}

// RecentPings groups the pings sent since `since` by "<host>:<SESSION>".
func (s *Store) RecentPings(chatID int64, since time.Time) (map[string]*SessionPings, error) {
	rows, err := s.db.Query(`SELECT host, session, body, buttons, answered_at IS NULL, created_at FROM pings
		WHERE chat_id=? AND created_at >= ? AND session != '' ORDER BY created_at`, chatID, ts(since))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]*SessionPings{}
	for rows.Next() {
		var host, session, body, buttons string
		var open bool
		var at float64
		if err := rows.Scan(&host, &session, &body, &buttons, &open, &at); err != nil {
			return nil, err
		}
		k := host + ":" + strings.ToUpper(session)
		p := out[k]
		if p == nil {
			p = &SessionPings{}
			out[k] = p
		}
		p.Last, p.LastAt = body, fromTS(sql.NullFloat64{Float64: at, Valid: true})
		if buttons != "" && open {
			p.Open++
			p.OpenText, p.Buttons = body, nil
			_ = json.Unmarshal([]byte(buttons), &p.Buttons)
		}
	}
	return out, rows.Err()
}

// AddPing records an agent message (idempotent: the first record wins).
func (s *Store) AddPing(p Ping, now time.Time) error {
	var buttons string
	if len(p.Buttons) > 0 {
		b, _ := json.Marshal(p.Buttons)
		buttons = string(b)
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO pings(chat_id, msg_id, slug, host, session, brief_dir, text, body, buttons, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, p.ChatID, p.MsgID, p.Slug, p.Host, p.Session, p.BriefDir, p.Text, p.Body, buttons, ts(now))
	return err
}

// GetPing returns an agent message (nil when msgID isn't one).
func (s *Store) GetPing(chatID int64, msgID int) (*Ping, error) {
	p := Ping{ChatID: chatID, MsgID: msgID}
	var buttons string
	err := s.db.QueryRow(`SELECT slug, host, session, brief_dir, text, body, buttons FROM pings WHERE chat_id=? AND msg_id=?`, chatID, msgID).
		Scan(&p.Slug, &p.Host, &p.Session, &p.BriefDir, &p.Text, &p.Body, &buttons)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if buttons != "" {
		_ = json.Unmarshal([]byte(buttons), &p.Buttons)
	}
	return &p, nil
}
