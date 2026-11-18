package agentcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

// fakeBot is the Bot API: sendMessage numbers messages from 500.
type fakeBot struct {
	mu    sync.Mutex
	calls []map[string]any
	next  int
	fail  string // description of a 400 for every call
}

func (f *fakeBot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := map[string]any{}
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		r.ParseForm()
		for k := range r.PostForm {
			body[k] = r.PostForm.Get(k)
		}
	} else {
		json.NewDecoder(r.Body).Decode(&body)
	}
	body["_method"] = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	f.calls = append(f.calls, body)
	if f.fail != "" {
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"ok":false,"description":%q}`, f.fail)
		return
	}
	f.next++
	fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, 500+f.next)
}

func (f *fakeBot) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c["_method"].(string))
	}
	return out
}

type cliRig struct {
	t      *testing.T
	dir    string
	bot    *fakeBot
	botURL string
	env    map[string]string
	briefs agterm.Env
	out    bytes.Buffer
	errb   bytes.Buffer
}

func newCLIRig(t *testing.T) *cliRig {
	dir := t.TempDir()
	bot := &fakeBot{}
	srv := httptest.NewServer(bot)
	t.Cleanup(srv.Close)
	briefs := filepath.Join(dir, "agent-briefs")
	os.MkdirAll(briefs, 0o755)
	return &cliRig{t: t, dir: dir, bot: bot, botURL: srv.URL, env: map[string]string{},
		briefs: agterm.Env{Home: dir, BriefsRoot: briefs, FallbackInbox: filepath.Join(dir, "inbox")}}
}

// config writes an env file (extra lines appended) and returns its path.
func (c *cliRig) config(extra ...string) string {
	p := filepath.Join(c.dir, "notify.env")
	lines := append([]string{"TELEGRAM_BOT_TOKEN='123:tok'", `TELEGRAM_CHAT_ID="1001"`, "TELEGRAM_API_URL=" + c.botURL,
		"SPOOL_DIR=" + filepath.Join(c.dir, "spool")}, extra...)
	os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	return p
}

func (c *cliRig) sender(stdin string) sender {
	c.out.Reset()
	c.errb.Reset()
	return sender{stdin: strings.NewReader(stdin), stdout: &c.out, stderr: &c.errb,
		env: func(k string) string { return c.env[k] }, cwd: func() (string, error) { return c.dir, nil },
		sleep: func(time.Duration) {}, now: time.Now, briefs: c.briefs}
}

func (c *cliRig) send(args ...string) int {
	return c.sender("").run(context.Background(), args)
}

func TestSendPlainLikeSendPy(t *testing.T) {
	c := newCLIRig(t)
	cfg := c.config()
	long := strings.Repeat("a", 4000) + "\n" + strings.Repeat("b", 200)
	if code := c.send("--env-file", cfg, long, "--silent"); code != 0 {
		t.Fatalf("exit %d: %s", code, c.errb.String())
	}
	if c.out.String() != "sent message_id=501\nsent message_id=502\n" {
		t.Fatalf("output %q", c.out.String())
	}
	c.bot.mu.Lock()
	first := c.bot.calls[0]
	c.bot.mu.Unlock()
	if first["disable_notification"] != "true" || first["text"] != strings.Repeat("a", 4000) || first["chat_id"] != "1001" {
		t.Errorf("first call %v", first["text"] == strings.Repeat("a", 4000))
	}

	// stdin; errors worded as send.py's, exit 1
	if code := c.sender("  from stdin \n").run(context.Background(), []string{"--env-file", cfg, "-"}); code != 0 || c.out.String() != "sent message_id=503\n" {
		t.Fatalf("stdin: %d %q", code, c.out.String())
	}
	for _, tc := range []struct {
		args []string
		err  string
	}{
		{[]string{"--env-file", cfg, "   "}, "error: empty message\n"},
		{[]string{"--env-file", filepath.Join(c.dir, "nope.env"), "x"}, "error: no such file or directory: "},
		{[]string{"--env-file", cfg}, "Send the owner a Telegram message"},
	} {
		if code := c.send(tc.args...); code != 1 || !strings.HasPrefix(c.errb.String(), tc.err) {
			t.Errorf("%v: exit %d %q", tc.args, code, c.errb.String())
		}
	}
	os.WriteFile(filepath.Join(c.dir, "bad.env"), []byte("TELEGRAM_BOT_TOKEN=x\n"), 0o600)
	if code := c.send("--env-file", filepath.Join(c.dir, "bad.env"), "x"); code != 1 ||
		c.errb.String() != "error: TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID missing in "+filepath.Join(c.dir, "bad.env")+"\n" {
		t.Errorf("missing keys: %q", c.errb.String())
	}
	// TELEGRAM_ENV_FROM another env file (its OWNER_CHAT_ID is the chat)
	botEnv := filepath.Join(c.dir, "bot.env")
	os.WriteFile(botEnv, []byte("TELEGRAM_BOT_TOKEN=123:tok\nOWNER_CHAT_ID=1001\nCLAUDE_BIN=x\n"), 0o600)
	os.WriteFile(filepath.Join(c.dir, "from.env"), []byte("TELEGRAM_ENV_FROM="+botEnv+"\nTELEGRAM_API_URL="+c.botURL+"\n"), 0o600)
	if code := c.send("--env-file", filepath.Join(c.dir, "from.env"), "via env-from"); code != 0 || !strings.HasPrefix(c.out.String(), "sent message_id=") {
		t.Errorf("TELEGRAM_ENV_FROM: %d %q %q", code, c.out.String(), c.errb.String())
	}
	c.bot.fail = "Bad Request: chat not found"
	if code := c.send("--env-file", cfg, "x"); code != 1 || c.errb.String() != "error: HTTP 400: Bad Request: chat not found\n" {
		t.Errorf("api error: %q", c.errb.String())
	}
	if strings.Contains(c.errb.String(), "tok") {
		t.Error("token leaked")
	}
}

// fakeHub is the hub's host API.
type fakeHub struct {
	mu    sync.Mutex
	sends []proto.SendRequest
	acks  []proto.AckRequest
	code  int
}

func (f *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("k", 40) || r.Header.Get("X-Hommy-Host") != "mac2" {
		w.WriteHeader(401)
		return
	}
	if f.code != 0 {
		w.WriteHeader(f.code)
		io.WriteString(w, `{"error":"nope"}`)
		return
	}
	switch r.URL.Path {
	case "/api/send":
		var req proto.SendRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.sends = append(f.sends, req)
		fmt.Fprintf(w, `{"ok":true,"chat_id":1001,"message_ids":[%d]}`, 900+len(f.sends))
	case "/api/ack":
		var req proto.AckRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.acks = append(f.acks, req)
		io.WriteString(w, `{"ok":true}`)
	}
}

func (c *cliRig) hubConfig(hubURL string) string {
	key := filepath.Join(c.dir, "host.key")
	os.WriteFile(key, []byte(strings.Repeat("k", 40)+"\n"), 0o600)
	return c.config("HUB_URL="+strings.Replace(hubURL, "http://", "ws://", 1)+"/ws", "HOST_NAME=mac2", "HOST_KEY_FILE="+key)
}

func TestSendAgentThroughHub(t *testing.T) {
	c := newCLIRig(t)
	hub := &fakeHub{}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	brief := filepath.Join(c.briefs.BriefsRoot, "2026-11-01-hc-e2e-agent")
	os.MkdirAll(brief, 0o755)
	os.WriteFile(filepath.Join(brief, "state.json"), []byte(`{"active_agent":"AAAA1111-0000"}`), 0o644)
	c.env["AGTERM_SESSION_ID"] = "AAAA1111-0000"
	c.env["CLAUDE_CODE_BRIDGE_SESSION_ID"] = "session_abc"
	c.env["HOMMY_SEND_ENV"] = c.hubConfig(srv.URL)
	t.Setenv("HOMMY_SEND_ENV", c.env["HOMMY_SEND_ENV"])
	if code := c.send("--agent", "hc-e2e-agent", "--buttons", "Keep|Drop|Answer…", "dealer 3 asks a €500 deposit, ok?"); code != 0 {
		t.Fatalf("exit %d: %s", code, c.errb.String())
	}
	if got := strings.TrimSpace(c.out.String()); got != `{"ok":true,"via":"hub","chat_id":1001,"message_ids":[901]}` {
		t.Fatalf("output %s", got)
	}
	req := hub.sends[0]
	realBrief, _ := filepath.EvalSymlinks(brief)
	if req.Agent == nil || req.Agent.Slug != "hc-e2e-agent" || req.Agent.Session != "AAAA1111-0000" || req.Agent.BriefDir != realBrief ||
		req.Agent.Link != "https://claude.ai/code/session_abc" || strings.Join(req.Buttons, "|") != "Keep|Drop|Answer…" || req.ID == "" {
		t.Fatalf("request %+v %+v", req, req.Agent)
	}
	if len(c.bot.methods()) != 0 {
		t.Error("the hub sends, not the CLI")
	}
	// a 4xx is final: no direct send
	hub.code = 400
	if code := c.send("--agent", "x", "y"); code != 1 || !strings.Contains(c.errb.String(), "nope") || len(c.bot.methods()) != 0 {
		t.Errorf("4xx: %d %q", code, c.errb.String())
	}
}

// A plain send (no --agent) from an agent session names the session for the
// hub, so the owner's reply routes back; the text and output stay send.py's.
func TestSendPlainFromSessionNamesIt(t *testing.T) {
	c := newCLIRig(t)
	hub := &fakeHub{}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	brief := filepath.Join(c.briefs.BriefsRoot, "2026-11-04-acme-e2e")
	os.MkdirAll(brief, 0o755)
	os.WriteFile(filepath.Join(brief, "state.json"), []byte(`{"active_agent":"AAAA1111-0000"}`), 0o644)
	c.env["AGTERM_SESSION_ID"] = "AAAA1111-0000"
	t.Setenv("HOMMY_SEND_ENV", c.hubConfig(srv.URL))
	if code := c.send("Claude needs you\nkill or wait?"); code != 0 {
		t.Fatalf("exit %d: %s", code, c.errb.String())
	}
	req := hub.sends[0]
	if req.Agent == nil || !req.Plain || req.Agent.Session != "AAAA1111-0000" || req.Agent.Slug != "acme-e2e" ||
		req.Text != "Claude needs you\nkill or wait?" {
		t.Fatalf("request %+v %+v", req, req.Agent)
	}
	if got := strings.TrimSpace(c.out.String()); got != "sent message_id=901" {
		t.Errorf("output %q", got)
	}
	// outside any session: a plain message, as before
	delete(c.env, "AGTERM_SESSION_ID")
	os.Remove(filepath.Join(brief, "state.json"))
	if code := c.send("hello"); code != 0 || hub.sends[1].Agent != nil || hub.sends[1].Plain {
		t.Fatalf("exit %d request %+v", code, hub.sends[1])
	}
}

func TestSlugOf(t *testing.T) {
	for in, want := range map[string]string{"acme": "acme", "my repo (2)": "my-repo-2-", "...": "agent", "": "agent", "_x": "x"} {
		if got := slugOf(in); got != want {
			t.Errorf("slugOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSendHubDownGoesDirectAndSpools(t *testing.T) {
	c := newCLIRig(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	dead := srv.URL
	srv.Close() // connection refused: unreachable
	cfg := c.hubConfig(dead)
	c.env["AGTERM_SESSION_ID"] = "AAAA1111-0000"
	if code := c.send("--env-file", cfg, "--agent", "hc-e2e-agent", "--buttons", "A|B", "hub is down"); code != 0 {
		t.Fatalf("exit %d: %s", code, c.errb.String())
	}
	if got := c.bot.methods(); strings.Join(got, ",") != "sendMessage,editMessageReplyMarkup" {
		t.Fatalf("calls %v", got)
	}
	c.bot.mu.Lock()
	text, kb := c.bot.calls[0]["text"], fmt.Sprint(c.bot.calls[1]["reply_markup"])
	c.bot.mu.Unlock()
	if text != "🤖 hc-e2e-agent · mac2\nhub is down" || !strings.Contains(kb, "ag:501:1") {
		t.Errorf("direct: %q %s", text, kb)
	}
	var res sendResult
	json.Unmarshal(c.out.Bytes(), &res)
	if res.Via != "direct" || !res.Spooled || res.MessageIDs[0] != 501 {
		t.Fatalf("result %s", c.out.String())
	}
	files, _ := filepath.Glob(filepath.Join(c.dir, "spool", "*.json"))
	if len(files) != 1 {
		t.Fatalf("spool %v", files)
	}
	var e proto.SpoolEntry
	b, _ := os.ReadFile(files[0])
	json.Unmarshal(b, &e)
	if e.Kind != proto.SpoolSent || e.Bot != 123 || e.Hub != strings.Replace(dead, "http://", "ws://", 1)+"/ws" || e.MessageIDs[0] != 501 ||
		e.Agent.Session != "AAAA1111-0000" || strings.Join(e.Buttons, "|") != "A|B" || e.Texts[0] != text {
		t.Errorf("entry %s", b)
	}
	if filepath.Base(files[0]) != e.ID+".json" {
		t.Errorf("name %s id %s", files[0], e.ID)
	}
}

func (c *cliRig) inbox(args ...string) int {
	c.out.Reset()
	c.errb.Reset()
	ib := inboxer{stdout: &c.out, stderr: &c.errb, env: func(k string) string { return c.env[k] },
		cwd: func() (string, error) { return c.dir, nil }, sleep: func(time.Duration) {}, now: time.Now, briefs: c.briefs}
	return ib.run(context.Background(), args)
}

func TestInboxCommands(t *testing.T) {
	c := newCLIRig(t)
	hub := &fakeHub{}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	t.Setenv("HOMMY_SEND_ENV", c.hubConfig(srv.URL))
	brief := filepath.Join(c.briefs.BriefsRoot, "2026-11-01-hc-e2e-agent")
	os.MkdirAll(brief, 0o755)
	os.WriteFile(filepath.Join(brief, "state.json"), []byte(`{"active_agent":"AAAA1111-0000"}`), 0o644)
	c.env["AGTERM_SESSION_ID"] = "AAAA1111-0000"
	inbox := filepath.Join(brief, "inbox.jsonl")
	add := func(id string, msg int64) {
		if _, err := agterm.AppendInbox(inbox, agterm.InboxRecord{ID: id, MsgID: msg, Text: "t " + id, From: "owner"}); err != nil {
			t.Fatal(err)
		}
	}
	if code := c.inbox("pending"); code != 1 {
		t.Errorf("empty inbox pending: %d", code)
	}
	add("tg-11", 11)
	add("tg-12-r7", 12)
	if code := c.inbox("pending"); code != 0 || strings.TrimSpace(c.out.String()) != `{"pending":2}` {
		t.Errorf("pending: %d %s", code, c.out.String())
	}
	if code := c.inbox("ack", "tg-12-r7"); code != 0 || !strings.Contains(c.out.String(), `"hub":"sent"`) {
		t.Fatalf("ack: %d %s %s", code, c.out.String(), c.errb.String())
	}
	if len(hub.acks) != 1 || hub.acks[0].MsgID != 12 || hub.acks[0].Item != "tg-12-r7" {
		t.Errorf("hub acks %+v", hub.acks)
	}
	c.inbox("list", "--unacked")
	var l struct {
		Inbox string `json:"inbox"`
		Items []Item `json:"items"`
	}
	json.Unmarshal(c.out.Bytes(), &l)
	if len(l.Items) != 1 || l.Items[0].ID != "tg-11" || l.Items[0].Acked {
		t.Errorf("list --unacked %s", c.out.String())
	}
	if code := c.inbox("ack", "tg-404"); code != 1 {
		t.Errorf("unknown id: %d", code)
	}
	c.inbox("ack", "tg-11")
	if code := c.inbox("pending"); code != 1 {
		t.Errorf("all acked: pending %d", code)
	}

	// hub down: the ack is spooled
	add("tg-13", 13)
	hub.code = 503
	if code := c.inbox("ack", "tg-13"); code != 0 || !strings.Contains(c.out.String(), `"hub":"spooled"`) {
		t.Errorf("spooled ack: %s %s", c.out.String(), c.errb.String())
	}

	// wait: returns the next item to arrive, or times out
	n := 0
	ib := inboxer{stdout: &c.out, stderr: &c.errb, env: func(k string) string { return c.env[k] },
		cwd: func() (string, error) { return c.dir, nil }, now: time.Now, briefs: c.briefs,
		sleep: func(time.Duration) {
			if n++; n == 3 {
				add("tg-14", 14)
			}
		}}
	c.out.Reset()
	if code := ib.run(context.Background(), []string{"wait", "--timeout", "30"}); code != 0 || !strings.Contains(c.out.String(), `"event":"owner"`) || !strings.Contains(c.out.String(), `"id":"tg-14"`) {
		t.Errorf("wait: %d %s", code, c.out.String())
	}
	c.out.Reset()
	if code := c.inbox("wait", "--timeout=0.01"); code != 1 || strings.TrimSpace(c.out.String()) != `{"event":"timeout"}` {
		t.Errorf("wait timeout: %d %s", code, c.out.String())
	}

	// wait --pending: an unacked item already there ends it at once (no gap
	// between a separate `pending` check and the wait)
	c.out.Reset()
	if code := c.inbox("wait", "--pending", "--timeout=0.01"); code != 0 || !strings.Contains(c.out.String(), `"event":"owner"`) || !strings.Contains(c.out.String(), `"id":"tg-14"`) {
		t.Errorf("wait --pending with tg-14 unacked: %d %s", code, c.out.String())
	}
	c.inbox("ack", "tg-14")
	c.out.Reset()
	if code := c.inbox("wait", "--pending", "--timeout=0.01"); code != 1 || strings.TrimSpace(c.out.String()) != `{"event":"timeout"}` {
		t.Errorf("wait --pending, all acked: %d %s", code, c.out.String())
	}
}
