package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// Job statuses. A lane (one target) has at most one job in pushed/started/result.
const (
	JobQueued  = "queued"  // stored, not yet pushed
	JobPushed  = "pushed"  // sent to the host at least once
	JobStarted = "started" // the host reported it running
	JobResult  = "result"  // result stored, answer being delivered
	JobDone    = "done"    // answer delivered
)

// Event statuses.
const (
	EventReceived = "received"
	EventDone     = "done"
	EventIgnored  = "ignored"
	EventFailed   = "failed"
	EventParked   = "parked" // waits for a spooled agent message it is about (agents.go)
)

const schema = `
CREATE TABLE IF NOT EXISTS events(
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id   TEXT UNIQUE NOT NULL,
  update_id  INTEGER NOT NULL,
  chat_id    INTEGER NOT NULL,
  msg_id     INTEGER NOT NULL,
  kind       TEXT NOT NULL,
  raw        TEXT NOT NULL,
  status     TEXT NOT NULL,
  error      TEXT NOT NULL DEFAULT '',
  created_at REAL NOT NULL,
  updated_at REAL NOT NULL);
CREATE INDEX IF NOT EXISTS events_status ON events(status, seq);
CREATE TABLE IF NOT EXISTS jobs(
  id          TEXT PRIMARY KEY,
  event_id    TEXT UNIQUE NOT NULL,
  seq         INTEGER NOT NULL,
  sub         INTEGER NOT NULL DEFAULT 0,
  host        TEXT NOT NULL,
  lane        TEXT NOT NULL,
  target      TEXT NOT NULL,
  type        TEXT NOT NULL,
  payload     BLOB NOT NULL,
  chat_id     INTEGER NOT NULL,
  msg_id      INTEGER NOT NULL,
  session_gen INTEGER NOT NULL DEFAULT 0,
  chain       TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL,
  result      TEXT NOT NULL DEFAULT '',
  attempts    INTEGER NOT NULL DEFAULT 0,
  delivered   INTEGER NOT NULL DEFAULT 0,
  draft_id    INTEGER NOT NULL DEFAULT 0,
  stop_requested INTEGER NOT NULL DEFAULT 0,
  created_at  REAL NOT NULL,
  pushed_at   REAL,
  started_at  REAL,
  result_at   REAL,
  done_at     REAL);
CREATE INDEX IF NOT EXISTS jobs_host_status ON jobs(host, status);
CREATE INDEX IF NOT EXISTS jobs_lane ON jobs(lane, status, seq, sub);
CREATE TABLE IF NOT EXISTS done_ledger(
  event_id TEXT PRIMARY KEY,
  job_id   TEXT NOT NULL,
  host     TEXT NOT NULL,
  status   TEXT NOT NULL,
  done_at  REAL NOT NULL);
CREATE TABLE IF NOT EXISTS messages(
  chat_id    INTEGER NOT NULL,
  msg_id     INTEGER NOT NULL,
  direction  TEXT NOT NULL,
  kind       TEXT NOT NULL DEFAULT '',
  target     TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL DEFAULT '',
  job_id     TEXT NOT NULL DEFAULT '',
  created_at REAL NOT NULL,
  PRIMARY KEY(chat_id, msg_id));
CREATE TABLE IF NOT EXISTS sessions(
  target     TEXT NOT NULL,
  gen        INTEGER NOT NULL,
  session_id TEXT NOT NULL DEFAULT '',
  updated_at REAL NOT NULL,
  PRIMARY KEY(target, gen));
CREATE TABLE IF NOT EXISTS chat_targets(
  chat_id    INTEGER PRIMARY KEY,
  target     TEXT NOT NULL,
  updated_at REAL NOT NULL);
CREATE TABLE IF NOT EXISTS hosts(
  name           TEXT PRIMARY KEY,
  last_seen      REAL,
  connected_at   REAL,
  version        TEXT NOT NULL DEFAULT '',
  heartbeat      TEXT NOT NULL DEFAULT '',
  silent_alerted INTEGER NOT NULL DEFAULT 0,
  queued_noticed INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS kv(
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL);
`

// Store is the hub's SQLite state.
type Store struct {
	db *sql.DB
}

