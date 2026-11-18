package speech

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTranscribeWhisperSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("bad path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]string{"text": "hola\nmundo"})
	}))
	defer srv.Close()
	cfg := TranscribeConfig{Endpoints: []WhisperEndpoint{{Name: "mac2", URL: srv.URL + "/v1", Model: "m"}}}
	text, source, err := Transcribe(context.Background(), cfg, []byte("ogg"))
	if err != nil {
		t.Fatal(err)
	}
	if source != "mac2" || text != "holamundo" { // newline cleanup applied
		t.Errorf("got %q from %q", text, source)
	}
}

// TestTranscribeChainFailover checks that when the first endpoint in the
// chain is down, the second is tried and its name is reported as source.
func TestTranscribeChainFailover(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", 500)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"text": "from second"})
	}))
	defer second.Close()

	cfg := TranscribeConfig{Endpoints: []WhisperEndpoint{
		{Name: "first", URL: first.URL + "/v1", Model: "m"},
		{Name: "second", URL: second.URL + "/v1", Model: "m"},
	}}
	text, source, err := Transcribe(context.Background(), cfg, []byte("ogg"))
	if err != nil {
		t.Fatal(err)
	}
	if source != "second" || text != "from second" {
		t.Errorf("got %q from %q", text, source)
	}
}

func TestTranscribeFallsBackToGemini(t *testing.T) {
	whisper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", 500)
	}))
	defer whisper.Close()
	gemini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": "fallback text"}}}}},
		})
	}))
	defer gemini.Close()
	old := geminiBaseURL
	geminiBaseURL = gemini.URL
	defer func() { geminiBaseURL = old }()

	cfg := TranscribeConfig{
		Endpoints: []WhisperEndpoint{
			{Name: "mac2", URL: whisper.URL + "/v1", Model: "m"},
			{Name: "mac3", URL: whisper.URL + "/v1", Model: "m"},
		},
		GoogleAPIKey: "k",
		GeminiModel:  "g",
	}
	text, source, err := Transcribe(context.Background(), cfg, []byte("ogg"))
	if err != nil {
		t.Fatal(err)
	}
	if source != "gemini" || text != "fallback text" {
		t.Errorf("got %q from %q", text, source)
	}
}

func TestTranscribeBothFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", 500)
	}))
	defer srv.Close()
	cfg := TranscribeConfig{Endpoints: []WhisperEndpoint{
		{Name: "mac2", URL: srv.URL + "/v1", Model: "m"},
		{Name: "mac3", URL: srv.URL + "/v1", Model: "m"},
	}} // no GoogleAPIKey
	_, _, err := Transcribe(context.Background(), cfg, []byte("ogg"))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"mac2:", "mac3:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	if strings.Contains(strings.ToLower(err.Error()), "gemini") {
		t.Errorf("no key, yet the error names Gemini: %q", err.Error())
	}
}

func TestParseWhisperEndpoints(t *testing.T) {
	got := ParseWhisperEndpoints("mac2|http://127.0.0.1:8300/v1|ggml-medium, mac3 | http://127.0.0.1:8301/v1 | m ,malformed|x,blank||m")
	if len(got) != 2 || got[1] != (WhisperEndpoint{Name: "mac3", URL: "http://127.0.0.1:8301/v1", Model: "m"}) {
		t.Errorf("got %+v", got)
	}
}

// TestDefaultsAreLoopback: no default reaches beyond this Mac.
func TestDefaultsAreLoopback(t *testing.T) {
	urls := []string{DefaultTTSURL}
	for _, ep := range ParseWhisperEndpoints(DefaultWhisperEndpoints) {
		urls = append(urls, ep.URL)
	}
	for _, b := range ParseTTSLangBackends(DefaultTTSLangBackends) {
		urls = append(urls, b.URL)
	}
	if len(urls) < 2 {
		t.Fatalf("defaults missing: %v", urls)
	}
	for _, u := range urls {
		if !strings.HasPrefix(u, "http://127.0.0.1:") {
			t.Errorf("default %q is not loopback", u)
		}
	}
}
