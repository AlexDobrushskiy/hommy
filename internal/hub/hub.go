// Package hub is `hommy hub`: the only getUpdates poller, SQLite state,
// the owner's pairing, and the WebSocket the hosts connect to.
package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
	"github.com/AlexDobrushskiy/hommy/internal/speech"
)

// Speech is transcription and synthesis (faked in tests).
type Speech interface {
	Transcribe(ctx context.Context, audio []byte) (text, source string, err error)
	Synthesize(ctx context.Context, b speech.TTSBackend, text string) ([]byte, error)
	ListVoices(ctx context.Context) ([]string, error)
}

type realSpeech struct{ cfg Config }

func (s realSpeech) Transcribe(ctx context.Context, audio []byte) (string, string, error) {
	return speech.Transcribe(ctx, s.cfg.Transcribe, audio)
}

func (s realSpeech) Synthesize(ctx context.Context, b speech.TTSBackend, text string) ([]byte, error) {
	return speech.Synthesize(ctx, s.cfg.TTS.Timeout, b, text)
}

func (s realSpeech) ListVoices(ctx context.Context) ([]string, error) {
	return speech.ListVoices(ctx, s.cfg.TTS.URL)
}

// ResultHandler turns a job's result into chat output. An error means "try
// again later": the handler runs again (also after a hub restart) until it
// returns nil, so it sends each chat message through h.part, which skips the
// ones already sent.
type ResultHandler func(ctx context.Context, j *JobRow, r proto.Result) error

// Hub wires Telegram, the store and the host connections.
type Hub struct {
	cfg     Config
	st      *Store
	tg      Telegram
	sp      Speech
	now     func() time.Time
	started time.Time
	rootMu  sync.Mutex
	root    context.Context // outlives host connections; set by Run (read via rootCtx)

	wakeEvents chan struct{}
	wakeStops  chan struct{}
	kick       chan struct{}
	repush     chan string

	mu      sync.Mutex
	conns   map[string]*hostConn
	typing  map[int64]int // chat → jobs started and not yet delivered
	results map[string]ResultHandler

	waitMu  sync.Mutex
	waiters map[string]chan proto.Result // API jobs (console peek) → their caller

	streamsMu    sync.Mutex
	streams      map[string]*draftStream // job id → its live draft
	streamClosed map[string]closedStream // job ids whose stream ended (never reopened)

	eventsMu sync.Mutex // one drainEvents at a time: events are processed in order, once
	stopsMu  sync.Mutex // one drainStops at a time
	healthMu sync.Mutex // one checkHealth at a time, so an alert goes out once

	spoolGen atomic.Int64 // bumped when a spool entry records agent messages (parked events look again)
	sendMu   sync.Mutex   // agent sends (/api/send), one at a time: a retried request id never sends twice
	pauseMu  sync.Mutex   // a ⏸ message and its end (hook_waiting / hook_resumed)

	kbMu       sync.Mutex // keyboard generation (picker token serial)
	limit      *rateLimit // Mini App API
	writeLimit *rateLimit // Mini App console actions (talk, pause, done)

	tgMu     sync.Mutex
	tgOK     time.Time // last successful Bot API call (for /healthz)
	tgUnauth bool      // the Bot API refused the token on its last answer
	botName  string    // the bot's username (getMe), once known
	botSetUp bool      // commands (and menu button) set for the current owner

	ownerMu sync.Mutex
	owner   Owner // the paired owner (zero = unpaired), as stored

	retryAfter func(attempt int) time.Duration // delay before retrying a failed delivery or intake

	bg sync.WaitGroup // loops and deliveries; Run returns once they have stopped
}

// New builds a hub around an open store. tg and sp may be fakes.
func New(cfg Config, st *Store, tg Telegram, sp Speech, now func() time.Time) *Hub {
	if now == nil {
		now = time.Now
	}
	h := &Hub{cfg: cfg, st: st, sp: sp, now: now, started: now(), root: context.Background(),
		wakeEvents: make(chan struct{}, 1), wakeStops: make(chan struct{}, 1), kick: make(chan struct{}, 1), repush: make(chan string, 16),
		conns: map[string]*hostConn{}, typing: map[int64]int{}, results: map[string]ResultHandler{}, waiters: map[string]chan proto.Result{},
		streams: map[string]*draftStream{}, streamClosed: map[string]closedStream{},
		retryAfter: defaultRetryAfter, limit: newRateLimit(miniAppRateLimit), writeLimit: newRateLimit(miniAppWriteLimit)}
	o, err := st.Owner()
	if err != nil {
		log.Printf("owner: %v", err)
	}
	h.owner = o
	if tg != nil {
		h.tg = ownerGuard{tg: tg, owner: h.ownerChat}
		h.tg.SetKeyboard(h.markup)
	}
	h.OnResult(proto.JobRunP, h.deliverRunP)
	h.OnResult(proto.JobSaveFile, h.deliverSaveFile)
	h.registerSessionResults()
	h.registerHookResults()
	h.OnResult(proto.JobCancel, func(context.Context, *JobRow, proto.Result) error { return nil })
	for name := range cfg.HostKeys {
		if err := st.EnsureHost(name); err != nil {
			log.Printf("host %s: %v", name, err)
		}
	}
	return h
}