// OpenStore opens (creating) the database at path.
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; keeps SQLite locking out of the picture
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := db.Exec(pingsSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("pings schema: %w", err)
	}
	if err := migratePings(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate pings: %w", err)
	}
	if _, err := db.Exec(ownerSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("owner schema: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate adds columns introduced after a database was created.
func migrate(db *sql.DB) error {
	for _, c := range []struct{ name, def string }{
		{"delivered", "INTEGER NOT NULL DEFAULT 0"},
		{"draft_id", "INTEGER NOT NULL DEFAULT 0"},
		{"stop_requested", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name=?`, c.name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE jobs ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
				return err
			}
		}
	}
	_, err := db.Exec(`CREATE INDEX IF NOT EXISTS jobs_draft ON jobs(draft_id) WHERE draft_id != 0`)
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database answers.
func (s *Store) Ping() error {
	var one int
	return s.db.QueryRow("SELECT 1").Scan(&one)
}

func ts(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func fromTS(f sql.NullFloat64) time.Time {
	if !f.Valid {
		return time.Time{}
	}
	sec := int64(f.Float64)
	return time.Unix(sec, int64((f.Float64-float64(sec))*1e9))
}

// ---- events ----

// Event is one authorized Telegram update.
type Event struct {
	Seq      int64
	EventID  string
	UpdateID int64
	ChatID   int64
	MsgID    int
	Kind     string
	Raw      []byte
	Status   string
	Created  float64 // when the hub stored it
	// spawn: for a message to a new@ interactive target, the creation still
	// running when it arrived (it goes to that session, however long its
	// download or transcription takes).
	spawn *JobRow
}

// AddEvent stores an update; false when this event_id was already stored
// (Telegram redelivery after a restart).
func (s *Store) AddEvent(e Event, status string, now time.Time) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO events(event_id, update_id, chat_id, msg_id, kind, raw, status, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, e.EventID, e.UpdateID, e.ChatID, e.MsgID, e.Kind, string(e.Raw), status, ts(now), ts(now))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// kindStop is a draft's stop button: processed on its own lane (NextStop),
// never behind a message that is still being transcribed.
const kindStop = "stopped_message_generation"

// NextEvent returns the oldest message event still to process.
func (s *Store) NextEvent() (*Event, error) { return s.nextEvent(false) }

// NextStop returns the oldest stop event still to process.
func (s *Store) NextStop() (*Event, error) { return s.nextEvent(true) }

func (s *Store) nextEvent(stops bool) (*Event, error) {
	var e Event
	var raw string
	err := s.db.QueryRow(`SELECT seq, event_id, update_id, chat_id, msg_id, kind, raw, status, created_at FROM events
		WHERE status = ? AND (kind = ?) = ? ORDER BY seq LIMIT 1`, EventReceived, kindStop, stops).
		Scan(&e.Seq, &e.EventID, &e.UpdateID, &e.ChatID, &e.MsgID, &e.Kind, &raw, &e.Status, &e.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.Raw = []byte(raw)
	return &e, nil
}

// UnparkEvents puts parked events stored before `before` back in the queue
// (they keep their place: seq order).
func (s *Store) UnparkEvents(before time.Time) error {
	_, err := s.db.Exec(`UPDATE events SET status=? WHERE status=? AND created_at < ?`, EventReceived, EventParked, ts(before))
	return err
}

// FinishEvent sets an event's final status.
func (s *Store) FinishEvent(seq int64, status, errText string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE events SET status=?, error=?, updated_at=? WHERE seq=?`, status, errText, ts(now), seq)
	return err
}

// ---- jobs ----

// JobRow is a stored job.
type JobRow struct {
	ID            string
	EventID       string
	Seq           int64
	Sub           int
	Host          string
	Lane          string
	Target        string
	Type          string
	Payload       json.RawMessage
	ChatID        int64
	MsgID         int
	SessionGen    int
	Chain         string
	Status        string
	Result        string
	Attempts      int
	Delivered     int   // chat messages of the answer already sent (deliveries resume after it)
	DraftID       int64 // run_p: the sendMessageDraft id its stream uses (0 = none)
	StopRequested bool  // the owner pressed stop on its draft
	CreatedAt     time.Time
}

// Proto is the wire form of the job.
func (j *JobRow) Proto() proto.Job {
	return proto.Job{ID: j.ID, EventID: j.EventID, Type: j.Type, Payload: j.Payload}
}

const jobCols = `id, event_id, seq, sub, host, lane, target, type, payload, chat_id, msg_id, session_gen, chain, status, result, attempts, delivered, draft_id, stop_requested, created_at`

func scanJob(sc interface{ Scan(...any) error }) (*JobRow, error) {
	var j JobRow
	var payload []byte
	var created sql.NullFloat64
	var stop int
	if err := sc.Scan(&j.ID, &j.EventID, &j.Seq, &j.Sub, &j.Host, &j.Lane, &j.Target, &j.Type, &payload,
		&j.ChatID, &j.MsgID, &j.SessionGen, &j.Chain, &j.Status, &j.Result, &j.Attempts, &j.Delivered, &j.DraftID, &stop, &created); err != nil {
		return nil, err
	}
	j.StopRequested = stop == 1
	j.Payload = payload
	j.CreatedAt = fromTS(created)
	return &j, nil
}

func (s *Store) queryJobs(q string, args ...any) ([]*JobRow, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*JobRow
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// AddJob stores a queued job; false when its event_id already has a job.
func (s *Store) AddJob(j *JobRow, now time.Time) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO jobs(id, event_id, seq, sub, host, lane, target, type, payload, chat_id, msg_id, session_gen, chain, draft_id, status, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, j.ID, j.EventID, j.Seq, j.Sub, j.Host, j.Lane, j.Target, j.Type, []byte(j.Payload),
		j.ChatID, j.MsgID, j.SessionGen, j.Chain, j.DraftID, JobQueued, ts(now))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	j.Status = JobQueued
	j.CreatedAt = now
	return n == 1, nil
}

// AddDelivery stores something a host reported on its own (a hook answer) as
// a job that already has its result, so the usual delivery (retries, parts
// sent once, resumed after a restart) brings it to the chat. False when its
// event_id was already stored (a re-sent report).
func (s *Store) AddDelivery(j *JobRow, r proto.Result, now time.Time) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO jobs(id, event_id, seq, sub, host, lane, target, type, payload, chat_id, msg_id, status, result, created_at, result_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, j.ID, j.EventID, j.Seq, j.Sub, j.Host, j.Lane, j.Target, j.Type, []byte(j.Payload),
		j.ChatID, j.MsgID, JobResult, mustJSON(r), ts(now), ts(now))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	j.Status, j.Result, j.CreatedAt = JobResult, mustJSON(r), now
	return n == 1, nil
}

// Job returns a job by id (nil when unknown).
func (s *Store) Job(id string) (*JobRow, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// JobByDraft returns the job streaming into draftID (nil when unknown).
func (s *Store) JobByDraft(draftID int64) (*JobRow, error) {
	if draftID == 0 {
		return nil, nil
	}
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE draft_id=?`, draftID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// RequestStop flags a job as stopped from the chat; false when it already
// was, or has finished (its result is in).
func (s *Store) RequestStop(id string) (bool, error) {
	res, err := s.db.Exec(`UPDATE jobs SET stop_requested=1 WHERE id=? AND stop_requested=0 AND status IN (?,?,?)`,
		id, JobQueued, JobPushed, JobStarted)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReadyJobs returns, for each of the host's lanes with nothing in flight, its
// next queued job (lowest seq, sub).
func (s *Store) ReadyJobs(host string) ([]*JobRow, error) {
	return s.queryJobs(`SELECT `+jobCols+` FROM jobs j WHERE host=? AND status=?
		AND NOT EXISTS (SELECT 1 FROM jobs b WHERE b.lane=j.lane AND b.status IN (?,?,?))
		AND NOT EXISTS (SELECT 1 FROM jobs e WHERE e.lane=j.lane AND e.status=? AND (e.seq < j.seq OR (e.seq = j.seq AND e.sub < j.sub)))
		ORDER BY seq, sub`, host, JobQueued, JobPushed, JobStarted, JobResult, JobQueued)
}

// InflightJobs returns the host's pushed or started jobs (re-pushed on reconnect).
func (s *Store) InflightJobs(host string) ([]*JobRow, error) {
	return s.queryJobs(`SELECT `+jobCols+` FROM jobs WHERE host=? AND status IN (?,?) ORDER BY seq, sub`, host, JobPushed, JobStarted)
}

// JobsInStatus returns all jobs in a status, oldest first.
func (s *Store) JobsInStatus(status string) ([]*JobRow, error) {
	return s.queryJobs(`SELECT `+jobCols+` FROM jobs WHERE status=? ORDER BY seq, sub`, status)
}

// OldestWaiting is the oldest not-yet-started job for host (nil if none).
func (s *Store) OldestWaiting(host string) (*JobRow, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE host=? AND status IN (?,?) ORDER BY created_at LIMIT 1`, host, JobQueued, JobPushed))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// QueueDepth counts the host's unfinished jobs.
func (s *Store) QueueDepth(host string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE host=? AND status != ?`, host, JobDone).Scan(&n)
	return n, err
}

// MarkPushed records a push (with the payload as pushed).
func (s *Store) MarkPushed(id string, payload json.RawMessage, now time.Time) error {
	_, err := s.db.Exec(`UPDATE jobs SET status=CASE WHEN status=? THEN ? ELSE status END, payload=?, attempts=attempts+1, pushed_at=? WHERE id=?`,
		JobQueued, JobPushed, []byte(payload), ts(now), id)
	return err
}

// MarkStarted moves a pushed job to started; false if it wasn't pushed.
func (s *Store) MarkStarted(id string, now time.Time) (bool, error) {
	res, err := s.db.Exec(`UPDATE jobs SET status=?, started_at=? WHERE id=? AND status=?`, JobStarted, ts(now), id, JobPushed)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SetResult stores the host's result once, mirrors it into the done ledger
// and, for a run_p, saves the session the run ended on, all in one
// transaction; false when the job already had a result (a duplicate ack).
func (s *Store) SetResult(id string, r proto.Result, now time.Time) (bool, error) {
	b, _ := json.Marshal(r)
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE jobs SET status=?, result=?, result_at=? WHERE id=? AND status IN (?,?,?)`,
		JobResult, string(b), ts(now), id, JobQueued, JobPushed, JobStarted)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO done_ledger(event_id, job_id, host, status, done_at)
		SELECT event_id, id, host, ?, ? FROM jobs WHERE id=?`, r.Status, ts(now), id); err != nil {
		return false, err
	}
	var typ, target string
	var gen int
	if err := tx.QueryRow(`SELECT type, target, session_gen FROM jobs WHERE id=?`, id).Scan(&typ, &target, &gen); err != nil {
		return false, err
	}
	var d proto.RunPResult
	if typ == proto.JobRunP && json.Unmarshal(r.Data, &d) == nil && d.SessionID != "" {
		if _, err := tx.Exec(`INSERT INTO sessions(target, gen, session_id, updated_at) VALUES(?,?,?,?)
			ON CONFLICT(target, gen) DO UPDATE SET session_id=excluded.session_id, updated_at=excluded.updated_at`,
			target, gen, d.SessionID, ts(now)); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// SetDelivered records how many of a job's answer messages have been sent.
func (s *Store) SetDelivered(id string, n int) error {
	_, err := s.db.Exec(`UPDATE jobs SET delivered=? WHERE id=?`, n, id)
	return err
}

// MarkDone records that the job's answer was delivered.
func (s *Store) MarkDone(id string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE jobs SET status=?, done_at=? WHERE id=?`, JobDone, ts(now), id)
	return err
}

