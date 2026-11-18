// Package booking is the booking fixture site of the e2e harness (master spec §14 D-3): "Barbearia Exemplo", an
// invented barber. `/` is the business page, `/book` lists the next 14 days × slots 09:00–18:00 every 30 minutes,
// `/confirm` shows the summary with a "Confirm booking" button and `POST /bookings` records a booking. Every request
// (control paths under /_fake/ excepted) is one JSON line in the log: method, path, query, form.
package booking

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
)

// Name is the business the site plays.
const Name = "Barbearia Exemplo"

// Days is the booking window: today and the 13 days after it, by the site's clock.
const Days = 14

// Service is one bookable service.
type Service struct {
	ID, Name string
	Minutes  int
	Price    string
}

// Services the site offers, in page order.
var Services = []Service{
	{"haircut", "Haircut", 30, "€15"},
	{"beard", "Beard trim", 30, "€10"},
	{"haircut-beard", "Haircut + beard", 60, "€22"},
}

// Slots are the bookable start times of every day.
func Slots() []string {
	var s []string
	for m := 9 * 60; m <= 18*60; m += 30 {
		s = append(s, fmt.Sprintf("%02d:%02d", m/60, m%60))
	}
	return s
}

// Options configure a Site. Now defaults to time.Now (the site's clock, in its location); Log may be nil.
type Options struct {
	Now func() time.Time
	Log io.Writer
}

// Booking is a recorded booking.
type Booking struct {
	Ref     string `json:"ref"`
	Service string `json:"service"`
	Date    string `json:"date"`
	Time    string `json:"time"`
}

// Site is the fixture's http.Handler.
type Site struct {
	now      func() time.Time
	mu       sync.Mutex
	log      io.Writer
	bookings []Booking
	mux      *http.ServeMux
}

// LogLine is one request in the log.
type LogLine struct {
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Query  map[string]string `json:"query"`
	Form   map[string]string `json:"form"`
}

// New returns the site.
func New(o Options) *Site {
	s := &Site{now: o.Now, log: o.Log, mux: http.NewServeMux()}
	if s.now == nil {
		s.now = time.Now
	}
	s.mux.HandleFunc("GET /{$}", s.home)
	s.mux.HandleFunc("GET /book", s.book)
	s.mux.HandleFunc("GET /confirm", s.confirm)
	s.mux.HandleFunc("POST /bookings", s.create)
	s.mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	s.mux.HandleFunc("GET /_fake/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	s.mux.HandleFunc("GET /_fake/bookings", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.Bookings())
	})
	s.mux.HandleFunc("POST /_fake/reset", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.bookings = nil
		s.mu.Unlock()
		_, _ = io.WriteString(w, "reset\n")
	})
	return s
}

// Bookings returns the bookings recorded so far.
func (s *Site) Bookings() []Booking {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Booking{}, s.bookings...)
}

// Window returns the bookable dates (YYYY-MM-DD), today first.
func (s *Site) Window() []string {
	t := s.now()
	day := time.Date(t.Year(), t.Month(), t.Day(), 12, 0, 0, 0, t.Location())
	out := make([]string, Days)
	for i := range out {
		out[i] = day.AddDate(0, 0, i).Format(time.DateOnly)
	}
	return out
}

func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Path) < 7 || r.URL.Path[:7] != "/_fake/" {
		_ = r.ParseForm()
		s.record(r)
	}
	s.mux.ServeHTTP(w, r)
}

func flat(v url.Values) map[string]string {
	m := map[string]string{}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m[k] = v.Get(k)
	}
	return m
}

func (s *Site) record(r *http.Request) {
	if s.log == nil {
		return
	}
	b, _ := json.Marshal(LogLine{Method: r.Method, Path: r.URL.Path, Query: flat(r.URL.Query()), Form: flat(r.PostForm)})
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.log.Write(append(b, '\n'))
}

func service(id string) (Service, bool) {
	for _, sv := range Services {
		if sv.ID == id {
			return sv, true
		}
	}
	return Service{}, false
}

// check validates a service, date and time against the window and the slots.
func (s *Site) check(svc, date, tm string) (Service, error) {
	sv, ok := service(svc)
	if !ok {
		return sv, fmt.Errorf("unknown service %q", svc)
	}
	inWindow := false
	for _, d := range s.Window() {
		inWindow = inWindow || d == date
	}
	if !inWindow {
		return sv, fmt.Errorf("date %q is not bookable (the next %d days only)", date, Days)
	}
	for _, t := range Slots() {
		if t == tm {
			return sv, nil
		}
	}
	return sv, fmt.Errorf("time %q is not a slot (09:00–18:00, every 30 minutes)", tm)
}

func (s *Site) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = pages.ExecuteTemplate(w, name, data)
}

func (s *Site) home(w http.ResponseWriter, _ *http.Request) {
	s.render(w, http.StatusOK, "home", map[string]any{"Name": Name, "Services": Services})
}

type day struct {
	Date, Label string
}

