package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/AlexDobrushskiy/hommy/internal/host"
	"github.com/AlexDobrushskiy/hommy/internal/ping"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
	"github.com/AlexDobrushskiy/hommy/internal/speech"
)

const ownerUser = int64(1001)

// call is one recorded Bot API call.
type call struct {
	Kind      string // send | voice | react | typing | edit | pin | delete | menu
	Text      string // message text, or the emoji
	MsgID     int    // reacted / edited / pinned / deleted message, or the sent message's id
	ReplyTo   int
	Entities  []models.MessageEntity
	KB        *models.ReplyKeyboardMarkup // the reply keyboard a send carried
	Inline    *models.InlineKeyboardMarkup
	NoPreview bool
	Opts      SendOpts        // SendPing's options
	Buttons   [][]ping.Button // SetButtons
	Query     string          // AnswerCallback's query id
}

type fakeTG struct {
	mu     sync.Mutex
	calls  []call
	nextID int
	files  map[string][]byte
	notify chan struct{}

	failSend     func(text string) error // non-nil error: the send fails and isn't recorded
	failDraft    func(text string) error // non-nil error: the draft fails and isn't recorded
	downloadHook func(ctx context.Context, id string) ([]byte, error)
	failEdit     error
	failPin      error // non-nil: pinChatMessage fails and isn't recorded
	failButtons  error // non-nil: editMessageReplyMarkup fails
	failReact    error // non-nil: setMessageReaction fails and isn't recorded
	meErr        error // non-nil: getMe fails
	kb           func(chatID int64) models.ReplyMarkup
}

func (f *fakeTG) SetKeyboard(fn func(chatID int64) models.ReplyMarkup) {
	f.mu.Lock()
	f.kb = fn
	f.mu.Unlock()
}

func (f *fakeTG) keyboard(chatID int64) *models.ReplyKeyboardMarkup {
	f.mu.Lock()
	fn := f.kb
	f.mu.Unlock()
	if fn == nil {
		return nil
	}
	m, _ := fn(chatID).(*models.ReplyKeyboardMarkup)
	return m
}

func (f *fakeTG) SendText(_ context.Context, chatID int64, text string, ents []models.MessageEntity, replyTo int) (int, error) {
	f.mu.Lock()
	fail := f.failSend
	f.mu.Unlock()
	if fail != nil {
		if err := fail(text); err != nil {
			return 0, err
		}
	}
	return f.add(call{Kind: "send", Text: text, ReplyTo: replyTo, Entities: ents, KB: f.keyboard(chatID)}), nil
}

func (f *fakeTG) SendPing(_ context.Context, chatID int64, text string, o SendOpts) (int, error) {
	f.mu.Lock()
	fail := f.failSend
	f.mu.Unlock()
	if fail != nil {
		if err := fail(text); err != nil {
			return 0, err
		}
	}
	c := call{Kind: "send", Text: text, Opts: o}
	if !o.Bare && len(o.Inline) == 0 {
		c.KB = f.keyboard(chatID)
	}
	return f.add(c), nil
}

func (f *fakeTG) SendOut(_ context.Context, chatID int64, o Out) (int, error) {
	f.mu.Lock()
	fail := f.failSend
	f.mu.Unlock()
	if fail != nil {
		if err := fail(o.Text); err != nil {
			return 0, err
		}
	}
	c := call{Kind: "send", Text: o.Text, ReplyTo: o.ReplyTo, Entities: o.Entities, Inline: o.Inline, NoPreview: o.NoPreview}
	if o.Inline == nil {
		c.KB = f.keyboard(chatID)
	}
	return f.add(c), nil
}

func (f *fakeTG) SetButtons(_ context.Context, _ int64, msgID int, rows [][]ping.Button) error {
	f.mu.Lock()
	fail := f.failButtons
	f.mu.Unlock()
	if fail != nil {
		return fail
	}
	f.add(call{Kind: "buttons", MsgID: msgID, Buttons: rows})
	return nil
}

func (f *fakeTG) setFailButtons(err error) {
	f.mu.Lock()
	f.failButtons = err
	f.mu.Unlock()
}

func (f *fakeTG) setFailReact(err error) {
	f.mu.Lock()
	f.failReact = err
	f.mu.Unlock()
}

func (f *fakeTG) AnswerCallback(_ context.Context, id, text string) error {
	f.add(call{Kind: "answer", Text: text, Query: id})
	return nil
}

func (f *fakeTG) SendBare(_ context.Context, _ int64, text string, ents []models.MessageEntity) (int, error) {
	return f.add(call{Kind: "send", Text: text, Entities: ents}), nil
}

func (f *fakeTG) Unpin(_ context.Context, _ int64, msgID int) error {
	f.add(call{Kind: "unpin", MsgID: msgID})
	return nil
}

