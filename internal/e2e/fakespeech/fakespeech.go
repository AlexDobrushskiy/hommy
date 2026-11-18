// Package fakespeech is a fake OpenAI-compatible speech server for tests (docs/testing.md §5): transcriptions from a
// queue, then fixture transcripts matched by the audio's SHA-256, then "fake transcript <sha8>"; speech as a short Ogg
// Opus clip; llama-swap's /health and /upstream/<model>/… aliases; a request log and STT/TTS faults.
package fakespeech

import (
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed reply.ogg
var replyOgg []byte

// Voices the fake lists at /v1/audio/voices.
var Voices = []string{"af_heart", "af_bella", "am_adam", "bf_emma", "bm_george"}

// Entry is one request in the log.
type Entry struct {
	Seq        int    `json:"seq"`
	At         string `json:"at"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Kind       string `json:"kind"` // stt|tts|voices|health|other
	Status     int    `json:"status"`
	Model      string `json:"model,omitempty"`
	Voice      string `json:"voice,omitempty"`
	LangCode   string `json:"lang_code,omitempty"`
	Language   string `json:"language,omitempty"`
	Format     string `json:"response_format,omitempty"`
	InputChars int    `json:"input_chars,omitempty"`
	Input      string `json:"input,omitempty"`
	AudioSHA8  string `json:"audio_sha8,omitempty"`
	AudioBytes int    `json:"audio_bytes,omitempty"`
	Text       string `json:"text,omitempty"`
	Source     string `json:"source,omitempty"` // queue|fixture|default
}

// Options configure a Server.
type Options struct {
	FixturesDir string // *.ogg files with a sibling .txt transcript
	Now         func() time.Time
}

// Server is the fake speech server.
type Server struct {
	now      func() time.Time
	mu       sync.Mutex
	queue    []string
	fixtures map[string]string // sha256 hex → transcript
	log      []Entry
	fault    string // "", "stt", "tts"
}

// New loads the fixtures (a missing dir is no error: the server then has none).
func New(o Options) (*Server, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Server{now: o.Now, fixtures: map[string]string{}}
	if o.FixturesDir == "" {
		return s, nil
	}
	audio, _ := filepath.Glob(filepath.Join(o.FixturesDir, "*.ogg"))
	for _, a := range audio {
		txt, err := os.ReadFile(strings.TrimSuffix(a, ".ogg") + ".txt")
		if err != nil {
			continue
		}
		data, err := os.ReadFile(a)
		if err != nil {
			return nil, err
		}
		s.fixtures[sha(data)] = strings.TrimSpace(string(txt))
	}
	return s, nil
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Fixtures is the number of fixture transcripts loaded.
func (s *Server) Fixtures() int { return len(s.fixtures) }

// Queue adds a transcript that the next transcription returns (first in, first out).
func (s *Server) Queue(text string) {
	s.mu.Lock()
	s.queue = append(s.queue, text)
	s.mu.Unlock()
}

// SetFault makes STT ("stt") or TTS ("tts") answer 500; "" or "none" clears it.
func (s *Server) SetFault(kind string) {
	if kind == "none" {
		kind = ""
	}
	s.mu.Lock()
	s.fault = kind
	s.mu.Unlock()
}

// Log returns the request log.
func (s *Server) Log() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Entry{}, s.log...)
}

func (s *Server) record(e Entry) {
	s.mu.Lock()
	e.Seq = len(s.log) + 1
	e.At = s.now().UTC().Format(time.RFC3339Nano)
	s.log = append(s.log, e)
	s.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// route strips llama-swap's /upstream/<model> prefix and returns the model it named.
func route(path string) (string, string) {
	if rest, ok := strings.CutPrefix(path, "/upstream/"); ok {
		model, sub, _ := strings.Cut(rest, "/")
		return "/" + sub, model
	}
	return path, ""
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_fake/") {
		s.serveControl(w, r)
		return
	}
	path, model := route(r.URL.Path)
	path = strings.TrimSuffix(path, "/")
	switch {
	case path == "/health" || path == "":
		s.record(Entry{Method: r.Method, Path: r.URL.Path, Kind: "health", Status: 200, Model: model})
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "OK")
	case path == "/v1/audio/transcriptions" && r.Method == http.MethodPost:
		s.transcribe(w, r, model)
	case path == "/v1/audio/speech" && r.Method == http.MethodPost:
		s.speak(w, r, model)
	case path == "/v1/audio/voices":
		s.record(Entry{Method: r.Method, Path: r.URL.Path, Kind: "voices", Status: 200, Model: model})
		writeJSON(w, 200, map[string]any{"voices": Voices})
	case path == "/v1/models":
		s.record(Entry{Method: r.Method, Path: r.URL.Path, Kind: "other", Status: 200})
		writeJSON(w, 200, map[string]any{"object": "list", "data": []map[string]string{
			{"id": "whisper", "object": "model"}, {"id": "kokoro", "object": "model"}}})
	default:
		s.record(Entry{Method: r.Method, Path: r.URL.Path, Kind: "other", Status: 404})
		writeJSON(w, 404, map[string]any{"error": map[string]string{"message": "not found: " + r.URL.Path}})
	}
}

func (s *Server) faulted(kind string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fault == kind
}

func (s *Server) transcribe(w http.ResponseWriter, r *http.Request, model string) {
	e := Entry{Method: r.Method, Path: r.URL.Path, Kind: "stt", Model: model}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		e.Status = 400
		s.record(e)
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "expected multipart form: " + err.Error()}})
		return
	}
	if m := r.FormValue("model"); m != "" {
		e.Model = m
	}
	e.Language, e.Format = r.FormValue("language"), r.FormValue("response_format")
	f, _, err := r.FormFile("file")
	if err != nil {
		e.Status = 400
		s.record(e)
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "file is required"}})
		return
	}
	audio, _ := io.ReadAll(f)
	_ = f.Close()
	sum := sha(audio)
	e.AudioSHA8, e.AudioBytes = sum[:8], len(audio)
	if s.faulted("stt") {
		e.Status = 500
		s.record(e)
		writeJSON(w, 500, map[string]any{"error": map[string]string{"message": "fake fault: stt"}})
		return
	}
	s.mu.Lock()
	switch {
	case len(s.queue) > 0:
		e.Text, e.Source = s.queue[0], "queue"
		s.queue = s.queue[1:]
	case s.fixtures[sum] != "":
		e.Text, e.Source = s.fixtures[sum], "fixture"
	default:
		e.Text, e.Source = "fake transcript "+sum[:8], "default"
	}
	s.mu.Unlock()
	e.Status = 200
	s.record(e)
	if e.Format == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, e.Text+"\n")
		return
	}
	writeJSON(w, 200, map[string]string{"text": e.Text})
}

func (s *Server) speak(w http.ResponseWriter, r *http.Request, model string) {
	e := Entry{Method: r.Method, Path: r.URL.Path, Kind: "tts", Model: model}
	var req struct {
		Model    string  `json:"model"`
		Input    string  `json:"input"`
		Voice    string  `json:"voice"`
		Format   string  `json:"response_format"`
		LangCode string  `json:"lang_code"`
		Speed    float64 `json:"speed"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		e.Status = 400
		s.record(e)
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "bad JSON: " + err.Error()}})
		return
	}
	if req.Model != "" {
		e.Model = req.Model
	}
	e.Voice, e.LangCode, e.Format, e.InputChars = req.Voice, req.LangCode, req.Format, len([]rune(req.Input))
	e.Input = req.Input
	if len([]rune(e.Input)) > 200 {
		e.Input = string([]rune(e.Input)[:200])
	}
	if s.faulted("tts") {
		e.Status = 500
		s.record(e)
		writeJSON(w, 500, map[string]any{"error": map[string]string{"message": "fake fault: tts"}})
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		e.Status = 400
		s.record(e)
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "input is empty"}})
		return
	}
	e.Status = 200
	s.record(e)
	if req.Format == "wav" {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(silenceWAV(24000, 300))
		return
	}
	w.Header().Set("Content-Type", "audio/ogg")
	_, _ = w.Write(replyOgg)
}

