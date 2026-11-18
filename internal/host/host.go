// Package host is `hommy host`: one WebSocket to the hub, jobs executed
// locally, results acked after a durable ledger write.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/AlexDobrushskiy/hommy/internal/hook"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
	"github.com/AlexDobrushskiy/hommy/internal/version"
)

// Handler executes one job. emit sends progress events to the hub.
type Handler func(ctx context.Context, job proto.Job, emit func(proto.Event)) (proto.Result, error)

// Host holds the handlers, the ledger and the current hub connection.
type Host struct {
	cfg      Config
	ledger   *Ledger
	now      func() time.Time
	started  time.Time
	handlers map[string]Handler
	once     map[string]bool // job types run at most once (see RegisterAtMostOnce)

	mu        sync.Mutex
	conn      *websocket.Conn // nil while disconnected
	inflight  map[string]*inflightJob
	cancelled map[string]time.Time // event ids cancelled before they arrived
	jobs      sync.WaitGroup

	hbMu    sync.Mutex
	hbExtra func(ctx context.Context, hb *proto.Heartbeat)
	// hookSessions lists what the hooks reported (StartHookRelay sets it).
	hookSessions func() []proto.HookSession
	checks       func(ctx context.Context) map[string]string
	relay        *hook.Relay

	outbox *outbox       // durable events waiting for the hub's ack
	hbNow  chan struct{} // HeartbeatNow: report without waiting for the tick

	sendHook func(proto.Frame) // tests: observe frames instead of writing to a socket

	// Tests only: beforeClaim runs before a pushed job is checked against the
	// ledger and the running jobs; waitRetry replaces the ledger-retry backoff.
	beforeClaim func()
	waitRetry   func(ctx context.Context, attempt int) bool
}

type inflightJob struct {
	jobID    string
	started  bool         // a "started" event was emitted; re-sent when the job is re-pushed
	progress *proto.Event // the newest progress snapshot; re-sent when the job is re-pushed
	cancel   context.CancelCauseFunc
}

// errStopped is the cancel cause of a job stopped from the chat.
var errStopped = errors.New(proto.ErrStopped)

// New opens the ledger in cfg.DataDir and registers the phase-02 handlers.
func New(cfg Config) (*Host, error) {
	now := time.Now
	l, err := OpenLedger(cfg.ledgerPath(), now())
	if err != nil {
		return nil, err
	}
	h := &Host{cfg: cfg, ledger: l, now: now, started: now(), handlers: map[string]Handler{},
		once: map[string]bool{}, inflight: map[string]*inflightJob{}, cancelled: map[string]time.Time{},
		hbNow:  make(chan struct{}, 1),
		outbox: &outbox{path: filepath.Join(cfg.DataDir, "outbox.json")}}
	if err := h.interruptOrphans(); err != nil {
		l.Close()
		return nil, err
	}
	h.RegisterAtMostOnce(proto.JobRunP, h.runP)
	h.Register(proto.JobSaveFile, h.saveFile)
	h.Register(proto.JobCancel, h.cancelJob)
	return h, nil
}

// interruptOrphans answers the jobs a previous process began and never
// finished (it crashed or was killed) with ErrInterrupted, and kills what is
// left of their process groups.
func (h *Host) interruptOrphans() error {
	for _, o := range h.ledger.Orphans() {
		if o.PGID > 0 {
			killOrphanGroup(o.PGID, o.At)
		}
		data, _ := json.Marshal(proto.RunPResult{})
		r := proto.Result{Status: proto.StatusError, Error: proto.ErrInterrupted, Data: data}
		if err := h.ledger.Put(o.EventID, r, h.now()); err != nil {
			// Without a record a re-push would run it again: don't start.
			return fmt.Errorf("record interrupted job %s: %w", o.EventID, err)
		}
		log.Printf("job for event %s was running when the host stopped: answered %q", o.EventID, proto.ErrInterrupted)
	}
	return nil
}

// Register adds (or replaces) the handler for a job type.
func (h *Host) Register(jobType string, fn Handler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[jobType] = fn
}