// DropPayload empties a finished job's payload (file bytes don't need to live in the DB forever).
func (s *Store) DropPayload(id string) error {
	_, err := s.db.Exec(`UPDATE jobs SET payload='{}' WHERE id=? AND status=?`, id, JobDone)
	return err
}

// DoneLedger reports whether an event's job finished, with its status.
func (s *Store) DoneLedger(eventID string) (string, bool, error) {
	var st string
	err := s.db.QueryRow(`SELECT status FROM done_ledger WHERE event_id=?`, eventID).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return st, err == nil, err
}

// ---- messages ----

// Message links a chat message to where it came from or went.
type Message struct {
	ChatID    int64
	MsgID     int
	Direction string // in | out
	Kind      string
	Target    string
	SessionID string
	JobID     string
}

// AddMessage records a message (idempotent).
func (s *Store) AddMessage(m Message, now time.Time) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO messages(chat_id, msg_id, direction, kind, target, session_id, job_id, created_at)
		VALUES(?,?,?,?,?,?,?,?)`, m.ChatID, m.MsgID, m.Direction, m.Kind, m.Target, m.SessionID, m.JobID, ts(now))
	return err
}

// GetMessage returns a recorded message (nil when unknown).
func (s *Store) GetMessage(chatID int64, msgID int) (*Message, error) {
	m := Message{ChatID: chatID, MsgID: msgID}
	err := s.db.QueryRow(`SELECT direction, kind, target, session_id, job_id FROM messages WHERE chat_id=? AND msg_id=?`, chatID, msgID).
		Scan(&m.Direction, &m.Kind, &m.Target, &m.SessionID, &m.JobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &m, err
}

// ---- -p sessions ----

// CurrentGen is the target's current session generation (0 when never reset).
func (s *Store) CurrentGen(target string) (int, error) {
	var g sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(gen) FROM sessions WHERE target=?`, target).Scan(&g)
	return int(g.Int64), err
}

