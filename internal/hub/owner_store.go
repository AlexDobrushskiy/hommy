package hub

import (
	"database/sql"
	"errors"
	"time"
)

const ownerSchema = `
CREATE TABLE IF NOT EXISTS owner(
  user_id    INTEGER PRIMARY KEY,
  chat_id    INTEGER NOT NULL,
  first_name TEXT NOT NULL DEFAULT '',
  paired_at  REAL NOT NULL);
CREATE TABLE IF NOT EXISTS pairing(
  code_hash  TEXT PRIMARY KEY,
  expires_at REAL NOT NULL,
  used_at    REAL);
CREATE TABLE IF NOT EXISTS pairing_attempts(
  user_id INTEGER PRIMARY KEY,
  count   INTEGER NOT NULL);
`

// Owner is the paired Telegram user (zero UserID = unpaired).
type Owner struct {
	UserID    int64
	ChatID    int64
	FirstName string
	PairedAt  time.Time
}

// Paired reports whether there is an owner.
func (o Owner) Paired() bool { return o.UserID != 0 }

// Owner is the stored owner (zero when unpaired).
func (s *Store) Owner() (Owner, error) {
	var o Owner
	var at sql.NullFloat64
	err := s.db.QueryRow(`SELECT user_id, chat_id, first_name, paired_at FROM owner LIMIT 1`).
		Scan(&o.UserID, &o.ChatID, &o.FirstName, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Owner{}, nil
	}
	o.PairedAt = fromTS(at)
	return o, err
}

// setOwnerTx replaces the owner (at most one row) and drops pending codes and
// wrong-code counters.
func setOwnerTx(tx *sql.Tx, o Owner) error {
	for _, q := range []string{`DELETE FROM owner`, `DELETE FROM pairing WHERE used_at IS NULL`, `DELETE FROM pairing_attempts`} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`INSERT INTO owner(user_id, chat_id, first_name, paired_at) VALUES(?,?,?,?)`,
		o.UserID, o.ChatID, o.FirstName, ts(o.PairedAt))
	return err
}

func (s *Store) inTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// SetOwner binds o directly (the operator path, no code).
func (s *Store) SetOwner(o Owner) error {
	return s.inTx(func(tx *sql.Tx) error { return setOwnerTx(tx, o) })
}

// ResetPairing clears the owner, every code and every wrong-code counter.
func (s *Store) ResetPairing() error {
	return s.inTx(func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM owner`, `DELETE FROM pairing`, `DELETE FROM pairing_attempts`} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		return nil
	})
}

// NewPairingCode stores a code's hash; it replaces a pending code and resets
// the per-user wrong-code counters.
func (s *Store) NewPairingCode(hash string, expires time.Time) error {
	return s.inTx(func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM pairing WHERE used_at IS NULL`, `DELETE FROM pairing_attempts`} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT OR REPLACE INTO pairing(code_hash, expires_at) VALUES(?,?)`, hash, ts(expires))
		return err
	})
}

// PendingPairing is the unused, unexpired code's expiry (ok=false when none).
func (s *Store) PendingPairing(now time.Time) (time.Time, bool, error) {
	var exp sql.NullFloat64
	err := s.db.QueryRow(`SELECT expires_at FROM pairing WHERE used_at IS NULL AND expires_at > ? ORDER BY expires_at DESC LIMIT 1`,
		ts(now)).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return fromTS(exp), true, nil
}

// ClaimPairing consumes the code with hash and makes o the owner, in one
// transaction; false when the hub is already paired or no pending, unexpired
// code has that hash.
func (s *Store) ClaimPairing(hash string, o Owner, now time.Time) (bool, error) {
	claimed := false
	err := s.inTx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM owner`).Scan(&n); err != nil || n > 0 {
			return err
		}
		res, err := tx.Exec(`UPDATE pairing SET used_at=? WHERE code_hash=? AND used_at IS NULL AND expires_at > ?`,
			ts(now), hash, ts(now))
		if err != nil {
			return err
		}
		if k, _ := res.RowsAffected(); k != 1 {
			return nil
		}
		claimed = true
		return setOwnerTx(tx, o)
	})
	return claimed && err == nil, err
}

// PairingAttempts is how many wrong codes user has sent since the last new code.
func (s *Store) PairingAttempts(user int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count FROM pairing_attempts WHERE user_id=?`, user).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// AddPairingAttempt counts one wrong code from user and returns the new count.
func (s *Store) AddPairingAttempt(user int64) (int, error) {
	var n int
	err := s.db.QueryRow(`INSERT INTO pairing_attempts(user_id, count) VALUES(?, 1)
		ON CONFLICT(user_id) DO UPDATE SET count=count+1 RETURNING count`, user).Scan(&n)
	return n, err
}