// RegisterAtMostOnce is Register for jobs that must never run twice (a
// claude run, typing into a session): the host records the start durably, and
// a job it was running when it stopped is answered ErrInterrupted instead of
// being run again.
func (h *Host) RegisterAtMostOnce(jobType string, fn Handler) {
	h.Register(jobType, fn)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.once[jobType] = true
}

// SetHeartbeatExtra lets later phases add data (agterm tree, sessions) to every heartbeat.
func (h *Host) SetHeartbeatExtra(fn func(ctx context.Context, hb *proto.Heartbeat)) {
	h.hbMu.Lock()
	defer h.hbMu.Unlock()
	h.hbExtra = fn
}

// SetChecks sets the host's own health report (vault, agterm) for every heartbeat.
func (h *Host) SetChecks(fn func(ctx context.Context) map[string]string) {
	h.hbMu.Lock()
	defer h.hbMu.Unlock()
	h.checks = fn
}

// SetHookSessions sets what the heartbeat reports as the hooks' sessions
// (StartHookRelay sets the relay's; tests set their own).
func (h *Host) SetHookSessions(fn func() []proto.HookSession) {
	h.hbMu.Lock()
	defer h.hbMu.Unlock()
	h.hookSessions = fn
}

// Close waits (up to closeWait) for running jobs to record their results,
// then releases the ledger. Jobs end when the ctx given to Run ends.
func (h *Host) Close() error {
	done := make(chan struct{})
	go func() { h.jobs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(closeWait):
		log.Printf("closing with jobs still running")
	}
	return h.ledger.Close()
}

// closeWait bounds Close; launchd's default exit timeout is 20 s.
const closeWait = 15 * time.Second

// Run keeps a connection to the hub until ctx ends, reconnecting with backoff.
func (h *Host) Run(ctx context.Context) error {
	attempt := 0
	for {
		began := h.now()
		err := h.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if h.now().Sub(began) > 30*time.Second {
			attempt = 0 // the connection was healthy for a while: start the backoff over
		}
		d := reconnectBackoff(attempt, rand.Float64())
		attempt++
		log.Printf("hub connection ended (%v); reconnecting in %s", err, d.Round(100*time.Millisecond))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(d):
		}
	}
}

// backoff is exponential (1 s doubling, capped at 60 s) with jitter in [d/2, d].
func backoff(attempt int, r float64) time.Duration {
	d := time.Second << min(attempt, 6)
	if d > time.Minute {
		d = time.Minute
	}
	return d/2 + time.Duration(r*float64(d/2))
}

// reconnectBackoff is backoff capped at 8 s: the hub is usually on this Mac
// (hommy config set restarts it), and a host must not stay away for a minute.
func reconnectBackoff(attempt int, r float64) time.Duration {
	d := min(time.Second<<min(attempt, 3), 8*time.Second)
	return d/2 + time.Duration(r*float64(d/2))
}

func (h *Host) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+h.cfg.Key)
	hdr.Set("X-Hommy-Host", h.cfg.Name)
	c, resp, err := websocket.Dial(dialCtx, h.cfg.HubURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial: %w (HTTP %d)", err, resp.StatusCode)
		}
		return fmt.Errorf("dial: %w", err)
	}
	c.SetReadLimit(proto.MaxFrameBytes)
	defer c.CloseNow()

	if err := writeFrame(ctx, c, proto.Frame{Type: proto.FrameHello,
		Hello: &proto.Hello{Host: h.cfg.Name, Version: version.String()}}); err != nil {
		return err
	}
	h.setConn(c)
	defer h.setConn(nil)
	log.Printf("connected to hub %s as %s", h.cfg.HubURL, h.cfg.Name)

	sctx, stop := context.WithCancel(ctx)
	defer stop()
	go h.heartbeatLoop(sctx, c)

	for {
		rctx, rcancel := context.WithTimeout(sctx, proto.ReadTimeout)
		_, data, err := c.Read(rctx)
		rcancel()
		if err != nil {
			return err
		}
		var f proto.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			log.Printf("bad frame from hub: %v", err)
			continue
		}
		switch f.Type {
		case proto.FrameJob:
			if f.Job != nil {
				h.receive(ctx, *f.Job)
			}
		case proto.FrameAck:
			if f.Ack != nil && f.Ack.Kind == proto.EventSpool {
				h.spoolDone(f.Ack.JobID)
			} else if f.Ack != nil {
				if err := h.outbox.done(*f.Ack); err != nil {
					log.Printf("outbox: %v", err)
				}
			}
		default:
			log.Printf("unexpected frame %q from hub", f.Type)
		}
	}
}

