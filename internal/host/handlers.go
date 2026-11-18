package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/claude"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// streamClaude is swapped in tests.
var streamClaude = claude.Stream

// errTimeout is the cancel cause of a run that hit ClaudeTimeout.
var errTimeout = errors.New("timeout")

func (h *Host) runP(ctx context.Context, job proto.Job, emit func(proto.Event)) (proto.Result, error) {
	var p proto.RunP
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return proto.Result{}, fmt.Errorf("bad run_p payload: %w", err)
	}
	dir := h.cfg.VaultDir
	if p.NewDir {
		if err := os.MkdirAll(h.cfg.AgentBriefsDir, 0o755); err != nil { // always allowed, made on first use
			return proto.Result{}, err
		}
		roots := append([]string{h.cfg.AgentBriefsDir}, h.cfg.newSessionRoots()...)
		d, err := agterm.NewSessionDir(h.cfg.Home, roots, p.Dir, true)
		if err != nil {
			return proto.Result{}, err
		}
		dir = d
	} else if p.Dir != "" {
		d, err := h.resolveDir(p.Dir)
		if err != nil {
			return proto.Result{}, err
		}
		dir = d
	}
	emit(proto.Event{Kind: proto.EventStarted})
	tctx, cancel := context.WithTimeoutCause(ctx, h.cfg.ClaudeTimeout, errTimeout)
	defer cancel()
	env := claude.CleanEnv(os.Environ())
	log.Printf("run_p %s: claude -p in %s (resume %q), CLAUDE* env passed: %v", job.ID, dir, p.Resume, claude.ClaudeVars(env))
	pe := &progressEmitter{emit: emit, every: h.cfg.progressEvery(), now: h.now}
	var res claude.StreamResult
	var err error
	if tctx.Err() == nil {
		res, err = streamClaude(tctx, claude.StreamOptions{Bin: h.cfg.ClaudeBin, Dir: dir,
			AppendSystemPrompt: p.AppendSystemPrompt, Env: env, KillGrace: h.cfg.KillGrace,
			OnStart: func(pgid int) {
				if err := h.ledger.SetPGID(job.EventID, pgid, h.now()); err != nil {
					log.Printf("ledger pgid %s: %v", job.EventID, err)
				}
			},
			OnProgress: pe.offer}, p.Resume, p.Prompt)
	} else {
		err = context.Cause(tctx) // cancelled before it started
	}
	pe.close()
	d := proto.RunPResult{Text: res.Text, SessionID: res.SessionID, CostUSD: res.CostUSD}
	if err == nil {
		data, _ := json.Marshal(d)
		return proto.Result{Status: proto.StatusOK, Data: data}, nil
	}
	msg := err.Error()
	switch cause := context.Cause(tctx); {
	case errors.Is(cause, errStopped):
		d.Stopped, msg = true, proto.ErrStopped
	case errors.Is(cause, errTimeout):
		msg = fmt.Sprintf("timed out after %s", h.cfg.ClaudeTimeout)
	case ctx.Err() != nil: // the host is shutting down
		msg = proto.ErrInterrupted
	}
	log.Printf("run_p %s: %s", job.ID, msg)
	data, _ := json.Marshal(d)
	return proto.Result{Status: proto.StatusError, Data: data, Error: msg}, nil
}

// progressEmitter forwards run progress as Progress snapshots, at most one
// per `every` (the newest wins; a trailing one follows a quiet spell).
type progressEmitter struct {
	emit  func(proto.Event)
	every time.Duration
	now   func() time.Time

	mu      sync.Mutex
	seq     int
	last    time.Time
	sent    claude.Progress
	pending *claude.Progress
	timer   *time.Timer
	closed  bool
}

