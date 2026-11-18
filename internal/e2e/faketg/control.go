package faketg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The control API (/_fake/…), used by scripts/tg.sh. /_fake/cli takes telegram-agent-cli e2e argv and answers its
// JSON envelope; the rest are fake-only.

// Envelope is telegram-agent-cli's JSON output.
type Envelope struct {
	SchemaVersion string    `json:"schema_version"`
	OK            bool      `json:"ok"`
	Data          any       `json:"data,omitempty"`
	Error         *cliError `json:"error,omitempty"`
}

func (s *Server) serveControl(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/_fake/")
	text := func(v string) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, v+"\n")
	}
	switch path {
	case "health":
		text("ok")
	case "token":
		text(s.token)
	case "bot-url":
		text("http://" + r.Host + "/bot" + s.token)
	case "initdata":
		id, err := userArg(r.URL.Query().Get("user"))
		if err != nil {
			http.Error(w, err.Message, http.StatusBadRequest)
			return
		}
		v, ierr := s.InitData(id)
		if ierr != nil {
			http.Error(w, ierr.Error(), http.StatusBadRequest)
			return
		}
		text(v)
	case "reset":
		s.Reset()
		text("ok")
	case "cli":
		args, upload, err := cliRequest(r)
		env := Envelope{SchemaVersion: "1"}
		exit := 0
		if err != nil {
			env.Error, exit = usage("%v", err), 2
		} else if data, cerr := s.CLI(r.Context(), args, upload); cerr != nil {
			env.Error, exit = cerr, cerr.Exit()
		} else {
			env.OK, env.Data = true, data
		}
		w.Header().Set("X-Exit-Code", strconv.Itoa(exit))
		writeJSON(w, http.StatusOK, env)
	default:
		http.NotFound(w, r)
	}
}