func (h *Host) setConn(c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conn = c
}

// send writes to the current connection; while disconnected the frame is
// dropped (the hub re-pushes unacked jobs and the ledger answers them).
func (h *Host) send(f proto.Frame) {
	h.mu.Lock()
	c, hook := h.conn, h.sendHook
	h.mu.Unlock()
	if hook != nil {
		hook(f)
		return
	}
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := writeFrame(ctx, c, f); err != nil {
		log.Printf("send %s: %v", f.Type, err)
	}
}

func writeFrame(ctx context.Context, c *websocket.Conn, f proto.Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

// receive handles a pushed job: a finished one is re-acked from the ledger,
// a running one is left alone (its result goes out when it finishes), anything
// else starts. Jobs run under the host's root ctx, so they outlive reconnects.
func (h *Host) receive(ctx context.Context, job proto.Job) {
	if h.beforeClaim != nil {
		h.beforeClaim()
	}
	// Ledger and inflight are checked under one lock: a run is removed from
	// inflight only after its ledger write, so one of the two always sees it.
	h.mu.Lock()
	if r, ok := h.ledger.Get(job.EventID); ok {
		h.mu.Unlock()
		r.JobID = job.ID
		h.send(proto.Frame{Type: proto.FrameResult, Result: &r})
		return
	}
	if in, ok := h.inflight[job.EventID]; ok {
		started, progress := in.started, in.progress
		h.mu.Unlock()
		if started {
			h.send(proto.Frame{Type: proto.FrameEvent, Event: &proto.Event{JobID: job.ID, Kind: proto.EventStarted}})
		}
		if progress != nil {
			e := *progress
			e.JobID = job.ID
			h.send(proto.Frame{Type: proto.FrameEvent, Event: &e})
		}
		return
	}
	jctx, cancel := context.WithCancelCause(ctx)
	in := &inflightJob{jobID: job.ID, cancel: cancel}
	if _, ok := h.cancelled[job.EventID]; ok {
		cancel(errStopped)
	}
	h.inflight[job.EventID] = in
	fn := h.handlers[job.Type]
	once := h.once[job.Type]
	h.jobs.Add(1)
	h.mu.Unlock()

	go func() {
		defer h.jobs.Done()
		defer func() {
			h.mu.Lock()
			delete(h.inflight, job.EventID)
			h.mu.Unlock()
			cancel(nil)
		}()
		var res proto.Result
		if err := h.begin(job, once); err != nil {
			res = proto.Result{Status: proto.StatusError, Error: "host ledger: " + err.Error()}
		} else {
			res = h.execute(jctx, job, fn, in)
		}
		res.JobID = job.ID
		// Ack only a durable result; meanwhile the job stays inflight, so a re-push doesn't rerun it.
		for attempt := 0; ; attempt++ {
			err := h.ledger.Put(job.EventID, res, h.now())
			if err == nil {
				break
			}
			log.Printf("ledger write for %s (attempt %d): %v", job.EventID, attempt+1, err)
			if !h.retryWait(ctx, attempt) {
				return // shutting down unacked: the hub re-pushes after the restart
			}
		}
		h.send(proto.Frame{Type: proto.FrameResult, Result: &res})
	}()
}

// begin records the start of an at-most-once job before it runs.
func (h *Host) begin(job proto.Job, once bool) error {
	if !once {
		return nil
	}
	return h.ledger.Begin(job.EventID, h.now())
}

// cancelJob stops a running job (its handler sees the cause errStopped); a job
// not running yet is remembered so a later push of it doesn't run it.
func (h *Host) cancelJob(ctx context.Context, job proto.Job, emit func(proto.Event)) (proto.Result, error) {
	var p proto.Cancel
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.EventID == "" {
		return proto.Result{}, fmt.Errorf("bad cancel payload: %v", err)
	}
	h.mu.Lock()
	in, running := h.inflight[p.EventID]
	if running {
		in.cancel(errStopped)
	} else if _, done := h.ledger.Get(p.EventID); !done {
		now := h.now()
		for id, at := range h.cancelled {
			if now.Sub(at) > 24*time.Hour {
				delete(h.cancelled, id)
			}
		}
		h.cancelled[p.EventID] = now
	}
	h.mu.Unlock()
	log.Printf("cancel %s: running=%v", p.EventID, running)
	data, _ := json.Marshal(proto.CancelResult{WasRunning: running})
	return proto.Result{Status: proto.StatusOK, Data: data}, nil
}

func (h *Host) retryWait(ctx context.Context, attempt int) bool {
	if h.waitRetry != nil {
		return h.waitRetry(ctx, attempt)
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(backoff(attempt, rand.Float64())):
		return true
	}
}

func (h *Host) execute(ctx context.Context, job proto.Job, fn Handler, in *inflightJob) (res proto.Result) {
	if fn == nil {
		return proto.Result{Status: proto.StatusError, Error: fmt.Sprintf("unknown job type %q", job.Type)}
	}
	defer func() {
		if p := recover(); p != nil {
			log.Printf("job %s (%s) panicked: %v", job.ID, job.Type, p)
			res = proto.Result{Status: proto.StatusError, Error: fmt.Sprintf("handler panic: %v", p)}
		}
	}()
	emit := func(e proto.Event) {
		e.JobID = job.ID
		switch e.Kind {
		case proto.EventStarted:
			h.mu.Lock()
			in.started = true
			h.mu.Unlock()
		case proto.EventProgress:
			h.mu.Lock()
			in.progress = &e
			h.mu.Unlock()
		}
		h.send(proto.Frame{Type: proto.FrameEvent, Event: &e})
	}
	log.Printf("job %s %s (event %s)", job.ID, job.Type, job.EventID)
	r, err := fn(ctx, job, emit)
	if err != nil {
		if r.Status == "" || r.Status == proto.StatusOK {
			r.Status = proto.StatusError
		}
		if r.Error == "" {
			r.Error = err.Error()
		}
	}
	if r.Status == "" {
		r.Status = proto.StatusOK
	}
	return r
}

func (h *Host) heartbeatLoop(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(h.cfg.heartbeatEvery())
	defer t.Stop()
	for {
		hb := h.heartbeat(ctx)
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := writeFrame(wctx, c, proto.Frame{Type: proto.FrameHeartbeat, Heartbeat: &hb})
		cancel()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("heartbeat: %v", err)
			}
			c.CloseNow() // the read loop notices and the host reconnects
			return
		}
		for _, e := range append(h.outbox.pending(), h.spoolEvents()...) { // until the hub acks them
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := writeFrame(wctx, c, proto.Frame{Type: proto.FrameEvent, Event: &e})
			cancel()
			if err != nil {
				log.Printf("outbox: %v", err)
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-h.hbNow:
		}
	}
}

// HeartbeatNow makes the next heartbeat go out now (a new session shows up on
// the hub without waiting for the tick).
func (h *Host) HeartbeatNow() {
	select {
	case h.hbNow <- struct{}{}:
	default:
	}
}

func (h *Host) heartbeat(ctx context.Context) proto.Heartbeat {
	h.mu.Lock()
	running := len(h.inflight)
	h.mu.Unlock()
	hb := proto.Heartbeat{
		Host:           h.cfg.Name,
		Version:        version.String(),
		StartedAt:      h.started,
		RunningJobs:    running,
		ClaudeSessions: countClaudeProcesses(ctx),
		AgentFolders:   agentFolders(h.cfg.AgentBriefsDir, 20),
	}
	h.hbMu.Lock()
	extra, hooks, checks := h.hbExtra, h.hookSessions, h.checks
	h.hbMu.Unlock()
	if extra != nil {
		extra(ctx, &hb)
	}
	if checks != nil {
		hb.Checks = checks(ctx)
	}
	if hooks != nil {
		hb.Hooks = hooks()
	}
	return hb
}
