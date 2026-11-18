package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/proto"
)

func goodInit(r *rig) string {
	return signInitData(testConfig().TelegramToken, url.Values{"auth_date": {fmt.Sprint(r.clk.Now().Unix())},
		"user": {fmt.Sprintf(`{"id":%d,"first_name":"Owner"}`, ownerUser)}})
}

// post is a console action as the page sends it.
func (r *rig) miniPost(path, initData string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if initData != "" {
		req.Header.Set("X-Telegram-Init-Data", initData)
	}
	rec := httptest.NewRecorder()
	r.hub.MiniAppHandler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func sid(c byte) string { return strings.Repeat(string(c), 8) + "-0000-0000-0000-000000000000" }

func TestBoardGroupsNeedsYouFirst(t *testing.T) {
	r := newRig(t)
	r.fakeConnected("mac2")
	r.heartbeat("mac2",
		agtermSession{ID: sid('a'), Name: "idle-one", Agent: "claude", State: "ready"},
		agtermSession{ID: sid('b'), Name: "at-menu", Agent: "claude", State: "menu"},
		agtermSession{ID: sid('c'), Name: "busy-one", Agent: "codex", State: "working"},
		agtermSession{ID: sid('d'), Name: "unacked", Agent: "claude", State: "ready", Unacked: 2},
		agtermSession{ID: sid('e'), Name: "buttons", Agent: "claude", State: "ready"},
		agtermSession{ID: sid('f'), Name: "waiting", Agent: "claude", State: "ready"},
		agtermSession{ID: sid('g'), Name: "answered", Agent: "claude", State: "ready"},
		agtermSession{ID: "SHELL", Name: "zsh"})
	r.clk.Add(30 * time.Second)
	r.heartbeat("mac1") // seen, but not connected: offline with its last-seen time
	now := r.clk.Now()
	for i, s := range []struct {
		id      string
		buttons []string
	}{{sid('e'), []string{"Pay", "Drop"}}, {sid('g'), []string{"Keep"}}, {sid('a'), nil}} {
		if err := r.st.AddPing(Ping{ChatID: ownerUser, MsgID: 100 + i, Slug: "s", Host: "mac2", Session: strings.ToUpper(s.id),
			Body: "ping " + s.id[:1], Buttons: s.buttons}, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.st.MarkPingAnswered(ownerUser, 101, now); err != nil {
		t.Fatal(err)
	}
	for _, j := range []struct {
		id, typ string
		at      time.Duration
	}{{sid('f'), proto.EventHookWaiting, 0}, {sid('g'), proto.EventHookWaiting, -time.Minute}, {sid('g'), proto.EventHookAnswer, 0}} {
		row := &JobRow{ID: newID(), EventID: newID(), Host: "mac2", Lane: "x", Type: j.typ, ChatID: ownerUser,
			Target: Target{Kind: KindSession, Host: "mac2", Dir: strings.ToUpper(j.id)}.String(), Payload: []byte("{}")}
		if _, err := r.st.AddDelivery(row, proto.Result{Status: proto.StatusOK}, now.Add(j.at)); err != nil {
			t.Fatal(err)
		}
	}

	b := r.hub.boardAPI(ownerUser).(boardResp)
	hosts := map[string]boardHost{}
	for _, h := range b.Hosts {
		hosts[h.Name] = h
	}
	if g := hosts["mac2"]; !g.Online || g.Sessions != 7 || g.Version != "v1" {
		t.Errorf("mac2 = %+v", g)
	}
	if bl := hosts["mac1"]; bl.Online || bl.LastSeen != now.Unix() || bl.SeenAgo != 0 {
		t.Errorf("mac1 = %+v", bl)
	}
	var order []string
	group := map[string]boardSession{}
	for _, s := range b.Sessions {
		order = append(order, s.Group+":"+s.Name)
		group[s.Name] = s
	}
	if len(order) != 7 {
		t.Fatalf("sessions %v", order)
	}
	for i, want := range []string{"need", "need", "need", "need", "busy", "idle", "idle"} {
		if !strings.HasPrefix(order[i], want+":") {
			t.Fatalf("order %v", order)
		}
	}
	for name, reason := range map[string]string{"at-menu": "at a prompt", "unacked": "2 unacked replies",
		"buttons": "buttons: Pay · Drop", "waiting": "waiting at the Mac"} {
		if s := group[name]; s.Group != "need" || !strings.Contains(strings.Join(s.Reasons, "|"), reason) {
			t.Errorf("%s = %+v", name, s)
		}
	}
	if s := group["answered"]; s.Group != "idle" || s.Last != "ping g" {
		t.Errorf("answered buttons + an answered ⏸ must not need the owner: %+v", s)
	}
	if s := group["idle-one"]; s.Last != "ping a" || s.Target != "session@mac2:"+strings.ToUpper(sid('a')) {
		t.Errorf("idle = %+v", s)
	}
}

func TestBoardHidesAnOfflineHostsSessions(t *testing.T) {
	r := newRig(t)
	r.heartbeat("mac2", agtermSession{ID: sid('a'), Name: "x", Agent: "claude", State: "menu"})
	b := r.hub.boardAPI(ownerUser).(boardResp)
	if len(b.Sessions) != 0 {
		t.Fatalf("offline host's sessions listed: %+v", b.Sessions)
	}
}

// Board and console APIs: initData only (the picker token, tampered or stale
// initData → 401); writes have their own limit.
func TestConsoleAPIAuth(t *testing.T) {
	r := newRig(t)
	r.fakeConnected("mac2")
	r.heartbeat("mac2", agtermSession{ID: sid('a'), Name: "x", Agent: "claude", State: "ready"})
	tgt := "session@mac2:" + strings.ToUpper(sid('a'))
	tok := r.pickerURLToken()
	good := goodInit(r)
	stale := signInitData(testConfig().TelegramToken, url.Values{"auth_date": {fmt.Sprint(r.clk.Now().Add(-61 * time.Minute).Unix())},
		"user": {fmt.Sprintf(`{"id":%d}`, ownerUser)}})
	tampered := strings.Replace(good, "Owner", "Eve", 1)
	gets := []string{"/api/board", "/api/console?peek=0&target=" + url.QueryEscape(tgt),
		"/api/console/files?target=" + url.QueryEscape(tgt)}
	for _, p := range gets {
		for name, hdr := range map[string]map[string]string{
			"picker token": {"X-Picker-Token": tok}, "tampered": {"X-Telegram-Init-Data": tampered},
			"stale": {"X-Telegram-Init-Data": stale}, "none": nil} {
			if code, _ := r.api(p, hdr); code != 401 {
				t.Errorf("GET %s with %s: %d", p, name, code)
			}
		}
	}
	for _, a := range []string{"talk", "pause", "done"} {
		for name, init := range map[string]string{"tampered": tampered, "stale": stale, "none": ""} {
			if code, _ := r.miniPost("/api/console/"+a, init, map[string]string{"target": tgt}); code != 401 {
				t.Errorf("POST %s with %s: %d", a, name, code)
			}
		}
		req := httptest.NewRequest("POST", "/api/console/"+a, strings.NewReader(`{"target":"`+tgt+`"}`))
		req.Header.Set("X-Picker-Token", tok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.hub.MiniAppHandler().ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("POST %s with the picker token: %d", a, rec.Code)
		}
	}
	if code, body := r.api("/api/console?peek=0&target="+url.QueryEscape(tgt), map[string]string{"X-Telegram-Init-Data": good}); code != 200 || body["live"] != true {
		t.Fatalf("console with initData: %d %v", code, body)
	}
	for _, p := range []string{"../x", "/etc/passwd", "~/x", "a/../../b"} {
		if code, body := r.api("/api/console/files?path="+url.QueryEscape(p)+"&target="+url.QueryEscape(tgt),
			map[string]string{"X-Telegram-Init-Data": good}); code != 400 {
			t.Errorf("path %q: %d %v", p, code, body)
		}
	}
	if code, _ := r.api("/api/console?target=vault@mac1", map[string]string{"X-Telegram-Init-Data": good}); code != 400 {
		t.Errorf("console for a vault: %d", code)
	}
	// write limit: 10 a minute (the talk itself is valid every time)
	for i := 1; i <= 11; i++ {
		code, _ := r.miniPost("/api/console/talk", good, map[string]string{"target": tgt})
		if i <= 10 && code != 200 || i == 11 && code != 429 {
			t.Fatalf("talk %d: %d", i, code)
		}
	}
}

// Talk to: the chat's target becomes the session, with the switch notice.
func TestConsoleTalkSwitchesTarget(t *testing.T) {
	r := newRig(t)
	r.fakeConnected("mac2")
	r.heartbeat("mac2", agtermSession{ID: sid('a'), Name: "ev-picker", Agent: "claude", State: "ready"})
	short := "session@mac2:" + sid('a')[:8]
	code, body := r.miniPost("/api/console/talk", goodInit(r), map[string]string{"target": short})
	if code != 200 || body["target"] != "session@mac2:"+strings.ToUpper(sid('a')) {
		t.Fatalf("talk: %d %v", code, body)
	}
	r.tg.waitFor(t, "switch notice", has("send", "Now talking to ev-picker · mac2"))
	if tgt, _ := r.st.ChatTarget(ownerUser); tgt != "session@mac2:"+strings.ToUpper(sid('a')) {
		t.Fatalf("target = %q", tgt)
	}
	// a session that has gone: refused, nothing switched
	code, body = r.miniPost("/api/console/talk", goodInit(r), map[string]string{"target": "session@mac2:" + sid('9')})
	if code != http.StatusConflict {
		t.Fatalf("gone session: %d %v", code, body)
	}
}

// Pause / Done / files / peek go to the host and come back to the page; the
// chat sees none of it.
func TestConsoleActionsOnTheHost(t *testing.T) {
	r := startAgtermHost(t)
	r.ag.add(sessA)
	brief := r.agentBrief("2026-11-01-hc-e2e-agent", sessA)
	if err := os.WriteFile(filepath.Join(brief, "brief.md"), []byte("# the brief\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "secret.txt"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(r.dir, "secret.txt"), filepath.Join(brief, "escape")); err != nil {
		t.Fatal(err)
	}
	tgt := "session@mac1:" + sessA
	init := goodInit(r.rig)
	hdr := map[string]string{"X-Telegram-Init-Data": init}
	before := len(r.tg.snapshot())

	code, c := r.api("/api/console?target="+url.QueryEscape(tgt), hdr)
	if code != 200 || !strings.Contains(fmt.Sprint(c["text"]), "❯") {
		t.Fatalf("console: %d %v", code, c)
	}
	code, f := r.api("/api/console/files?path=brief.md&target="+url.QueryEscape(tgt), hdr)
	if code != 200 || f["content"] != "# the brief\n" {
		t.Fatalf("brief.md: %d %v", code, f)
	}
	code, f = r.api("/api/console/files?target="+url.QueryEscape(tgt), hdr)
	if code != 200 || !strings.Contains(fmt.Sprint(f["files"]), "brief.md") {
		t.Fatalf("listing: %d %v", code, f)
	}
	if code, f = r.api("/api/console/files?path=escape&target="+url.QueryEscape(tgt), hdr); code != 200 || f["error"] == nil || f["content"] != nil {
		t.Fatalf("symlink out of the folder: %d %v", code, f)
	}

	code, p := r.miniPost("/api/console/pause", init, map[string]string{"target": tgt})
	if code != 200 || !strings.HasSuffix(fmt.Sprint(p["path"]), "PAUSE") {
		t.Fatalf("pause: %d %v", code, p)
	}
	if _, err := os.Stat(filepath.Join(brief, "PAUSE")); err != nil {
		t.Fatalf("no PAUSE: %v", err)
	}
	code, d := r.miniPost("/api/console/done", init, map[string]string{"target": tgt})
	if code != 200 {
		t.Fatalf("done: %d %v", code, d)
	}
	st, _ := os.ReadFile(filepath.Join(brief, "state.json"))
	if !strings.Contains(string(st), `"status": "done"`) {
		t.Fatalf("state.json:\n%s", st)
	}
	for _, c := range r.tg.snapshot()[before:] {
		if c.Kind == "send" {
			t.Errorf("a console action reached the chat: %+v", c)
		}
	}
	if typed := r.ag.submitted(sessA); len(typed) != 0 {
		t.Errorf("the console typed into the session: %q", typed)
	}
}
