package agentcli

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// lostAnswerHub reads each request (the hub may have sent it) and drops the
// connection without answering; after `answered` requests it stops listening.
func lostAnswerHub(t *testing.T, closeAfter int32) (string, *int32) {
	var n int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		if atomic.AddInt32(&n, 1) == closeAfter {
			srv.Listener.Close() // the next attempt is refused
		}
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

// A request that reached the hub may have been sent: the CLI must not send it
// again on its own, whatever the later attempts see.
func TestSendNoDirectFallbackAfterTheHubGotIt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		closeAfter int32
		tries      int32
	}{
		{"every answer lost", 99, 3},
		{"answer lost, then refused", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLIRig(t)
			url, n := lostAnswerHub(t, tc.closeAfter)
			cfg := c.hubConfig(url)
			code := c.send("--env-file", cfg, "--agent", "hc-e2e-agent", "maybe sent")
			if got := c.bot.methods(); len(got) != 0 {
				t.Fatalf("sent directly as well: %v (stderr %q)", got, c.errb.String())
			}
			if code != 1 || !strings.Contains(c.errb.String(), "may have been sent") {
				t.Errorf("exit %d stderr %q", code, c.errb.String())
			}
			if got := atomic.LoadInt32(n); got != tc.tries {
				t.Errorf("hub saw %d requests, want %d", got, tc.tries)
			}
		})
	}
}

// A 5xx from the hub (Telegram's answer to the hub was lost: it may have
// delivered) and then a refused connection: the ping may be out, so no
// direct send.
func TestSendNoDirectFallbackAfterHub5xx(t *testing.T) {
	c := newCLIRig(t)
	var n int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		atomic.AddInt32(&n, 1)
		_ = srv.Listener.Close() // the next attempt is refused
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"telegram: context deadline exceeded"}`)
	}))
	t.Cleanup(srv.Close)
	code := c.send("--env-file", c.hubConfig(srv.URL), "--agent", "hc-e2e-agent", "maybe sent")
	if got := c.bot.methods(); len(got) != 0 {
		t.Fatalf("sent directly as well: %v (stderr %q)", got, c.errb.String())
	}
	if code != 1 || !strings.Contains(c.errb.String(), "may have been sent") {
		t.Errorf("exit %d stderr %q", code, c.errb.String())
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Errorf("hub saw %d requests, want 1", got)
	}
}

// The server is off: its SYNs go unanswered and the connect is still pending
// when the attempt's deadline hits (Linux's kernel connect timeout is longer
// than the attempt). Nothing reached the hub, so the ping goes out directly,
// once, and is spooled.
func TestSendDirectWhenTheConnectNeverCompletes(t *testing.T) {
	c := newCLIRig(t)
	var dials int32
	oldHTTP, oldTimeout := hubHTTP, hubAttemptTimeout
	hubHTTP = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		atomic.AddInt32(&dials, 1)
		<-ctx.Done() // a SYN nobody answers
		return nil, ctx.Err()
	}}}
	hubAttemptTimeout = 20 * time.Millisecond
	t.Cleanup(func() { hubHTTP, hubAttemptTimeout = oldHTTP, oldTimeout })
	code := c.send("--env-file", c.hubConfig("http://192.0.2.1:8472"), "--agent", "hc-e2e-agent", "server off")
	if code != 0 || len(c.bot.methods()) != 1 {
		t.Fatalf("exit %d, direct calls %v, stderr %q", code, c.bot.methods(), c.errb.String())
	}
	if !strings.Contains(c.out.String(), `"via":"direct"`) || !strings.Contains(c.out.String(), `"spooled":true`) {
		t.Errorf("stdout %q", c.out.String())
	}
	if got := atomic.LoadInt32(&dials); got != 1 {
		t.Errorf("%d connect attempts, want 1 (unreachable is final)", got)
	}
}

// --agent with a config that has no hub (the default notify.env) sends, but
// says that nothing the owner does with the message can reach the session.
func TestSendAgentWithoutHubWarns(t *testing.T) {
	c := newCLIRig(t)
	c.env["AGTERM_SESSION_ID"] = "AAAA1111-0000"
	if code := c.send("--env-file", c.config(), "--agent", "hc-e2e-agent", "hi"); code != 0 {
		t.Fatalf("exit %d %q", code, c.errb.String())
	}
	if !strings.Contains(c.errb.String(), "no hub") || !strings.Contains(c.out.String(), `"warning":"no hub`) {
		t.Errorf("stderr %q stdout %q", c.errb.String(), c.out.String())
	}
}

// The hub answered 5xx (it tried Telegram and failed): retried by id, never
// re-sent around the hub.
func TestSendHub5xxIsNotSentDirectly(t *testing.T) {
	c := newCLIRig(t)
	hub := &fakeHub{code: 502}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	code := c.send("--env-file", c.hubConfig(srv.URL), "--agent", "hc-e2e-agent", "x")
	if code != 1 || len(c.bot.methods()) != 0 {
		t.Fatalf("exit %d, direct calls %v, stderr %q", code, c.bot.methods(), c.errb.String())
	}
}