// SessionID is the -p session id for a target's generation ("" = fresh).
func (s *Store) SessionID(target string, gen int) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT session_id FROM sessions WHERE target=? AND gen=?`, target, gen).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// NewGen starts a fresh -p conversation for target; jobs created before keep
// their own generation.
func (s *Store) NewGen(target string, now time.Time) (int, error) {
	g, err := s.CurrentGen(target)
	if err != nil {
		return 0, err
	}
	_, err = s.db.Exec(`INSERT INTO sessions(target, gen, session_id, updated_at) VALUES(?,?,'',?)`, target, g+1, ts(now))
	return g + 1, err
}

// ---- targets ----

// ChatTarget is the chat's current target ("" when never set).
func (s *Store) ChatTarget(chatID int64) (string, error) {
	var t string
	err := s.db.QueryRow(`SELECT target FROM chat_targets WHERE chat_id=?`, chatID).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return t, err
}

// SetChatTarget makes target the chat's current target.
func (s *Store) SetChatTarget(chatID int64, target string, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO chat_targets(chat_id, target, updated_at) VALUES(?,?,?)
		ON CONFLICT(chat_id) DO UPDATE SET target=excluded.target, updated_at=excluded.updated_at`, chatID, target, ts(now))
	return err
}

// ---- hosts ----