func (f *fakeTG) EditText(_ context.Context, _ int64, msgID int, text string, ents []models.MessageEntity) error {
	f.mu.Lock()
	err := f.failEdit
	for _, c := range f.calls {
		if c.Kind == "send" && c.MsgID == msgID && c.KB != nil {
			err = fmt.Errorf("%w, Bad Request: message can't be edited", bot.ErrorBadRequest)
		}
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.add(call{Kind: "edit", Text: text, MsgID: msgID, Entities: ents})
	return nil
}

func (f *fakeTG) Pin(_ context.Context, _ int64, msgID int) error {
	f.mu.Lock()
	err := f.failPin
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.add(call{Kind: "pin", MsgID: msgID})
	return nil
}

func (f *fakeTG) Delete(_ context.Context, _ int64, msgID int) error {
	f.add(call{Kind: "delete", MsgID: msgID})
	return nil
}

func (f *fakeTG) SetMenuButton(_ context.Context, _ int64, text, url string) error {
	f.add(call{Kind: "menu", Text: text + " " + url})
	return nil
}

func (f *fakeTG) setFailSend(fn func(text string) error) {
	f.mu.Lock()
	f.failSend = fn
	f.mu.Unlock()
}

func (f *fakeTG) setDownloadHook(fn func(ctx context.Context, id string) ([]byte, error)) {
	f.mu.Lock()
	f.downloadHook = fn
	f.mu.Unlock()
}

func newFakeTG() *fakeTG {
	return &fakeTG{nextID: 1000, files: map[string][]byte{}, notify: make(chan struct{}, 1000)}
}

func (f *fakeTG) add(c call) int {
	f.mu.Lock()
	if c.Kind == "send" || c.Kind == "voice" {
		f.nextID++
		c.MsgID = f.nextID
	}
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	f.notify <- struct{}{}
	return c.MsgID
}

func (f *fakeTG) Send(ctx context.Context, chatID int64, text string, replyTo int) (int, error) {
	return f.SendText(ctx, chatID, text, nil, replyTo)
}
func (f *fakeTG) SendVoice(_ context.Context, chatID int64, ogg []byte, replyTo int) (int, error) {
	return f.add(call{Kind: "voice", Text: string(ogg), ReplyTo: replyTo, KB: f.keyboard(chatID)}), nil
}
func (f *fakeTG) React(_ context.Context, _ int64, msgID int, emoji string) error {
	f.mu.Lock()
	fail := f.failReact
	f.mu.Unlock()
	if fail != nil {
		return fail
	}
	f.add(call{Kind: "react", Text: emoji, MsgID: msgID})
	return nil
}
func (f *fakeTG) Draft(_ context.Context, _ int64, draftID int64, text string) error {
	f.mu.Lock()
	fail := f.failDraft
	f.mu.Unlock()
	if fail != nil {
		if err := fail(text); err != nil {
			return err
		}
	}
	f.add(call{Kind: "draft", Text: text, MsgID: int(draftID)})
	return nil
}

func (f *fakeTG) setFailDraft(fn func(text string) error) {
	f.mu.Lock()
	f.failDraft = fn
	f.mu.Unlock()
}

func (f *fakeTG) Typing(context.Context, int64) error { return nil }
func (f *fakeTG) Download(ctx context.Context, id string) ([]byte, error) {
	f.mu.Lock()
	hook := f.downloadHook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.files[id]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("no file %s", id)
}
func (f *fakeTG) SetCommands(_ context.Context, cmds []models.BotCommand) error {
	f.add(call{Kind: "commands", Text: fmt.Sprint(len(cmds))})
	return nil
}
func (f *fakeTG) Ping(context.Context) error { return nil }
func (f *fakeTG) Me(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.meErr != nil {
		return "", f.meErr
	}
	return testBotName, nil
}

func (f *fakeTG) snapshot() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// waitFor blocks until pred holds for the recorded calls.
func (f *fakeTG) waitFor(t *testing.T, what string, pred func([]call) bool) []call {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if cs := f.snapshot(); pred(cs) {
			return cs
		}
		select {
		case <-f.notify:
		case <-deadline:
			t.Fatalf("timed out waiting for %s; calls: %+v", what, f.snapshot())
		}
	}
}

func count(cs []call, kind, substr string) int {
	n := 0
	for _, c := range cs {
		if c.Kind == kind && strings.Contains(c.Text, substr) {
			n++
		}
	}
	return n
}

func has(kind, substr string) func([]call) bool {
	return func(cs []call) bool { return count(cs, kind, substr) > 0 }
}

