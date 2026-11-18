package pairing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGenerate(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		if n, ok := Normalize(c); !ok || n != c {
			t.Fatalf("generated %q is not canonical (%q)", c, n)
		}
		seen[c] = true
	}
	if len(seen) < 199 {
		t.Errorf("only %d distinct codes of 200", len(seen))
	}
	if d := Display("ABCD0123"); d != "ABCD-0123" {
		t.Errorf("display %q", d)
	}
}

// Case-insensitive, with or without the dash, Crockford's O→0 and I/L→1.
func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"ABCD-0123":   "ABCD0123",
		"abcd0123":    "ABCD0123",
		" abcd-0123 ": "ABCD0123",
		"ABCD-O1Z3":   "ABCD01Z3",
		"abcd-oiz3":   "ABCD01Z3",
		"abcd-0lz3":   "ABCD01Z3",
	} {
		if got, ok := Normalize(in); !ok || got != want {
			t.Errorf("Normalize(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "ABCD-012", "ABCD-01234", "ABCD-012U", "ABCD 0123", "ABCD-01*3", "ÄBCD-0123"} {
		if got, ok := Normalize(bad); ok {
			t.Errorf("Normalize(%q) accepted as %q", bad, got)
		}
	}
	if Hash("ABCD0123") == Hash("ABCD0124") || len(Hash("ABCD0123")) != 64 {
		t.Error("hash")
	}
}

func TestBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"ws://127.0.0.1:8470/ws":       "http://127.0.0.1:8470",
		"wss://hub.example.invalid/ws": "https://hub.example.invalid",
		"http://127.0.0.1:4710":        "http://127.0.0.1:4710",
	} {
		if got, err := BaseURL(in); err != nil || got != want {
			t.Errorf("BaseURL(%q) = %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"ftp://x/ws", "ws:///ws", "::"} {
		if _, err := BaseURL(bad); err == nil {
			t.Errorf("BaseURL(%q) accepted", bad)
		}
	}
}

func TestClient(t *testing.T) {
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	var bodies []Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" || r.Header.Get("X-Hommy-Host") != "mac1" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/pairing":
			json.NewEncoder(w).Encode(State{Pending: true, ExpiresAt: &exp})
		case "POST /api/pairing":
			var req Request
			json.NewDecoder(r.Body).Decode(&req)
			bodies = append(bodies, req)
			switch {
			case req.Reset:
				json.NewEncoder(w).Encode(State{})
			case req.OwnerID != 0:
				json.NewEncoder(w).Encode(State{Paired: true, OwnerChatID: req.OwnerID})
			default:
				json.NewEncoder(w).Encode(Code{Code: "ABCD-0123", ExpiresAt: exp, Bot: "@b", Link: "https://t.me/b?start=ABCD-0123"})
			}
		case "GET /api/status":
			w.Write([]byte(`{"version":"v1","telegram":"ok","paired":true,"owner":{"first_name":"Tess"},
				"hosts":[{"name":"mac1","online":true,"last_seen":"2030-01-02T03:04:05Z","version":"v1","checks":{"vault":"ok"}}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c := Client{BaseURL: srv.URL, HostName: "mac1", Key: "k"}
	code, err := c.New(ctx)
	if err != nil || code.Code != "ABCD-0123" || !code.ExpiresAt.Equal(exp) || code.Bot != "@b" {
		t.Fatalf("New: %+v %v", code, err)
	}
	if st, err := c.Status(ctx); err != nil || !st.Pending || st.ExpiresAt == nil || !st.ExpiresAt.Equal(exp) {
		t.Fatalf("Status: %+v %v", st, err)
	}
	if st, err := c.Bind(ctx, 1001); err != nil || !st.Paired || st.OwnerChatID != 1001 {
		t.Fatalf("Bind: %+v %v", st, err)
	}
	if _, err := c.Bind(ctx, 0); err == nil {
		t.Error("Bind(0) sent")
	}
	if st, err := c.Reset(ctx); err != nil || st.Paired {
		t.Fatalf("Reset: %+v %v", st, err)
	}
	s, err := c.APIStatus(ctx)
	if err != nil || s.Owner == nil || s.Owner.FirstName != "Tess" || len(s.Hosts) != 1 || s.Hosts[0].Checks["vault"] != "ok" ||
		s.Hosts[0].LastSeen == nil {
		t.Fatalf("APIStatus: %+v %v", s, err)
	}
	if len(bodies) != 3 || bodies[0] != (Request{}) || bodies[1].OwnerID != 1001 || !bodies[2].Reset {
		t.Errorf("bodies %+v", bodies)
	}

	_, err = Client{BaseURL: srv.URL, HostName: "mac1", Key: "wrong"}.Status(ctx)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 || !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("bad key: %v", err)
	}
}
