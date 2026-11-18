package host

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/hook"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// StartHookRelay serves the local hook endpoint: a unix socket (0600, in the
// data dir) that `hommy hook …` posts to. Telegram-driven turns become
// durable outbox events for the hub (re-sent until acked, so they survive a
// hub outage); requests the hooks spooled while the host was down are picked
// up at start and every few seconds. It runs until ctx ends.
func (h *Host) StartHookRelay(ctx context.Context) (*hook.Relay, error) {
	socket := hook.SocketPath(h.cfg.DataDir)
	if h.cfg.HookSocket != "" {
		socket = h.cfg.HookSocket
	}
	r, err := hook.NewRelay(filepath.Join(h.cfg.DataDir, "hookrelay.json"), hook.SpoolDir(h.cfg.DataDir), h.cfg.Name, time.Now,
		func(kind string, a proto.HookAnswer) error {
			data, _ := json.Marshal(a)
			ev := proto.Event{JobID: "hook:" + a.ID, Kind: kind, Data: data}
			if err := h.outbox.add(ev); err != nil {
				return err
			}
			log.Printf("hookrelay: %s for tg %d from %s %s (%d chars)", kind, a.TgMsgID, a.Agent, a.AgtermID, len(a.Text))
			go h.send(proto.Frame{Type: proto.FrameEvent, Event: &ev})
			return nil
		})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return nil, err
	}
	if c, err := net.DialTimeout("unix", socket, 500*time.Millisecond); err == nil {
		c.Close()
		return nil, errors.New("hookrelay: another process listens on " + socket)
	}
	os.Remove(socket) // stale from a previous run
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	h.hbMu.Lock()
	h.hookSessions = r.Sessions
	h.relay = r
	h.hbMu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook", func(w http.ResponseWriter, req *http.Request) {
		var hr hook.Request
		if err := json.NewDecoder(io.LimitReader(req.Body, 16<<20)).Decode(&hr); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := r.Ingest(hr); errors.Is(err, hook.ErrBadRequest) {
			log.Printf("hookrelay: %s %s dropped: %v", hr.Agent, hr.Event, err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		} else if err != nil {
			log.Printf("hookrelay: %s %s: %v", hr.Agent, hr.Event, err)
			http.Error(w, "not handled", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	r.DrainSpool()
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				srv.Close()
				os.Remove(socket)
				return
			case <-t.C:
				r.DrainSpool()
			}
		}
	}()
	log.Printf("hookrelay listening on %s", socket)
	return r, nil
}

// noteTyped tells the hook relay which agterm session a Telegram line went to.
func (h *Host) noteTyped(msgID int64, session string) {
	h.hbMu.Lock()
	r := h.relay
	h.hbMu.Unlock()
	if r != nil {
		r.NoteTyped(msgID, session)
	}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