func (pe *progressEmitter) offer(p claude.Progress) {
	pe.mu.Lock()
	if pe.closed {
		pe.mu.Unlock()
		return
	}
	pe.pending = &p
	wait := pe.every - pe.now().Sub(pe.last)
	if wait > 0 {
		if pe.timer == nil {
			pe.timer = time.AfterFunc(wait, pe.flush)
		}
		pe.mu.Unlock()
		return
	}
	e := pe.takeLocked()
	pe.mu.Unlock()
	pe.send(e)
}

func (pe *progressEmitter) flush() {
	pe.mu.Lock()
	pe.timer = nil
	var e *proto.Event
	if !pe.closed {
		e = pe.takeLocked()
	}
	pe.mu.Unlock()
	pe.send(e)
}

// takeLocked turns the pending progress into the next event (nil if unchanged).
func (pe *progressEmitter) takeLocked() *proto.Event {
	p := pe.pending
	pe.pending = nil
	if p == nil || *p == pe.sent {
		return nil
	}
	pe.seq++
	pe.sent, pe.last = *p, pe.now()
	data, _ := json.Marshal(proto.Progress{Seq: pe.seq, Status: p.Status, Text: p.Text})
	return &proto.Event{Kind: proto.EventProgress, Data: data}
}

// send emits outside the lock; Seq keeps a rare reordering harmless.
func (pe *progressEmitter) send(e *proto.Event) {
	if e != nil {
		pe.emit(*e)
	}
}

// close stops further events (the result carries the final text).
func (pe *progressEmitter) close() {
	pe.mu.Lock()
	pe.closed = true
	if pe.timer != nil {
		pe.timer.Stop()
	}
	pe.mu.Unlock()
}

var extRe = regexp.MustCompile(`^\.[A-Za-z0-9]{1,10}$`)

func (h *Host) saveFile(ctx context.Context, job proto.Job, emit func(proto.Event)) (proto.Result, error) {
	var p proto.SaveFile
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return proto.Result{}, fmt.Errorf("bad save_file payload: %w", err)
	}
	dir := h.cfg.MediaDir
	if p.Dest != "" {
		d, err := h.resolveDir(p.Dest)
		if err != nil {
			return proto.Result{}, err
		}
		dir = d
	}
	ext := p.Ext
	if !extRe.MatchString(ext) {
		ext = ".bin"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return proto.Result{}, err
	}
	name := h.now().Format("20060102-150405") + "-" + safeName(job.EventID) + ext
	path := filepath.Join(dir, name)
	tmp := path + ".part"
	if err := os.WriteFile(tmp, p.Data, 0o644); err != nil {
		return proto.Result{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return proto.Result{}, err
	}
	data, _ := json.Marshal(proto.SaveFileResult{Path: path})
	return proto.Result{Status: proto.StatusOK, Data: data}, nil
}

var unsafeRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func safeName(s string) string {
	s = unsafeRe.ReplaceAllString(s, "_")
	if len(s) > 40 {
		s = s[len(s)-40:]
	}
	return s
}

// resolveDir expands ~ and requires an existing directory under the host
// user's home (symlinks resolved), so a job can't point a run anywhere else.
// underNewRoot: real (resolved) is a configured new-session root or below one,
// so a -p session started there keeps running there.
func (h *Host) underNewRoot(real string) bool {
	for _, root := range h.cfg.newSessionRoots() {
		rr, err := filepath.EvalSymlinks(root)
		if err == nil && (real == rr || strings.HasPrefix(real, rr+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

func (h *Host) resolveDir(p string) (string, error) {
	p = expandHome(strings.TrimSpace(p), h.cfg.Home)
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("dir %q must be absolute or start with ~/", p)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("dir %q: %w", p, err)
	}
	home, err := filepath.EvalSymlinks(h.cfg.Home)
	if err != nil {
		return "", err
	}
	if real != home && !strings.HasPrefix(real, home+string(filepath.Separator)) && !h.underNewRoot(real) {
		return "", fmt.Errorf("dir %q is outside %s", p, h.cfg.Home)
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%q is not a directory", p)
	}
	return real, nil
}
