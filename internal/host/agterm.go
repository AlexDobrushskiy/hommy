package host

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// RegisterAgterm adds the agent-session jobs (type, new_session, inbox_append,
// peek, files, pause, done), the typing retry loop and the agterm heartbeat data.
func RegisterAgterm(ctx context.Context, h *Host, ctl agterm.Ctl, opts ...func(*agterm.Service)) *agterm.Service {
	env := agterm.Env{
		Home:          h.cfg.Home,
		BriefsRoot:    h.cfg.AgentBriefsDir,
		FallbackInbox: agterm.FallbackInboxFor(h.cfg.DataDir),
		StateDir:      filepath.Join(h.cfg.DataDir, "agterm"),
		Roots:         h.cfg.NewRoots,
		Pinned:        h.cfg.NewPinned,
		Host:          h.cfg.Name,
		Now:           time.Now,
	}
	svc := agterm.NewService(ctl, env, func(a agterm.Attempt) { h.typeOutcome(a) })
	svc.ClaudeCommand = h.cfg.AgentCommandLine()
	if h.cfg.Workspace != "" {
		svc.Workspace = h.cfg.Workspace
	}
	for _, o := range opts { // tests: clocks, before any loop reads them
		o(svc)
	}
	svc.NoteTyped = h.noteTyped
	h.Register(proto.JobType, adapt(func(ctx context.Context, job proto.Job, r agterm.TypeRequest) (string, any) {
		if r.EventID == "" {
			r.EventID = job.EventID
		}
		r.JobID = job.ID
		h.noteTyped(r.MsgID, r.Session) // before typing: the session's prompt hook may run before Type returns
		res := svc.Type(ctx, r)
		if res.Status == proto.StatusOK || res.Status == proto.StatusInbox {
			h.noteTyped(r.MsgID, firstNonEmpty(res.Session, r.Session))
		}
		return res.Status, res
	}))
	h.Register(proto.JobNewSession, adapt(func(ctx context.Context, job proto.Job, r agterm.NewSessionRequest) (string, any) {
		if r.EventID == "" {
			r.EventID = job.EventID
		}
		r.JobID = job.ID
		res := svc.NewSession(ctx, r)
		h.noteTyped(r.MsgID, res.Session)
		h.HeartbeatNow() // the hub sees the new session at once
		return res.Status, res
	}))
	h.Register(proto.JobInboxAppend, adapt(func(ctx context.Context, job proto.Job, r agterm.InboxAppendRequest) (string, any) {
		if r.EventID == "" {
			r.EventID = job.EventID
		}
		res := svc.InboxAppend(ctx, r)
		return res.Status, res
	}))
	h.Register(proto.JobPeek, adapt(func(ctx context.Context, _ proto.Job, r agterm.PeekRequest) (string, any) {
		res := svc.Peek(ctx, r)
		return res.Status, res
	}))
	for typ, fn := range map[string]func(context.Context, agterm.BriefRequest) agterm.BriefResult{
		proto.JobFiles: svc.Files, proto.JobPause: svc.Pause, proto.JobDone: svc.Done,
	} {
		h.Register(typ, adapt(func(ctx context.Context, _ proto.Job, r agterm.BriefRequest) (string, any) {
			res := fn(ctx, r)
			return res.Status, res
		}))
	}
	h.SetHeartbeatExtra(func(ctx context.Context, hb *proto.Heartbeat) {
		sctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		at := time.Now() // before any screen is read
		snap := svc.Snapshot(sctx)
		if b, err := json.Marshal(snap); err == nil {
			hb.Agterm = b
		}
		h.observeComposers(at, snap)
	})
	go svc.Queue.Run(ctx)
	return svc
}

// observeComposers lets the hook relay end the ⏸ of sessions that left their
// prompt (declined, interrupted, answered without a Stop yet, exited). Only a
// complete reading counts: a session missing from a failed one isn't gone.
func (h *Host) observeComposers(at time.Time, snap agterm.Snapshot) {
	h.hbMu.Lock()
	r := h.relay
	h.hbMu.Unlock()
	if r == nil || snap.Error != "" {
		return
	}
	states := map[string]string{}
	for _, s := range snap.Sessions {
		if s.State != "" {
			states[s.ID] = string(s.State)
		}
	}
	r.ObserveComposers(at, states)
}

// adapt decodes the payload into the request type and wraps the typed result
// as proto.Result (status from the result, the result itself as Data).
func adapt[R any](fn func(ctx context.Context, job proto.Job, req R) (string, any)) Handler {
	return func(ctx context.Context, job proto.Job, _ func(proto.Event)) (proto.Result, error) {
		var req R
		if err := json.Unmarshal(job.Payload, &req); err != nil {
			return proto.Result{}, fmt.Errorf("bad %s payload: %w", job.Type, err)
		}
		status, res := fn(ctx, job, req)
		data, _ := json.Marshal(res)
		r := proto.Result{Status: status, Data: data}
		if status == proto.StatusError {
			var e struct {
				Error  string `json:"error"`
				Detail string `json:"detail"`
			}
			_ = json.Unmarshal(data, &e)
			r.Error = e.Error + e.Detail
		}
		return r, nil
	}
}

// typeOutcome tells the hub what became of a line after its job returned.
func (h *Host) typeOutcome(a agterm.Attempt) {
	if a.Outcome == agterm.Delivered || a.Outcome == agterm.Unconfirmed {
		h.noteTyped(a.MsgID, a.Session)
	}
	if a.JobID == "" {
		return
	}
	data, _ := json.Marshal(proto.TypeOutcome{Outcome: string(a.Outcome), Session: a.Session, Detail: a.Detail,
		NoSession: a.Outcome == agterm.Expired && a.LastState == ""})
	log.Printf("type %s (job %s): %s %s", a.EventID, a.JobID, a.Outcome, a.Detail)
	ev := proto.Event{JobID: a.JobID, Kind: proto.EventTypeOutcome, Data: data}
	// kept until the hub acks it, so a disconnect now does not lose the ack on the owner's message
	if err := h.outbox.add(ev); err != nil {
		log.Printf("outbox: %v", err)
	}
	h.send(proto.Frame{Type: proto.FrameEvent, Event: &ev})
}
