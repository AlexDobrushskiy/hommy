package hub

// The console (a session's page in the Mini App: from a board row, an agent
// ping's or a ⏸ ping's "Open"): read-only terminal peek, the brief folder's
// files, "Talk to …", Pause / Done. Reads and writes authenticate with
// initData (never the picker token); writes have their own rate limit.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/agterm"
	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

const (
	consoleLines      = 80 // terminal rows the console shows
	peekWait          = 10 * time.Second
	miniAppWriteLimit = 10 // console actions per minute
	maxFileTail       = 400
	kindMiniApp       = "miniapp" // an event a console action stored (processed in order with messages)
)

type consoleResp struct {
	Target    string `json:"target"`
	Label     string `json:"label"`
	Name      string `json:"name,omitempty"`
	Host      string `json:"host"`
	Online    bool   `json:"online"`
	Live      bool   `json:"live"` // in the host's latest session list
	Agent     string `json:"agent,omitempty"`
	State     string `json:"state,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	BriefDir  string `json:"brief_dir,omitempty"`
	Paused    bool   `json:"paused,omitempty"`
	Status    string `json:"status,omitempty"`
	Unacked   int    `json:"unacked"`
	Current   bool   `json:"current"` // the chat's target now
	Link      string `json:"link,omitempty"`
	Text      string `json:"text,omitempty"`
	PeekError string `json:"peek_error,omitempty"`
}

// consoleTarget reads ?target= (or a JSON body's target): a session on one
// of our hosts, with a short id expanded.
func (h *Hub) consoleTarget(spec string) (Target, error) {
	t, err := ParseTarget(spec)
	if err != nil || t.Kind != KindSession || t.Dir == "" {
		return t, errors.New("target must be session@<host>:<id>")
	}
	if _, ok := h.cfg.HostKeys[t.Host]; !ok {
		return t, errors.New("unknown host")
	}
	if s, ok := h.liveSession(t); ok {
		t.Dir = strings.ToUpper(s.ID)
	}
	return t, nil
}

func bad(err error) (int, any) {
	return http.StatusBadRequest, map[string]string{"error": err.Error()}
}

func (h *Hub) consoleAPI(chatID int64, r *http.Request) (int, any) {
	t, err := h.consoleTarget(r.URL.Query().Get("target"))
	if err != nil {
		return bad(err)
	}
	resp := consoleResp{Target: t.String(), Label: h.label(t), Host: t.Host, Online: h.connected(t.Host), Link: h.sessionLink(t)}
	if cur, err := h.currentTarget(chatID); err == nil {
		resp.Current = strings.EqualFold(cur.String(), t.String())
	}
	if snap, ok := h.snapshot(t.Host); ok {
		if s, n := findSession(snap.Sessions, t.Dir); n == 1 {
			resp.Live, resp.Name = true, sessionName(s)
			resp.Agent, resp.State, resp.Cwd, resp.BriefDir, resp.Unacked = s.Agent, s.State, s.Cwd, s.BriefDir, s.Unacked
			if f, ok := folderOf(s.BriefDir, snap.Folders); ok {
				resp.Paused, resp.Status = f.Paused, f.Status
			}
		}
	}
	if r.URL.Query().Get("peek") == "0" {
		return http.StatusOK, resp
	}
	if !resp.Online {
		resp.PeekError = t.Host + " is offline"
		return http.StatusOK, resp
	}
	ctx, cancel := context.WithTimeout(r.Context(), peekWait)
	defer cancel()
	payload, _ := json.Marshal(agterm.PeekRequest{Session: t.Dir, Lines: consoleLines, Screen: true})
	res, err := h.jobNow(ctx, t, proto.JobPeek, payload)
	var p agterm.PeekResult
	_ = json.Unmarshal(res.Data, &p)
	switch {
	case err != nil:
		resp.PeekError = err.Error()
	case res.Status != proto.StatusOK:
		resp.PeekError = firstNonEmpty(p.Error, strings.TrimSpace(res.Status+" "+res.Error))
	default:
		resp.Text = p.Text
		if p.Composer.State != "" {
			resp.State = string(p.Composer.State)
		}
	}
	return http.StatusOK, resp
}

type filesResp struct {
	BriefDir  string             `json:"brief_dir,omitempty"`
	Path      string             `json:"path,omitempty"`
	Content   string             `json:"content,omitempty"`
	Truncated bool               `json:"truncated,omitempty"`
	Files     []agterm.FileEntry `json:"files,omitempty"`
	Error     string             `json:"error,omitempty"`
}

// validRel is a path the console may ask for: relative, inside the folder
// (the host checks again, symlinks included).
func validRel(p string) bool {
	if p == "" || len(p) > 200 || strings.ContainsAny(p, "\x00\\") || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// filesAPI lists the session's brief folder (no path) or returns one file
// (tail = its last lines).
func (h *Hub) filesAPI(_ int64, r *http.Request) (int, any) {
	q := r.URL.Query()
	t, err := h.consoleTarget(q.Get("target"))
	if err != nil {
		return bad(err)
	}
	req := agterm.BriefRequest{Session: t.Dir, Path: q.Get("path")}
	if req.Path != "" && !validRel(req.Path) {
		return http.StatusBadRequest, filesResp{Error: agterm.ErrPathRefused.Error()}
	}
	if v := q.Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxFileTail {
			return bad(fmt.Errorf("tail must be 0..%d", maxFileTail))
		}
		req.Tail = n
	}
	if !h.connected(t.Host) {
		return http.StatusServiceUnavailable, filesResp{Error: t.Host + " is offline"}
	}
	ctx, cancel := context.WithTimeout(r.Context(), peekWait)
	defer cancel()
	payload, _ := json.Marshal(req)
	res, err := h.jobNow(ctx, t, proto.JobFiles, payload)
	if err != nil {
		return http.StatusGatewayTimeout, filesResp{Error: err.Error()}
	}
	var d agterm.BriefResult
	_ = json.Unmarshal(res.Data, &d)
	out := filesResp{BriefDir: d.BriefDir, Path: d.Path, Content: d.Content, Truncated: d.Truncated, Files: d.Files}
	if res.Status != proto.StatusOK {
		out.Error = firstNonEmpty(d.Error, res.Error, res.Status)
		return http.StatusOK, out
	}
	return http.StatusOK, out
}

type actionReq struct {
	Target string `json:"target"`
}

// writeAPI is apiReq for console actions: POST with a small JSON body, and a
// second, stricter per-user rate limit.
func (h *Hub) writeAPI(fn func(chatID int64, ctx context.Context, t Target) (int, any)) http.HandlerFunc {
	return h.apiReq(false, func(chatID int64, r *http.Request) (int, any) {
		if !h.writeLimit.allow("user:"+strconv.FormatInt(chatID, 10), h.now()) {
			return http.StatusTooManyRequests, map[string]string{"error": "rate limited"}
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			return http.StatusUnsupportedMediaType, map[string]string{"error": "JSON body expected"}
		}
		var req actionReq
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			return bad(errors.New("bad request"))
		}
		t, err := h.consoleTarget(req.Target)
		if err != nil {
			return bad(err)
		}
		return fn(chatID, r.Context(), t)
	})
}

// talkAPI makes the session the chat's target, the way the picker does: a
// stored event, processed in order with the chat's messages (the switch
// notice brings the new keyboard, pin and input hint).
func (h *Hub) talkAPI(chatID int64, _ context.Context, t Target) (int, any) {
	if h.sessionGone(t) {
		return http.StatusConflict, map[string]string{"error": h.label(t) + " is gone"}
	}
	data := mustJSON(pickerChoice{V: 1, Target: t.String()})
	id := "miniapp:" + newID()
	e := Event{EventID: id, ChatID: chatID, Kind: kindMiniApp, Raw: []byte(data)}
	if _, err := h.st.AddEvent(e, EventReceived, h.now()); err != nil {
		log.Printf("talk %s: %v", t, err)
		return http.StatusInternalServerError, map[string]string{"error": "couldn't store the switch"}
	}
	log.Printf("mini app: talk to %s (%s)", t, id)
	h.wake()
	return http.StatusOK, map[string]any{"ok": true, "target": t.String(), "label": h.label(t)}
}

// handleMiniAppEvent applies a stored console action (only "talk to" so far).
func (h *Hub) handleMiniAppEvent(ctx context.Context, e *Event) error {
	return h.pickerSwitch(ctx, e.ChatID, string(e.Raw))
}

// briefAction runs pause or done on the session's brief folder and waits for
// the host's answer.
func (h *Hub) briefAction(typ string) func(int64, context.Context, Target) (int, any) {
	return func(_ int64, rctx context.Context, t Target) (int, any) {
		if !h.connected(t.Host) {
			return http.StatusServiceUnavailable, map[string]string{"error": t.Host + " is offline"}
		}
		ctx, cancel := context.WithTimeout(rctx, peekWait)
		defer cancel()
		payload, _ := json.Marshal(agterm.BriefRequest{Session: t.Dir})
		res, err := h.jobNow(ctx, t, typ, payload)
		if err != nil {
			return http.StatusGatewayTimeout, map[string]string{"error": err.Error()}
		}
		var d agterm.BriefResult
		_ = json.Unmarshal(res.Data, &d)
		if res.Status != proto.StatusOK {
			return http.StatusBadGateway, map[string]string{"error": firstNonEmpty(d.Error, res.Error, res.Status)}
		}
		log.Printf("mini app: %s %s → %s", typ, t, d.Path)
		return http.StatusOK, map[string]any{"ok": true, "path": d.Path, "brief_dir": d.BriefDir}
	}
}

// jobNow runs a job for an API caller and waits for its result. The job has
// no chat (ChatID 0): its delivery hands the result over instead of posting.
func (h *Hub) jobNow(ctx context.Context, t Target, typ string, payload []byte) (proto.Result, error) {
	id := newID()
	ch := make(chan proto.Result, 1)
	h.waitMu.Lock()
	h.waiters[id] = ch
	h.waitMu.Unlock()
	defer func() {
		h.waitMu.Lock()
		delete(h.waiters, id)
		h.waitMu.Unlock()
	}()
	j := &JobRow{ID: id, EventID: "api:" + id, Host: t.Host, Lane: "ctl:" + id, Target: t.String(), Type: typ, Payload: payload}
	if _, err := h.st.AddJob(j, h.now()); err != nil {
		return proto.Result{}, err
	}
	h.kickDispatch()
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return proto.Result{}, fmt.Errorf("%s didn't answer in %s", t.Host, peekWait)
	}
}

// handOver gives an API job's result to its waiter; false when j isn't one.
func (h *Hub) handOver(j *JobRow, r proto.Result) bool {
	if j.ChatID != 0 {
		return false
	}
	h.waitMu.Lock()
	ch := h.waiters[j.ID]
	h.waitMu.Unlock()
	if ch != nil {
		select {
		case ch <- r:
		default:
		}
	}
	return true
}