// HostRow is a host's stored health.
type HostRow struct {
	Name          string
	LastSeen      time.Time
	ConnectedAt   time.Time
	Version       string
	Heartbeat     proto.Heartbeat
	SilentAlerted bool
	QueuedNoticed bool
}

// EnsureHost creates the host's row if missing.
func (s *Store) EnsureHost(name string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO hosts(name) VALUES(?)`, name)
	return err
}

// Host returns a host's row (zero values when never seen).
func (s *Store) Host(name string) (HostRow, error) {
	h := HostRow{Name: name}
	var last, conn sql.NullFloat64
	var hb string
	var sa, qn int
	err := s.db.QueryRow(`SELECT last_seen, connected_at, version, heartbeat, silent_alerted, queued_noticed FROM hosts WHERE name=?`, name).
		Scan(&last, &conn, &h.Version, &hb, &sa, &qn)
	if errors.Is(err, sql.ErrNoRows) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	h.LastSeen, h.ConnectedAt = fromTS(last), fromTS(conn)
	h.SilentAlerted, h.QueuedNoticed = sa == 1, qn == 1
	if hb != "" {
		json.Unmarshal([]byte(hb), &h.Heartbeat)
	}
	return h, nil
}

// HostConnected records a new connection.
func (s *Store) HostConnected(name, version string, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO hosts(name, last_seen, connected_at, version) VALUES(?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET last_seen=excluded.last_seen, connected_at=excluded.connected_at, version=excluded.version`,
		name, ts(now), ts(now), version)
	return err
}

// HostSeen records a heartbeat (hb nil = any other frame).
func (s *Store) HostSeen(name string, hb *proto.Heartbeat, now time.Time) error {
	if hb == nil {
		_, err := s.db.Exec(`UPDATE hosts SET last_seen=? WHERE name=?`, ts(now), name)
		return err
	}
	b, _ := json.Marshal(hb)
	_, err := s.db.Exec(`UPDATE hosts SET last_seen=?, heartbeat=?, version=? WHERE name=?`, ts(now), string(b), hb.Version, name)
	return err
}

// SetHostFlags sets the alert flags.
func (s *Store) SetHostFlags(name string, silentAlerted, queuedNoticed bool) error {
	_, err := s.db.Exec(`UPDATE hosts SET silent_alerted=?, queued_noticed=? WHERE name=?`, b2i(silentAlerted), b2i(queuedNoticed), name)
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- kv ----

// Get returns a kv value ("" when unset).
func (s *Store) Get(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM kv WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// Set stores a kv value.
func (s *Store) Set(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO kv(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// Hosts lists every host with a row, sorted by name.
func (s *Store) Hosts() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