func (s *Site) book(w http.ResponseWriter, r *http.Request) {
	svc := r.URL.Query().Get("service")
	if svc == "" {
		svc = "haircut"
	}
	sv, ok := service(svc)
	if !ok {
		s.render(w, http.StatusBadRequest, "error", map[string]any{"Name": Name, "Error": fmt.Sprintf("unknown service %q", svc)})
		return
	}
	var days []day
	for _, d := range s.Window() {
		t, _ := time.Parse(time.DateOnly, d)
		days = append(days, day{d, t.Format("Mon 2 Jan")})
	}
	s.render(w, http.StatusOK, "book", map[string]any{"Name": Name, "Services": Services, "Service": sv, "Days": days, "Slots": Slots()})
}

func (s *Site) confirm(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sv, err := s.check(q.Get("service"), q.Get("date"), q.Get("time"))
	if err != nil {
		s.render(w, http.StatusBadRequest, "error", map[string]any{"Name": Name, "Error": err.Error()})
		return
	}
	s.render(w, http.StatusOK, "confirm", map[string]any{"Name": Name, "Service": sv, "Date": q.Get("date"), "Time": q.Get("time")})
}

func (s *Site) create(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	sv, err := s.check(f.Get("service"), f.Get("date"), f.Get("time"))
	if err != nil {
		s.render(w, http.StatusBadRequest, "error", map[string]any{"Name": Name, "Error": err.Error()})
		return
	}
	s.mu.Lock()
	b := Booking{Ref: fmt.Sprintf("BX-%d", len(s.bookings)+1), Service: sv.ID, Date: f.Get("date"), Time: f.Get("time")}
	s.bookings = append(s.bookings, b)
	s.mu.Unlock()
	s.render(w, http.StatusCreated, "booked", map[string]any{"Name": Name, "Service": sv, "Booking": b})
}

// dict builds a template map from key/value pairs.
func dict(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[fmt.Sprint(kv[i])] = kv[i+1]
	}
	return m
}

var pages = template.Must(template.New("").Funcs(template.FuncMap{"dict": dict}).Parse(`
{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Title}}</title>
<style>body{font-family:system-ui,sans-serif;max-width:46rem;margin:0 auto;padding:1rem;color:#222}
a{color:#7a3b12}table{border-collapse:collapse}td,th{padding:.2rem .35rem;text-align:left;vertical-align:top}
th{white-space:nowrap}
.slots a{display:inline-block;margin:.1rem;padding:.15rem .4rem;border:1px solid #ccc;border-radius:.3rem;text-decoration:none}
button{font-size:1.1rem;padding:.5rem 1rem}</style></head><body>{{end}}
{{define "home"}}{{template "head" (dict "Title" .Name)}}
<h1>{{.Name}}</h1>
<p>Classic barber shop. Rua Exemplo 1, 0000-000 Exemplo. Open every day 09:00–18:30.</p>
<h2>Services</h2><ul>{{range .Services}}<li>{{.Name}} · {{.Minutes}} min · {{.Price}}</li>{{end}}</ul>
<p><a href="/book">Book</a></p></body></html>{{end}}
{{define "book"}}{{template "head" (dict "Title" (print "Book · " .Name))}}
<h1>Book at {{.Name}}</h1>
<p>Service: {{$cur := .Service.ID}}{{range $i, $s := .Services}}{{if $i}} · {{end}}{{if eq $s.ID $cur}}<strong>{{$s.Name}}</strong>{{else}}<a href="/book?service={{$s.ID}}">{{$s.Name}}</a>{{end}}{{end}}</p>
<p>Pick a time ({{.Service.Name}}, {{.Service.Minutes}} min):</p>
<table>{{$svc := .Service.ID}}{{$slots := .Slots}}{{range .Days}}{{$d := .Date}}
<tr><th>{{.Label}}<br><small>{{.Date}}</small></th><td class="slots">{{range $slots}}<a href="/confirm?service={{$svc}}&amp;date={{$d}}&amp;time={{.}}">{{.}}</a>{{end}}</td></tr>{{end}}
</table></body></html>{{end}}
{{define "confirm"}}{{template "head" (dict "Title" (print "Confirm · " .Name))}}
<h1>Confirm your booking</h1>
<table><tr><th>Business</th><td>{{.Name}}</td></tr><tr><th>Service</th><td>{{.Service.Name}} ({{.Service.Minutes}} min, {{.Service.Price}})</td></tr>
<tr><th>Date</th><td>{{.Date}}</td></tr><tr><th>Time</th><td>{{.Time}}</td></tr></table>
<form method="post" action="/bookings"><input type="hidden" name="service" value="{{.Service.ID}}">
<input type="hidden" name="date" value="{{.Date}}"><input type="hidden" name="time" value="{{.Time}}">
<p><button type="submit">Confirm booking</button></p></form>
<p><a href="/book?service={{.Service.ID}}">Change time</a></p></body></html>{{end}}
{{define "booked"}}{{template "head" (dict "Title" (print "Booked · " .Name))}}
<h1>Booked ✓ ref {{.Booking.Ref}}</h1>
<p>{{.Service.Name}} at {{.Name}} on {{.Booking.Date}} at {{.Booking.Time}}.</p></body></html>{{end}}
{{define "error"}}{{template "head" (dict "Title" .Name)}}
<h1>{{.Name}}</h1><p class="error">Sorry: {{.Error}}.</p><p><a href="/book">Back to booking</a></p></body></html>{{end}}
`))
