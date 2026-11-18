package booking

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// at pins the site's clock: 2026-01-25 23:50 in a zone east of UTC (the window crosses a month end).
func at() time.Time {
	return time.Date(2026, 1, 25, 23, 50, 0, 0, time.FixedZone("T", 3600))
}

func site(t *testing.T) (*httptest.Server, *Site, *bytes.Buffer) {
	t.Helper()
	var log bytes.Buffer
	s := New(Options{Now: at, Log: &log})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, s, &log
}

func get(t *testing.T, u string) (int, string) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestWindowIsFourteenDaysFromTheSiteClock(t *testing.T) {
	w := New(Options{Now: at}).Window()
	if len(w) != Days || w[0] != "2026-01-25" || w[6] != "2026-01-31" || w[7] != "2026-02-01" || w[13] != "2026-02-07" {
		t.Fatalf("window %v", w)
	}
	s := Slots()
	if len(s) != 19 || s[0] != "09:00" || s[3] != "10:30" || s[18] != "18:00" {
		t.Fatalf("slots %v", s)
	}
}

func TestPagesAndBooking(t *testing.T) {
	srv, s, _ := site(t)
	if code, body := get(t, srv.URL+"/"); code != 200 || !strings.Contains(body, Name) || !strings.Contains(body, `href="/book"`) {
		t.Fatalf("home %d", code)
	}
	code, body := get(t, srv.URL+"/book")
	if code != 200 || !strings.Contains(body, "2026-02-07") || strings.Contains(body, "2026-02-08") ||
		!strings.Contains(body, "/confirm?service=haircut&amp;date=2026-01-26&amp;time=10%3a30") {
		t.Fatalf("book %d", code)
	}
	code, body = get(t, srv.URL+"/confirm?service=haircut&date=2026-01-26&time=10:30")
	if code != 200 || strings.Count(body, "Confirm booking") != 1 {
		t.Fatalf("confirm %d", code)
	}
	for _, q := range []string{"service=haircut&date=2026-02-08&time=10:30", "service=haircut&date=2026-01-24&time=10:30",
		"service=haircut&date=2026-01-26&time=10:15", "service=perm&date=2026-01-26&time=10:30"} {
		if code, _ := get(t, srv.URL+"/confirm?"+q); code != 400 {
			t.Errorf("%s: %d, want 400", q, code)
		}
	}
	if len(s.Bookings()) != 0 {
		t.Fatal("a GET booked")
	}
	for i, want := range []string{"BX-1", "BX-2"} {
		resp, err := http.PostForm(srv.URL+"/bookings", url.Values{"service": {"haircut"}, "date": {"2026-01-26"}, "time": {"10:30"}})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 201 || !strings.Contains(string(b), "Booked ✓ ref "+want) || len(s.Bookings()) != i+1 {
			t.Fatalf("post %d: %d", i, resp.StatusCode)
		}
	}
	resp, _ := http.Post(srv.URL+"/_fake/reset", "", nil)
	resp.Body.Close()
	if len(s.Bookings()) != 0 {
		t.Fatal("reset kept bookings")
	}
}

func TestLogHasOneJSONLinePerRequest(t *testing.T) {
	srv, _, log := site(t)
	get(t, srv.URL+"/")
	get(t, srv.URL+"/confirm?service=haircut&date=2026-01-26&time=10:30")
	get(t, srv.URL+"/_fake/health")
	resp, err := http.PostForm(srv.URL+"/bookings", url.Values{"service": {"haircut"}, "date": {"2026-01-26"}, "time": {"10:30"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d log lines (control paths must not log): %q", len(lines), lines)
	}
	var got []LogLine
	for _, l := range lines {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(l), &raw); err != nil || len(raw) != 4 {
			t.Fatalf("line %q: %v (want exactly method, path, query, form)", l, err)
		}
		var ll LogLine
		_ = json.Unmarshal([]byte(l), &ll)
		got = append(got, ll)
	}
	if got[0].Method != "GET" || got[0].Path != "/" || len(got[0].Query) != 0 {
		t.Errorf("home line %+v", got[0])
	}
	if got[1].Path != "/confirm" || got[1].Query["date"] != "2026-01-26" || got[1].Query["time"] != "10:30" || got[1].Query["service"] != "haircut" {
		t.Errorf("confirm line %+v", got[1])
	}
	if got[2].Method != "POST" || got[2].Path != "/bookings" || got[2].Form["time"] != "10:30" || len(got[2].Query) != 0 {
		t.Errorf("post line %+v", got[2])
	}
}
