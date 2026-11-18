package hub

// The end of a ⏸ (hook_resumed): the host saw the session leave the prompt a
// hook_waiting reported (answered, declined, interrupted, exited). The board
// stops counting it and the ⏸ message says so. The ⏸ and its end are separate
// jobs; pauseMu orders "is it over?" against sending / editing the ⏸ message.

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/ping"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
	"github.com/AlexDobrushskiy/hommy/internal/textx"
)

// waitingText is the ⏸ message: waiting, or (over) no longer waiting.
func (h *Hub) waitingText(host string, a proto.HookAnswer, over bool) (string, []models.MessageEntity) {
	lbl, link := h.hookLabel(host, a), h.hookLink(host, a)
	prefix, tail := "⏸ ", " is waiting at the Mac"
	if over {
		prefix, tail = "▶️ ", " is no longer waiting at the Mac"
	}
	head, ents := taggedHead(prefix, lbl, link)
	msg := head + tail
	if what := strings.TrimSpace(a.Text); what != "" {
		msg += "\n" + textx.Head(what, 300)
	}
	return msg + linkLine(link), ents
}

// hookJob is the stored hook report of host with the given report id (nil if none).
func (h *Hub) hookJob(host, id string) (*JobRow, error) {
	return h.st.JobByEventID("hook:" + host + ":" + id)
}

// deliverHookResumed edits the ⏸ message (if it went out) to say the session
// went on. A ⏸ not sent yet is sent that way by deliverHookWaiting.
func (h *Hub) deliverHookResumed(ctx context.Context, j *JobRow, r proto.Result) error {
	var a proto.HookAnswer
	_ = json.Unmarshal(r.Data, &a)
	h.pauseMu.Lock()
	defer h.pauseMu.Unlock()
	w, err := h.hookJob(j.Host, a.Of)
	if err != nil {
		return err
	}
	if w == nil {
		log.Printf("hook_resumed %s: no ⏸ %s stored", a.ID, a.Of)
		return nil
	}
	ids, err := h.st.JobMessages(w.ChatID, w.ID, "waiting")
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil // not sent yet: it goes out as "no longer waiting"
	}
	var wa proto.HookAnswer
	_ = json.Unmarshal(w.Payload, &wa)
	text, ents := h.waitingText(j.Host, wa, true)
	if err := h.part(j, 0, func() error { return h.tg.EditText(ctx, j.ChatID, ids[0], text, ents) }); err != nil {
		return err
	}
	t, err := ParseTarget(w.Target)
	if err != nil || w.Target == "" {
		return nil
	}
	open := h.consoleURL(t)
	if open == "" {
		return nil
	}
	// The edit dropped the Open button; a retry puts it back without editing again.
	return h.part(j, 1, func() error { return h.tg.SetButtons(ctx, j.ChatID, ids[0], ping.OpenRow(open)) })
}

// JobByEventID is the job stored for an event id (nil if none).
func (s *Store) JobByEventID(eventID string) (*JobRow, error) {
	jobs, err := s.queryJobs(`SELECT `+jobCols+` FROM jobs WHERE event_id = ?`, eventID)
	if err != nil || len(jobs) == 0 {
		return nil, err
	}
	return jobs[0], nil
}

// JobMessages are the ids of the messages of a kind recorded for a job, oldest first.
func (s *Store) JobMessages(chatID int64, jobID, kind string) ([]int, error) {
	rows, err := s.db.Query(`SELECT msg_id FROM messages WHERE chat_id = ? AND job_id = ? AND kind = ? ORDER BY msg_id`,
		chatID, jobID, kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// startNoticeWait bounds how long a session's answer waits for its 🆕 line.
const startNoticeWait = 3 * time.Minute

// awaitStartNotice holds a hook report from a session created from the phone
// until its "🆕 … started" line is out, so that line always comes first
// (online, after a reconnect, after a hub restart). The report belongs to the
// creation when it answers the message that created the session, or comes
// from the session the creation reported.
func (h *Hub) awaitStartNotice(ctx context.Context, j *JobRow, agtermID string) error {
	deadline := time.Now().Add(startNoticeWait)
	for {
		n, err := h.st.PendingStartNotices(j.Host, j.ChatID, j.MsgID, agtermID)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			log.Printf("job %s: the 🆕 line for tg %d is still not out after %s; sending anyway", j.ID, j.MsgID, startNoticeWait)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// PendingStartNotices counts new_session jobs on host whose 🆕 line is not out
// yet and that created (or are creating from) message msgID, or reported agterm session id.
func (s *Store) PendingStartNotices(host string, chatID int64, msgID int, agtermID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type = ? AND host = ? AND status != ? AND chat_id = ?
		AND (msg_id = ? OR (? != '' AND upper(coalesce(json_extract(NULLIF(result, ''), '$.data.session'), '')) = upper(?)))`,
		proto.JobNewSession, host, JobDone, chatID, msgID, agtermID, agtermID).Scan(&n)
	return n, err
}
