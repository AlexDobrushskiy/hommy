package hub

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// PendingSpawn is the chat's unfinished new_session job for target (nil if none).
func (s *Store) PendingSpawn(chatID int64, target string) (*JobRow, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE type=? AND chat_id=? AND target=? AND status != ?
		ORDER BY seq DESC LIMIT 1`, proto.JobNewSession, chatID, target, JobDone))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// FollowUps are the queued jobs waiting for new_session job spawn, in order.
func (s *Store) FollowUps(spawn string) ([]*JobRow, error) {
	return s.queryJobs(`SELECT `+jobCols+` FROM jobs WHERE lane=? AND status=? AND id != ? ORDER BY seq, sub`,
		"new:"+spawn, JobQueued, spawn)
}

// MoveJob puts a queued job in another lane with a new target and payload.
func (s *Store) MoveJob(id, lane, target string, payload json.RawMessage) error {
	_, err := s.db.Exec(`UPDATE jobs SET lane=?, target=?, payload=? WHERE id=? AND status=?`, lane, target, []byte(payload), id, JobQueued)
	return err
}