type fakeSpeech struct {
	mu      sync.Mutex
	synths  []string
	failSTT error
	gate    chan struct{} // non-nil: Synthesize waits until it is closed
	sttGate chan struct{} // non-nil: Transcribe waits until it is closed
}

func (s *fakeSpeech) Transcribe(context.Context, []byte) (string, string, error) {
	s.mu.Lock()
	g := s.sttGate
	s.mu.Unlock()
	if g != nil {
		<-g
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSTT != nil {
		return "", "", s.failSTT
	}
	return "what is two plus two", "mac2", nil
}

func (s *fakeSpeech) setFailSTT(err error) {
	s.mu.Lock()
	s.failSTT = err
	s.mu.Unlock()
}

func (s *fakeSpeech) Synthesize(ctx context.Context, b speech.TTSBackend, text string) ([]byte, error) {
	s.mu.Lock()
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	s.synths = append(s.synths, b.Voice+":"+text)
	s.mu.Unlock()
	return []byte("OggS" + text), nil
}
func (s *fakeSpeech) ListVoices(context.Context) ([]string, error) {
	return []string{"af_heart", "am_michael"}, nil
}

// clock is a settable time source for health checks.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const hostKey = "k-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

const testBotName = "hommy_test_bot"

// rig is a hub on a real listener plus helpers to run a real host against it.
type rig struct {
	t      *testing.T
	dir    string
	tg     *fakeTG
	sp     *fakeSpeech
	clk    *clock
	hub    *Hub
	st     *Store
	url    string
	stop   context.CancelFunc
	done   chan struct{}
	nextMs int

	unpaired bool          // the hub starts without an owner (pairing tests)
	mod      func(*Config) // changes testConfig() before the hub starts
}

func testConfig() Config {
	return Config{
		TelegramToken: "123456:test-token", MiniAppURL: "https://agents.test",
		HostKeys:      map[string]string{"mac1": hostKey, "mac2": hostKey + "-mac2"},
		DefaultTarget: "vault@mac1", SilentAfter: 2 * time.Minute, QueuedNotice: 2 * time.Minute, PairingTTL: 30 * time.Minute,
		DraftEvery: time.Millisecond, DraftKeepalive: time.Hour,
		TTS:        speech.TTSConfig{URL: "http://tts/v1", Model: "kokoro", DefaultVoice: "af_heart", MinChars: 400},
		Transcribe: speech.TranscribeConfig{Endpoints: speech.ParseWhisperEndpoints(speech.DefaultWhisperEndpoints)},
	}
}

// newRig is a hub paired with ownerUser (as after `/start <code>`).
func newRig(t *testing.T) *rig {
	r := &rig{t: t, dir: t.TempDir(), tg: newFakeTG(), sp: &fakeSpeech{},
		clk: &clock{t: time.Now()}, nextMs: 100}
	r.startHub()
	return r
}

// newUnpairedRig is a hub that has no owner yet.
func newUnpairedRig(t *testing.T) *rig {
	r := &rig{t: t, dir: t.TempDir(), tg: newFakeTG(), sp: &fakeSpeech{},
		clk: &clock{t: time.Now()}, nextMs: 100, unpaired: true}
	r.startHub()
	return r
}

func (r *rig) startHub() {
	st, err := OpenStore(filepath.Join(r.dir, "hub.sqlite3"))
	if err != nil {
		r.t.Fatal(err)
	}
	r.st = st
	if o, err := st.Owner(); err == nil && !o.Paired() && !r.unpaired {
		if err := st.SetOwner(Owner{UserID: ownerUser, ChatID: ownerUser, FirstName: "Owner", PairedAt: r.clk.Now()}); err != nil {
			r.t.Fatal(err)
		}
	}
	cfg := testConfig()
	if r.mod != nil {
		r.mod(&cfg)
	}
	r.hub = New(cfg, st, r.tg, r.sp, r.clk.Now)
	if !r.unpaired {
		r.hub.botSetUp = true // commands and menu button were set when the owner paired
	}
	r.hub.retryAfter = func(int) time.Duration { return time.Millisecond }
	addr := "127.0.0.1:0"
	if r.url != "" { // a restart comes back where running hosts reconnect
		addr = strings.TrimSuffix(strings.TrimPrefix(r.url, "ws://"), "/ws")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		r.t.Fatal(err)
	}
	r.url = "ws://" + ln.Addr().String() + "/ws"
	ctx, cancel := context.WithCancel(context.Background())
	r.stop, r.done = cancel, make(chan struct{})
	go func() { r.hub.Run(ctx, ln); close(r.done) }()
	r.t.Cleanup(r.stopHub)
	// Run serves only after its start-up recovery (re-delivering stored results):
	// a test that stores a result before then would see it delivered twice.
	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		r.t.Fatal(err)
	}
	_ = resp.Body.Close()
}