// cliRequest reads {"args": [...]} as JSON, or a multipart form with an "args" JSON field and an optional "file".
func cliRequest(r *http.Request) ([]string, []byte, error) {
	var args []string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(maxBody); err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal([]byte(r.FormValue("args")), &args); err != nil {
			return nil, nil, fmt.Errorf("args: %v", err)
		}
		var upload []byte
		if f, _, err := r.FormFile("file"); err == nil {
			defer f.Close()
			if upload, err = io.ReadAll(f); err != nil {
				return nil, nil, err
			}
		}
		return args, upload, nil
	}
	var body struct {
		Args []string `json:"args"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body); err != nil {
		return nil, nil, err
	}
	return body.Args, nil, nil
}

// Reset forgets every message, update, file, call and fault.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	fresh := New(Options{Token: s.token, Now: s.now, After: s.after})
	keep := s.nextUpdate
	for id, c := range fresh.chats { // ids keep counting: a running hub has stored the old ones
		c.nextID, c.onAdd = s.chats[id].nextID, s.saveCounters
	}
	s.chats, s.updates, s.nextUpdate, s.allowed = fresh.chats, nil, keep, nil
	s.files, s.nextFile, s.calls, s.fault = fresh.files, 0, nil, nil
	s.queries, s.nextQuery, s.draftSeq, s.commands, s.menu = fresh.queries, 0, 0, nil, nil
	s.bump()
}

func userArg(v string) (int64, *cliError) {
	switch v {
	case "", "owner", "@" + BotUsername:
		return OwnerID, nil
	case "other":
		return OtherID, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, usage("unknown user %q (owner|other|<id>)", v)
	}
	return n, nil
}

type flagSet struct {
	vals  map[string]string
	bools map[string]bool
	pos   []string
}

func (f *flagSet) int(name string) (int, *cliError) {
	v, ok := f.vals[name]
	if !ok {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, usage("--%s must be a number", name)
	}
	return n, nil
}

func (f *flagSet) seconds(name string, def float64) (time.Duration, *cliError) {
	v, ok := f.vals[name]
	if !ok {
		return time.Duration(def * float64(time.Second)), nil
	}
	x, err := strconv.ParseFloat(v, 64)
	if err != nil || x <= 0 {
		return 0, usage("--%s must be positive", name)
	}
	return time.Duration(x * float64(time.Second)), nil
}

func parseFlags(args []string, valued, booleans string) (*flagSet, *cliError) {
	f := &flagSet{vals: map[string]string{}, bools: map[string]bool{}}
	isIn := func(list, name string) bool { return strings.Contains(" "+list+" ", " "+name+" ") }
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") || a == "--" {
			f.pos = append(f.pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(a[2:], "=")
		switch {
		case isIn(booleans, name):
			f.bools[name] = true
		case isIn(valued, name):
			if !hasVal {
				if i+1 >= len(args) {
					return nil, usage("--%s needs a value", name)
				}
				i++
				val = args[i]
			}
			f.vals[name] = val
		default:
			return nil, usage("unknown flag --%s", name)
		}
	}
	return f, nil
}

var waitKinds = []string{"reply", "edit", "reaction", "pin", "keyboard", "draft"}

const writeWaitFlags = "wait timeout interval"

func (s *Server) botRef() map[string]any {
	return map[string]any{"id": s.bot.ID, "name": BotName, "type": "bot"}
}

// CLI runs one telegram-agent-cli e2e command against the fake and returns its data.
func (s *Server) CLI(ctx context.Context, args []string, upload []byte) (any, *cliError) {
	if len(args) == 0 {
		return nil, usage("a command is required: send|react|press|stop|read|wait|calls|fault")
	}
	switch args[0] {
	case "send":
		return s.cliSend(ctx, args[1:], upload)
	case "react":
		return s.cliReact(ctx, args[1:])
	case "press":
		return s.cliPress(ctx, args[1:])
	case "stop":
		return s.cliStop(args[1:])
	case "read":
		return s.cliRead(args[1:])
	case "wait":
		return s.cliWait(ctx, args[1:])
	case "calls":
		return s.cliCalls(args[1:])
	case "fault":
		return s.cliFault(args[1:])
	}
	return nil, usage("unknown command %q", args[0])
}

// writeWait parses a write command's --wait flags (nil spec without --wait).
func writeWait(f *flagSet) (*waitSpec, *cliError) {
	kind := f.vals["wait"]
	timeout, err := f.seconds("timeout", 30)
	if err != nil {
		return nil, err
	}
	if _, err := f.seconds("interval", 1); err != nil {
		return nil, err
	}
	if kind == "" {
		return nil, nil
	}
	if !contains(waitKinds, kind) {
		return nil, usage("--wait must be one of %s", strings.Join(waitKinds, "|"))
	}
	return &waitSpec{kind: kind, all: f.bools["all"], timeout: timeout}, nil
}

func (s *Server) finish(ctx context.Context, w *watch, out map[string]any) (any, *cliError) {
	if w == nil {
		return out, nil
	}
	r, err := w.wait(ctx)
	if err != nil {
		if err.Code == "timeout" {
			err.Details = map[string]any{"write": out}
		}
		return nil, err
	}
	out["wait"] = r
	return out, nil
}

func (s *Server) cliSend(ctx context.Context, args []string, upload []byte) (any, *cliError) {
	if len(args) == 0 {
		return nil, usage("send text|voice")
	}
	kind := args[0]
	f, err := parseFlags(args[1:], "message path duration reply-to from chat "+writeWaitFlags, "all")
	if err != nil {
		return nil, err
	}
	ws, err := writeWait(f)
	if err != nil {
		return nil, err
	}
	uid, err := pickUser(f)
	if err != nil {
		return nil, err
	}
	replyTo, err := f.int("reply-to")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	out, w, err := s.sendLocked(kind, f, ws, uid, replyTo, upload)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.finish(ctx, w, out)
}

// sendLocked does a send's write; callers hold mu.
func (s *Server) sendLocked(kind string, f *flagSet, ws *waitSpec, uid int64, replyTo int, upload []byte) (map[string]any, *watch, *cliError) {
	c, err := s.userChat(uid)
	if err != nil {
		return nil, nil, err
	}
	var w *watch
	if ws != nil {
		w = s.begin(c, *ws)
	}
	var m *message
	switch kind {
	case "text":
		text := f.vals["message"]
		if len(f.pos) > 0 {
			if text != "" {
				return nil, nil, usage("give the text as an argument or --message, not both")
			}
			text = strings.Join(f.pos, " ")
		}
		m, err = s.userText(c, text, replyTo)
	case "voice":
		if upload == nil {
			return nil, nil, usage("send voice needs the file (scripts/tg.sh uploads --path F or the positional path)")
		}
		dur, derr := f.int("duration")
		if derr != nil {
			return nil, nil, derr
		}
		m, err = s.userVoice(c, upload, dur, replyTo)
	default:
		return nil, nil, usage("send %s: the fake supports text|voice", kind)
	}
	if err != nil {
		return nil, nil, err
	}
	s.bump()
	if w != nil && w.spec.kind == "reaction" {
		w.only(m.ID)
	}
	if m.ID > c.seen {
		c.seen = m.ID
	}
	return map[string]any{"chat": s.botRef(), "sent": s.view(c, m)}, w, nil
}

// pickUser is the fake-only chat selector: --from owner|other (send verbs) or --chat.
func pickUser(f *flagSet) (int64, *cliError) {
	if v := f.vals["from"]; v != "" {
		return userArg(v)
	}
	return userArg(f.vals["chat"])
}

func (s *Server) cliReact(ctx context.Context, args []string) (any, *cliError) {
	f, err := parseFlags(args, "msg emoji from chat "+writeWaitFlags, "remove all")
	if err != nil {
		return nil, err
	}
	if (f.vals["emoji"] == "") == !f.bools["remove"] {
		return nil, usage("give exactly one of --emoji or --remove")
	}
	id, err := f.int("msg")
	if err != nil || id <= 0 {
		return nil, usage("--msg must be a message id")
	}
	ws, err := writeWait(f)
	if err != nil {
		return nil, err
	}
	uid, err := pickUser(f)
	if err != nil {
		return nil, err
	}
	emoji := strings.TrimSuffix(f.vals["emoji"], "️") // the client sends ❤️ as ❤
	s.mu.Lock()
	c, err := s.userChat(uid)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var w *watch
	if ws != nil {
		ws.msg, ws.ignoreMine = id, true
		w = s.begin(c, *ws)
	}
	m, err := s.userReact(c, id, emoji)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.bump()
	out := map[string]any{"chat": s.botRef(), "reacted": map[string]any{"msg": id, "emoji": emoji, "removed": f.bools["remove"]},
		"message": s.view(c, m)}
	s.mu.Unlock()
	return s.finish(ctx, w, out)
}

func (s *Server) cliPress(ctx context.Context, args []string) (any, *cliError) {
	f, err := parseFlags(args, "msg button from chat "+writeWaitFlags, "keyboard all")
	if err != nil {
		return nil, err
	}
	btn, hasBtn := f.vals["button"]
	if !hasBtn {
		return nil, usage("--button is required")
	}
	_, hasMsg := f.vals["msg"]
	if hasMsg == f.bools["keyboard"] {
		return nil, usage("give exactly one of --msg (inline button) or --keyboard")
	}
	ws, err := writeWait(f)
	if err != nil {
		return nil, err
	}
	uid, err := pickUser(f)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	c, err := s.userChat(uid)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if f.bools["keyboard"] {
		_, kb := c.keyboardInEffect()
		if kb == nil {
			s.mu.Unlock()
			return nil, notFound("the chat shows no reply keyboard")
		}
		b, err := pickButton(kb.Rows, btn)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if b.Type != "text" {
			s.mu.Unlock()
			return nil, usage("button %q is a %s button; only plain buttons send their text", b.Text, b.Type)
		}
		var w *watch
		if ws != nil {
			w = s.begin(c, *ws)
		}
		m, err := s.userText(c, b.Text, 0)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		s.bump()
		if w != nil && w.spec.kind == "reaction" {
			w.only(m.ID)
		}
		out := map[string]any{"chat": s.botRef(), "pressed": b, "sent": s.view(c, m)}
		s.mu.Unlock()
		return s.finish(ctx, w, out)
	}
	id, err := f.int("msg")
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var w *watch
	if ws != nil {
		ws.msg = id
		w = s.begin(c, *ws)
	}
	b, qid, err := s.pressInline(c, id, btn)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.bump()
	s.mu.Unlock()
	answer := s.awaitAnswer(ctx, qid, min(ws2timeout(ws), 15*time.Second))
	return s.finish(ctx, w, map[string]any{"chat": s.botRef(), "msg": id, "pressed": b, "answer": answer})
}

func ws2timeout(ws *waitSpec) time.Duration {
	if ws == nil {
		return 15 * time.Second
	}
	return ws.timeout
}

// awaitAnswer waits for the bot's answerCallbackQuery (answered:false when it never comes).
func (s *Server) awaitAnswer(ctx context.Context, qid string, d time.Duration) map[string]any {
	timer := s.after(d)
	for {
		s.mu.Lock()
		q := *s.queries[qid]
		ch := s.changed
		s.mu.Unlock()
		if q.answered {
			out := map[string]any{"answered": true, "message": q.text}
			if q.alert {
				out["alert"] = true
			}
			if q.url != "" {
				out["url"] = q.url
			}
			return out
		}
		select {
		case <-ch:
		case <-timer:
			return map[string]any{"answered": false}
		case <-ctx.Done():
			return map[string]any{"answered": false}
		}
	}
}

func (s *Server) cliStop(args []string) (any, *cliError) {
	f, err := parseFlags(args, "draft from chat", "")
	if err != nil {
		return nil, err
	}
	id, perr := strconv.ParseInt(f.vals["draft"], 10, 64)
	if perr != nil || id == 0 {
		return nil, usage("--draft is required (the draft_id from wait --for draft)")
	}
	uid, err := pickUser(f)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.userChat(uid)
	if err != nil {
		return nil, err
	}
	s.userStop(c, id)
	s.bump()
	return map[string]any{"chat": s.botRef(), "stopped": id}, nil
}

func (s *Server) cliRead(args []string) (any, *cliError) {
	if len(args) == 0 {
		return nil, usage("read messages|keyboard|pinned")
	}
	f, err := parseFlags(args[1:], "since limit from chat", "")
	if err != nil {
		return nil, err
	}
	uid, err := userArg(f.vals["chat"])
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.userChat(uid)
	if err != nil {
		return nil, err
	}
	switch args[0] {
	case "messages":
		since, err := f.int("since")
		if err != nil {
			return nil, err
		}
		limit := 20
		if _, ok := f.vals["limit"]; ok {
			if limit, err = f.int("limit"); err != nil || limit < 1 || limit > 100 {
				return nil, usage("--limit must be 1..100")
			}
		}
		from := f.vals["from"]
		if from == "" {
			from = "any"
		}
		if from != "bot" && from != "me" && from != "any" {
			return nil, usage("--from must be bot, me or any")
		}
		var out []botMessage
		for _, m := range c.msgs {
			if m.Deleted || m.ID <= since || (from == "bot" && !m.FromBot) || (from == "me" && m.FromBot) {
				continue
			}
			out = append(out, s.view(c, m))
		}
		if len(out) > limit {
			out = out[len(out)-limit:]
		}
		if out == nil {
			out = []botMessage{}
		}
		for _, m := range out {
			if m.ID > c.seen {
				c.seen = m.ID
			}
		}
		return map[string]any{"chat": s.botRef(), "messages": out}, nil
	case "keyboard":
		return map[string]any{"chat": s.botRef(), "keyboard": s.keyboardView(c)}, nil
	case "pinned":
		var pinned any
		if m := c.find(c.pinned); m != nil {
			pinned = s.view(c, m)
		}
		return map[string]any{"chat": s.botRef(), "pinned": pinned}, nil
	case "drafts":
		return map[string]any{"chat": s.botRef(), "drafts": draftViews(c.drafts, 0)}, nil
	}
	return nil, usage("read messages|keyboard|pinned|drafts")
}

func (s *Server) keyboardView(c *chat) any {
	m, kb := c.keyboardInEffect()
	if kb == nil {
		return nil
	}
	return map[string]any{"message_id": m.ID, "keyboard": kb}
}

func draftViews(ds []draft, after int) []draftView {
	out := []draftView{}
	for _, d := range ds {
		if d.seq > after {
			out = append(out, draftView{Date: stamp(d.At), DraftID: d.ID, Text: d.Text, CanStop: d.CanStop, KeepOnStop: d.KeepOnStop})
		}
	}
	return out
}

func (s *Server) cliWait(ctx context.Context, args []string) (any, *cliError) {
	f, err := parseFlags(args, "for since msg timeout interval chat", "all")
	if err != nil {
		return nil, err
	}
	kind := f.vals["for"]
	if !contains(waitKinds, kind) {
		return nil, usage("--for must be one of %s", strings.Join(waitKinds, "|"))
	}
	timeout, err := f.seconds("timeout", 30)
	if err != nil {
		return nil, err
	}
	if _, err := f.seconds("interval", 1); err != nil {
		return nil, err
	}
	since, err := f.int("since")
	if err != nil {
		return nil, err
	}
	msg, err := f.int("msg")
	if err != nil {
		return nil, err
	}
	uid, err := userArg(f.vals["chat"])
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	c, err := s.userChat(uid)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if since == 0 && kind == "reply" && c.seen > 0 {
		since = c.seen
	}
	w := s.begin(c, waitSpec{kind: kind, since: since, msg: msg, all: f.bools["all"], timeout: timeout})
	s.mu.Unlock()
	r, err := w.wait(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"chat": s.botRef(), "wait": r}, nil
}

func (s *Server) cliCalls(args []string) (any, *cliError) {
	f, err := parseFlags(args, "method since", "")
	if err != nil {
		return nil, err
	}
	since, err := f.int("since")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Call{}
	for _, c := range s.calls {
		if c.Seq > since && (f.vals["method"] == "" || strings.EqualFold(c.Method, f.vals["method"])) {
			out = append(out, c)
		}
	}
	return map[string]any{"calls": out}, nil
}

// cliFault: fault METHOD [--status 500] [--times N] (N=0: until cleared) | fault none.
func (s *Server) cliFault(args []string) (any, *cliError) {
	f, err := parseFlags(args, "status times", "")
	if err != nil {
		return nil, err
	}
	if len(f.pos) != 1 {
		return nil, usage("fault METHOD|file|*|none [--status 500] [--times 1]")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.pos[0] == "none" {
		s.fault = nil
		return map[string]any{"fault": nil}, nil
	}
	status, times := 500, 1
	if _, ok := f.vals["status"]; ok {
		if status, err = f.int("status"); err != nil || status < 500 || status > 599 {
			return nil, usage("--status must be 5xx")
		}
	}
	if _, ok := f.vals["times"]; ok {
		if times, err = f.int("times"); err != nil || times < 0 {
			return nil, usage("--times must be ≥ 0 (0: until fault none)")
		}
	}
	s.fault = &fault{Method: f.pos[0], Status: status, Times: times}
	return map[string]any{"fault": s.fault}, nil
}