// OnResult registers how a job type's results reach the chat.
func (h *Hub) OnResult(jobType string, fn ResultHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.results[jobType] = fn
}

// Run serves the host API and processes events until ctx ends. The Telegram
// poller is started by the caller (Main) so tests can drive intake directly.
func (h *Hub) Run(ctx context.Context, ln net.Listener, more ...net.Listener) error {
	h.rootMu.Lock()
	h.root = ctx
	h.rootMu.Unlock()
	h.recover(ctx)
	srv := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
		h.mu.Lock()
		for _, hc := range h.conns { // Shutdown leaves hijacked (WebSocket) connections open
			hc.c.Close(websocket.StatusGoingAway, "hub shutting down")
		}
		h.mu.Unlock()
	}()
	for _, loop := range []func(context.Context){h.eventLoop, h.stopLoop, h.dispatchLoop, h.healthLoop, h.typingLoop} {
		h.bg.Add(1)
		go func() { defer h.bg.Done(); loop(ctx) }()
	}
	for _, l := range more {
		go func() {
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("listen %s: %v", l.Addr(), err)
			}
		}()
	}
	err := srv.Serve(ln)
	h.bg.Wait() // nothing touches the store once Run returns
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// goDeliver delivers a job's result in the background.
func (h *Hub) goDeliver(j *JobRow) {
	h.bg.Add(1)
	go func() { defer h.bg.Done(); h.deliver(h.rootCtx(), j) }()
}

func (h *Hub) rootCtx() context.Context {
	h.rootMu.Lock()
	defer h.rootMu.Unlock()
	return h.root
}

// defaultRetryAfter backs off from 1 s to a minute.
func defaultRetryAfter(attempt int) time.Duration {
	return min(time.Second<<min(attempt, 6), time.Minute)
}

// sleepRetry waits before retry attempt+1; false when ctx ended.
func (h *Hub) sleepRetry(ctx context.Context, attempt int) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(h.retryAfter(attempt)):
		return true
	}
}

// Handler is the hub's HTTP surface: /ws for hosts, /healthz, and the
// host-key API (agents' sends, pairing, status).
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.serveWS)
	mux.HandleFunc("/healthz", h.serveHealthz)
	mux.HandleFunc("/api/send", h.serveSend)
	mux.HandleFunc("/api/ack", h.serveAck)
	mux.HandleFunc("/api/pairing", h.servePairing)
	mux.HandleFunc("/api/status", h.serveStatus)
	return mux
}

// recover resumes work a restart interrupted: deliveries in flight and the
// typing indicator for runs still going on a host.
func (h *Hub) recover(ctx context.Context) {
	if jobs, err := h.st.JobsInStatus(JobResult); err == nil {
		for _, j := range jobs {
			h.addTyping(j.ChatID)
			h.goDeliver(j)
		}
	}
	if jobs, err := h.st.JobsInStatus(JobStarted); err == nil {
		for _, j := range jobs {
			h.addTyping(j.ChatID)
		}
	}
	h.wake()
}

func (h *Hub) wake() {
	for _, c := range []chan struct{}{h.wakeEvents, h.wakeStops} {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

func (h *Hub) kickDispatch() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// intake stores an authorized update before anything else happens to it.
// Rejects are logged and dropped without a reply. An error means the update
// is not stored, so the poller must not confirm it to Telegram.
func (h *Hub) intake(u *models.Update) error {
	h.markTelegramOK()
	if h.cfg.LogUpdates {
		if raw, err := json.Marshal(u); err == nil {
			log.Printf("update %s", raw)
		}
	}
	if handled, err := h.pairingGate(h.rootCtx(), u); handled || err != nil {
		return err
	}
	e, ok, why := classify(u, h.ownerRow())
	if !ok {
		dropped(u, why)
		return nil
	}
	status := EventReceived
	if e.Kind == "edited_message" {
		status = EventIgnored // edits are kept, not routed
	}
	added, err := h.st.AddEvent(e, status, h.now())
	if err != nil {
		log.Printf("store update %d: %v", u.ID, err)
		return err
	}
	if !added {
		log.Printf("update %d (%s) already stored", u.ID, e.EventID)
		return nil
	}
	if status == EventIgnored {
		log.Printf("%s %s stored, not routed", e.Kind, e.EventID)
	}
	if u.CallbackQuery != nil {
		h.answerCallback(h.rootCtx(), u.CallbackQuery) // the phone spins until answered
	}
	h.wake()
	return nil
}

func (h *Hub) markTelegramOK() {
	h.tgMu.Lock()
	h.tgOK = h.now()
	h.tgUnauth = false
	h.tgMu.Unlock()
}

// telegramFailed notes a Bot API error; a refused token shows in /api/status.
func (h *Hub) telegramFailed(err error) {
	if errors.Is(err, bot.ErrorUnauthorized) {
		h.tgMu.Lock()
		h.tgUnauth = true
		h.tgMu.Unlock()
	}
}

// telegramState is /api/status's telegram: ok | stale | unauthorized.
func (h *Hub) telegramState() string {
	h.tgMu.Lock()
	defer h.tgMu.Unlock()
	switch {
	case h.tgUnauth:
		return "unauthorized"
	case !h.tgOK.IsZero() && h.now().Sub(h.tgOK) < 3*time.Minute:
		return "ok"
	}
	return "stale"
}