func (r *rig) stopHub() {
	if r.stop == nil {
		return
	}
	r.stop()
	<-r.done
	r.stop = nil
	r.st.Close()
}

// fakeClaude is a stream-json claude stand-in: the reply echoes the prompt,
// the cwd and whether --resume was passed; the session id is fixed per run.
// Prompts with GATE stream "1" and wait for the file $HC_TEST_GATE; SLOW
// streams "partial" and hangs (until killed).
const fakeClaude = `#!/bin/sh
prompt=$(awk 'BEGIN{ORS="\\n"} {print}' | sed 's/\\n$//')
resume=none
while [ $# -gt 0 ]; do [ "$1" = "--resume" ] && resume=$2; shift; done
sid="sess-$$"
case "$prompt" in *LONG*) extra=' <tts>spoken version</tts>';; *) extra='';; esac
ev() { printf '{"type":"stream_event","event":%s,"session_id":"%s","parent_tool_use_id":null}\n' "$1" "$sid"; }
delta() { ev "{\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"$1\"}}"; }
printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$sid"
ev '{"type":"message_start"}'
ev '{"type":"content_block_start","content_block":{"type":"text","text":""}}'
case "$prompt" in
*GATE*) delta '1'; while [ ! -f "$HC_TEST_GATE" ]; do sleep 0.02; done; delta ' 2';;
*SLOW*) delta 'partial'; exec sleep 300;;
esac
printf '{"type":"result","subtype":"success","is_error":false,"result":"answer to [%s] in %s resume=%s%s","session_id":"%s"}\n' "$prompt" "$(pwd -P)" "$resume" "$extra" "$sid"
`

type hostRig struct {
	h    *host.Host
	stop context.CancelFunc
	done chan struct{}
	cfg  host.Config
}

func (r *rig) hostConfig() host.Config {
	bin := filepath.Join(r.dir, "claude")
	os.WriteFile(bin, []byte(fakeClaude), 0o755)
	vault := filepath.Join(r.dir, "vault")
	os.MkdirAll(vault, 0o755)
	return host.Config{Name: "mac1", HubURL: r.url, Key: hostKey, DataDir: filepath.Join(r.dir, "host"),
		VaultDir: vault, MediaDir: filepath.Join(r.dir, "media"), ClaudeBin: bin, ClaudeTimeout: 10 * time.Second,
		Home: r.dir, AgentBriefsDir: r.dir, HeartbeatEvery: time.Hour, NewRoots: []string{filepath.Join(r.dir, "work")}}
}

func (r *rig) startHost() *hostRig {
	cfg := r.hostConfig()
	h, err := host.New(cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	hr := &hostRig{h: h, stop: cancel, done: make(chan struct{}), cfg: cfg}
	go func() { h.Run(ctx); close(hr.done) }()
	r.waitConnected(true)
	r.waitHeartbeat(func(proto.Heartbeat) bool { return true })
	r.t.Cleanup(hr.shutdown)
	return hr
}

// waitHeartbeat waits until the hub has stored a heartbeat from mac1 that ok
// accepts. A host sends one when it connects (then hourly in tests): a test
// that changes host state or injects a heartbeat must not race that one.
func (r *rig) waitHeartbeat(ok func(proto.Heartbeat) bool) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		row, err := r.st.Host("mac1")
		if err == nil && row.Heartbeat.Host == "mac1" && ok(row.Heartbeat) {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("no matching heartbeat from mac1 (last %+v, %v)", row.Heartbeat, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (hr *hostRig) shutdown() {
	if hr.stop == nil {
		return
	}
	hr.stop()
	<-hr.done
	hr.h.Close()
	hr.stop = nil
}

func (r *rig) waitConnected(want bool) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for r.hub.connected("mac1") != want {
		if time.Now().After(deadline) {
			r.t.Fatalf("host connected != %v", want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// send feeds an update from the owner as Telegram would and returns its message id.
func (r *rig) send(fill func(m *models.Message)) int {
	r.nextMs++
	m := &models.Message{ID: r.nextMs, From: &models.User{ID: ownerUser}, Chat: models.Chat{ID: ownerUser, Type: models.ChatTypePrivate}}
	fill(m)
	r.hub.intake(&models.Update{ID: int64(r.nextMs), Message: m})
	return m.ID
}

func (r *rig) text(s string) int {
	return r.send(func(m *models.Message) { m.Text = s })
}

func (r *rig) answers(cs []call) []call {
	var out []call
	for _, c := range cs {
		if c.Kind == "send" && strings.HasPrefix(c.Text, "answer to [") {
			out = append(out, c)
		}
	}
	return out
}

func mustJSONRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
