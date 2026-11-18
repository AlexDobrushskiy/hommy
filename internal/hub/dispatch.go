package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

type hostConn struct {
	name string
	c    *websocket.Conn
	wmu  sync.Mutex
}

func (hc *hostConn) send(ctx context.Context, f proto.Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	hc.wmu.Lock()
	defer hc.wmu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return hc.c.Write(wctx, websocket.MessageText, b)
}

func (h *Hub) connected(host string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[host] != nil
}

func (h *Hub) conn(host string) *hostConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[host]
}

// serveWS accepts one host. Auth: `Authorization: Bearer <key>` matching the
// host named in X-Hommy-Host; anything else gets 401 before the upgrade.
func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	name := r.Header.Get("X-Hommy-Host")
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !h.cfg.checkKey(name, key) {
		log.Printf("ws: rejected host %q from %s", name, r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("ws accept %s: %v", name, err)
		return
	}
	c.SetReadLimit(proto.MaxFrameBytes)
	defer c.CloseNow()
	ctx := r.Context()

	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, data, err := c.Read(hctx)
	cancel()
	var hello proto.Frame
	if err != nil || json.Unmarshal(data, &hello) != nil || hello.Type != proto.FrameHello || hello.Hello == nil || hello.Hello.Host != name {
		log.Printf("ws %s: no valid hello: %v", name, err)
		c.Close(websocket.StatusPolicyViolation, "hello expected")
		return
	}
	hc := &hostConn{name: name, c: c}
	h.register(ctx, hc, hello.Hello.Version)
	defer h.unregister(hc)

	for {
		rctx, rcancel := context.WithTimeout(ctx, proto.ReadTimeout)
		_, data, err := c.Read(rctx)
		rcancel()
		if err != nil {
			log.Printf("ws %s: %v", name, err)
			return
		}
		var f proto.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			log.Printf("ws %s: bad frame: %v", name, err)
			continue
		}
		h.handleFrame(ctx, hc, f)
	}
}

func (h *Hub) register(ctx context.Context, hc *hostConn, version string) {
	h.mu.Lock()
	old := h.conns[hc.name]
	h.conns[hc.name] = hc
	h.mu.Unlock()
	if old != nil {
		old.c.Close(websocket.StatusGoingAway, "replaced by a new connection")
	}
	log.Printf("host %s connected (version %s)", hc.name, version)
	now := h.now()
	row, _ := h.st.Host(hc.name)
	if err := h.st.HostConnected(hc.name, version, now); err != nil {
		log.Printf("host %s: %v", hc.name, err)
	}
	switch {
	case row.SilentAlerted:
		silent := "never seen before"
		if !row.LastSeen.IsZero() {
			silent = "silent for " + shortDur(now.Sub(row.LastSeen))
		}
		h.say(ctx, h.ownerChat(), "🟢 "+hc.name+" is back ("+silent+")", 0)
	case row.QueuedNoticed: // the owner was told "<host> is offline; queued"
		off := now.Sub(maxTime(row.LastSeen, h.started)) // as checkHealth counts it
		h.say(ctx, h.ownerChat(), "🟢 "+hc.name+" is back (offline for "+shortDur(off)+")", 0)
	}
	if row.SilentAlerted || row.QueuedNoticed {
		if err := h.st.SetHostFlags(hc.name, false, false); err != nil {
			log.Printf("host %s: %v", hc.name, err)
		}
	}
	select {
	case h.repush <- hc.name:
	default:
		h.kickDispatch()
	}
}

func (h *Hub) unregister(hc *hostConn) {
	h.mu.Lock()
	if h.conns[hc.name] == hc {
		delete(h.conns, hc.name)
	}
	h.mu.Unlock()
	log.Printf("host %s disconnected", hc.name)
}

func (h *Hub) handleFrame(ctx context.Context, hc *hostConn, f proto.Frame) {
	now := h.now()
	switch f.Type {
	case proto.FrameHeartbeat:
		if f.Heartbeat == nil {
			return
		}
		if err := h.st.HostSeen(hc.name, f.Heartbeat, now); err != nil {
			log.Printf("heartbeat %s: %v", hc.name, err)
		}
		if err := hc.send(ctx, proto.Frame{Type: proto.FrameAck}); err != nil {
			log.Printf("ack %s: %v", hc.name, err)
		}
	case proto.FrameEvent:
		h.seen(hc.name, now)
		if f.Event != nil && f.Event.Kind == proto.EventSpool {
			if err := h.handleSpool(ctx, hc.name, *f.Event); err != nil {
				log.Printf("spool %s: %v; the host sends it again", f.Event.JobID, err)
				return
			}
			if err := hc.send(ctx, proto.Frame{Type: proto.FrameAck, Ack: &proto.Ack{JobID: f.Event.JobID, Kind: f.Event.Kind}}); err != nil {
				log.Printf("ack %s: %v", hc.name, err)
			}
		} else if f.Event != nil && (f.Event.Kind == proto.EventHookAnswer || f.Event.Kind == proto.EventHookWaiting || f.Event.Kind == proto.EventHookResumed) {
			if err := h.handleHookEvent(hc.name, *f.Event); err != nil {
				log.Printf("hook event %s: %v; the host sends it again", f.Event.JobID, err)
				return
			}
			if err := hc.send(ctx, proto.Frame{Type: proto.FrameAck, Ack: &proto.Ack{JobID: f.Event.JobID, Kind: f.Event.Kind}}); err != nil {
				log.Printf("ack %s: %v", hc.name, err)
			}
		} else if f.Event != nil && f.Event.Kind == proto.EventTypeOutcome {
			if err := h.handleTypeOutcome(ctx, *f.Event); err != nil {
				log.Printf("type outcome %s: %v; the host sends it again", f.Event.JobID, err)
				return
			}
			if err := hc.send(ctx, proto.Frame{Type: proto.FrameAck, Ack: &proto.Ack{JobID: f.Event.JobID, Kind: f.Event.Kind}}); err != nil {
				log.Printf("ack %s: %v", hc.name, err)
			}
		} else if f.Event != nil {
			h.handleEvent(ctx, *f.Event)
		}
	case proto.FrameResult:
		h.seen(hc.name, now)
		if f.Result != nil {
			if err := h.handleResult(ctx, hc.name, *f.Result); err != nil {
				// The host sends a result once per push: reconnecting makes the hub
				// re-push the job and the host's ledger answer it again.
				log.Printf("store result %s: %v; dropping %s's connection for a replay", f.Result.JobID, err, hc.name)
				hc.c.Close(websocket.StatusTryAgainLater, "result not stored")
			}
		}
	default:
		log.Printf("ws %s: unexpected frame %q", hc.name, f.Type)
	}
}