// silenceWAV is a mono 16-bit PCM WAV of ms milliseconds of silence.
func silenceWAV(rate, ms int) []byte {
	n := rate * ms / 1000 * 2
	b := make([]byte, 44+n)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+n))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(n))
	return b
}

// serveControl: POST /_fake/transcript {"text"} queues one; GET /_fake/log; POST /_fake/fault {"kind": stt|tts|none}.
func (s *Server) serveControl(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimPrefix(r.URL.Path, "/_fake/") {
	case "transcript":
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text == "" {
			writeJSON(w, 400, map[string]string{"error": "want {\"text\": \"…\"}"})
			return
		}
		s.Queue(body.Text)
		writeJSON(w, 200, map[string]any{"queued": body.Text})
	case "log":
		writeJSON(w, 200, map[string]any{"requests": s.Log()})
	case "fault":
		var body struct {
			Kind string `json:"kind"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.Kind {
		case "stt", "tts", "none", "":
			s.SetFault(body.Kind)
			writeJSON(w, 200, map[string]any{"fault": body.Kind})
		default:
			writeJSON(w, 400, map[string]string{"error": "kind must be stt|tts|none"})
		}
	case "health":
		_, _ = io.WriteString(w, "ok\n")
	default:
		http.NotFound(w, r)
	}
}
