package fakespeech

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func newServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s, err := New(Options{FixturesDir: "../../../e2e/fixtures/audio"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Fixtures() != 2 {
		t.Fatalf("fixtures loaded: %d", s.Fixtures())
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

// transcribe posts the way the source hub's whisper client does: multipart file "audio.ogg" plus model.
func transcribe(t *testing.T, url string, audio []byte) (int, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, _ := w.CreateFormFile("file", "audio.ogg")
	_, _ = fw.Write(audio)
	_ = w.WriteField("model", "whisper")
	_ = w.Close()
	resp, err := http.Post(url, w.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r struct {
		Text string `json:"text"`
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &r)
	return resp.StatusCode, r.Text
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../e2e/fixtures/audio/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTranscriptionOrder(t *testing.T) {
	s, srv := newServer(t)
	url := srv.URL + "/v1/audio/transcriptions"
	if code, text := transcribe(t, url, fixture(t, "hello.ogg")); code != 200 || text != "hello" {
		t.Fatalf("fixture: %d %q", code, text)
	}
	if _, text := transcribe(t, srv.URL+"/upstream/whisper/v1/audio/transcriptions", fixture(t, "book-haircut.ogg")); text != "book a haircut at Barbearia Exemplo tomorrow at ten thirty" {
		t.Fatalf("upstream alias: %q", text)
	}
	resp, _ := http.Post(srv.URL+"/_fake/transcript", "application/json", strings.NewReader(`{"text":"Reply with pong"}`))
	resp.Body.Close()
	if _, text := transcribe(t, url, fixture(t, "hello.ogg")); text != "Reply with pong" {
		t.Fatalf("queued: %q", text)
	}
	if _, text := transcribe(t, url, []byte("OggS unknown")); !strings.HasPrefix(text, "fake transcript ") || len(text) != len("fake transcript ")+8 {
		t.Fatalf("default: %q", text)
	}
	log := s.Log()
	if len(log) != 4 || log[0].Source != "fixture" || log[2].Source != "queue" || log[3].Source != "default" || log[0].Model != "whisper" {
		t.Fatalf("log: %+v", log)
	}
}

func TestSpeechAndVoices(t *testing.T) {
	s, srv := newServer(t)
	body := `{"model":"kokoro","input":"hi","voice":"af_heart","response_format":"opus","lang_code":"en"}`
	resp, err := http.Post(srv.URL+"/upstream/kokoro/v1/audio/speech", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	audio, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.HasPrefix(audio, []byte("OggS")) || !bytes.Contains(audio, []byte("OpusHead")) {
		t.Fatalf("speech: %d %d bytes", resp.StatusCode, len(audio))
	}
	e := s.Log()[0]
	if e.Kind != "tts" || e.Voice != "af_heart" || e.LangCode != "en" || e.Model != "kokoro" || e.Input != "hi" {
		t.Fatalf("log entry: %+v", e)
	}
	resp, _ = http.Get(srv.URL + "/upstream/kokoro/v1/audio/voices")
	var v struct {
		Voices []string `json:"voices"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	resp.Body.Close()
	if len(v.Voices) == 0 || v.Voices[0] != "af_heart" {
		t.Fatalf("voices: %+v", v)
	}
	for _, p := range []string{"/health", "/upstream/whisper/health"} {
		resp, _ = http.Get(srv.URL + p)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}
	resp, _ = http.Post(srv.URL+"/v1/audio/speech", "application/json", strings.NewReader(`{"input":"x","response_format":"wav"}`))
	wav, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.HasPrefix(wav, []byte("RIFF")) {
		t.Fatal("wav format not honoured")
	}
}

func TestFaults(t *testing.T) {
	_, srv := newServer(t)
	post := func(kind string) {
		resp, _ := http.Post(srv.URL+"/_fake/fault", "application/json", strings.NewReader(`{"kind":"`+kind+`"}`))
		resp.Body.Close()
	}
	post("stt")
	if code, _ := transcribe(t, srv.URL+"/v1/audio/transcriptions", fixture(t, "hello.ogg")); code != 500 {
		t.Fatalf("stt fault: %d", code)
	}
	resp, _ := http.Post(srv.URL+"/v1/audio/speech", "application/json", strings.NewReader(`{"input":"x"}`))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("tts during an stt fault: %d", resp.StatusCode)
	}
	post("tts")
	resp, _ = http.Post(srv.URL+"/v1/audio/speech", "application/json", strings.NewReader(`{"input":"x"}`))
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("tts fault: %d", resp.StatusCode)
	}
	post("none")
	if code, text := transcribe(t, srv.URL+"/v1/audio/transcriptions", fixture(t, "hello.ogg")); code != 200 || text != "hello" {
		t.Fatalf("after none: %d %q", code, text)
	}
}