func (h *Hub) handleEvent(ctx context.Context, e proto.Event) {
	if e.Kind != proto.EventStarted && e.Kind != proto.EventProgress {
		return
	}
	j, err := h.st.Job(e.JobID)
	if err != nil || j == nil {
		return
	}
	if e.Kind == proto.EventProgress {
		var p proto.Progress
		if j.Type != proto.JobRunP || j.DraftID == 0 || j.StopRequested ||
			(j.Status != JobPushed && j.Status != JobStarted) || json.Unmarshal(e.Data, &p) != nil {
			return
		}
		h.streamProgress(j, p)
		return
	}
	first, err := h.st.MarkStarted(j.ID, h.now())
	if err != nil {
		log.Printf("job %s started: %v", j.ID, err)
		return
	}
	if !first && j.Status != JobStarted {
		return // already finished
	}
	if first {
		h.addTyping(j.ChatID)
	}
	if j.Type == proto.JobRunP {
		h.react(ctx, j.ChatID, j.MsgID, "✍")
	}
}

// handleResult stores a host's result and starts its delivery. An error means
// the result is not stored (storage failure) and must be replayed.
func (h *Hub) handleResult(ctx context.Context, host string, r proto.Result) error {
	j, err := h.st.Job(r.JobID)
	if err != nil {
		return err
	}
	if j == nil {
		log.Printf("result for unknown job %s from %s", r.JobID, host)
		return nil
	}
	if j.Host != host {
		log.Printf("result for job %s from %s, but it belongs to %s", j.ID, host, j.Host)
		return nil
	}
	stored, err := h.st.SetResult(j.ID, r, h.now())
	if err != nil {
		return err
	}
	if !stored {
		return nil // duplicate ack (re-push of a finished job)
	}
	if j.Status != JobStarted {
		h.addTyping(j.ChatID) // the started event was lost; typing still pairs with done
	}
	j.Status, j.Result = JobResult, mustJSON(r)
	h.goDeliver(j)
	return nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// dispatchLoop pushes jobs: on connect it re-pushes what the host hasn't
// acked, then the next queued job of every idle lane.
func (h *Hub) dispatchLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case name := <-h.repush:
			h.repushInflight(ctx, name)
		case <-h.kick:
		case <-t.C:
		}
		h.dispatchReady(ctx)
	}
}

func (h *Hub) repushInflight(ctx context.Context, host string) {
	hc := h.conn(host)
	if hc == nil {
		return
	}
	jobs, err := h.st.InflightJobs(host)
	if err != nil {
		log.Printf("inflight %s: %v", host, err)
		return
	}
	for _, j := range jobs {
		log.Printf("re-push job %s (%s) to %s", j.ID, j.Type, host)
		h.push(ctx, hc, j)
	}
}

func (h *Hub) dispatchReady(ctx context.Context) {
	for _, host := range h.hostNames() {
		hc := h.conn(host)
		if hc == nil {
			continue
		}
		jobs, err := h.st.ReadyJobs(host)
		if err != nil {
			log.Printf("ready jobs %s: %v", host, err)
			continue
		}
		for _, j := range jobs {
			if err := h.prepare(j); err != nil {
				if !errors.Is(err, errNotPushed) {
					log.Printf("prepare job %s: %v", j.ID, err)
				}
				continue
			}
			h.push(ctx, hc, j)
		}
	}
}

// prepare fills what is only known at push time: a run_p resumes the session
// its lane's previous run ended on.
func (h *Hub) prepare(j *JobRow) error {
	if j.Type == proto.JobType && j.Chain != "" {
		return h.prepareFollowUp(j)
	}
	if j.Type != proto.JobRunP {
		return nil
	}
	var p proto.RunP
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	sid, err := h.st.SessionID(j.Target, j.SessionGen)
	if err != nil {
		return err
	}
	p.Resume = sid
	j.Payload, _ = json.Marshal(p)
	return nil
}

func (h *Hub) push(ctx context.Context, hc *hostConn, j *JobRow) {
	if err := h.st.MarkPushed(j.ID, j.Payload, h.now()); err != nil {
		log.Printf("mark pushed %s: %v", j.ID, err)
		return
	}
	job := j.Proto()
	if err := hc.send(ctx, proto.Frame{Type: proto.FrameJob, Job: &job}); err != nil {
		// Stays pushed: re-pushed when the host reconnects.
		log.Printf("push %s to %s: %v", j.ID, hc.name, err)
		hc.c.CloseNow()
		return
	}
	log.Printf("pushed job %s (%s, lane %s) to %s", j.ID, j.Type, j.Lane, hc.name)
}

func (h *Hub) seen(host string, now time.Time) {
	if err := h.st.HostSeen(host, nil, now); err != nil {
		log.Printf("host %s seen: %v", host, err)
	}
}
