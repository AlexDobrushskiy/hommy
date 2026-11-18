package stack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelTable(t *testing.T) {
	for _, name := range []string{"small", "medium", "large-v3-turbo"} {
		m, err := ModelFor(name)
		if err != nil {
			t.Fatal(err)
		}
		if len(m.SHA256) != 64 || m.Size < 400<<20 {
			t.Errorf("%s: bad pin %q %d", name, m.SHA256, m.Size)
		}
		if !strings.HasSuffix(m.URL(), "/ggerganov/whisper.cpp/resolve/main/ggml-"+name+".bin") {
			t.Errorf("%s: url %s", name, m.URL())
		}
	}
	if _, err := ModelFor("tiny"); err == nil {
		t.Error("tiny accepted")
	}
}

func TestRender(t *testing.T) {
	y := string(Render(Params{StartPort: 14710, WhisperBin: "/opt/homebrew/bin/whisper-server",
		MlxAudioBin: "/h/.local/bin/mlx_audio.server", ModelFile: "/h/m/ggml-small.bin", LogDir: "/h/state/speech"}))
	for _, want := range []string{
		"startPort: 14710\n",
		`cmd: "/opt/homebrew/bin/whisper-server --model /h/m/ggml-small.bin --host 127.0.0.1 --port ${PORT} --convert --inference-path /v1/audio/transcriptions --language auto"`,
		"checkEndpoint: /health\n    ttl: 300\n",
		`cmd: "/h/.local/bin/mlx_audio.server --host 127.0.0.1 --port ${PORT} --log-dir /h/state/speech"`,
		"checkEndpoint: /\n    ttl: 600\n",
		"swap: false\n", "members: [whisper, kokoro]\n",
	} {
		if !strings.Contains(y, want) {
			t.Errorf("missing %q in\n%s", want, y)
		}
	}
	// same input, same bytes: the re-run writes nothing (I5)
	if string(Render(Params{StartPort: 14710})) != string(Render(Params{StartPort: 14710})) {
		t.Error("render not deterministic")
	}
}

func TestWriteIfChanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c", "llama-swap.yaml")
	for i, want := range []bool{true, false} {
		changed, err := WriteIfChanged(p, []byte("a\n"))
		if err != nil || changed != want {
			t.Fatalf("write %d: changed=%v err=%v", i, changed, err)
		}
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}
}

// fakeModel serves a small "model" with its real SHA-256 pinned.
func fakeModel(t *testing.T, body string) (Model, *httptest.Server, *int) {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	m := Model{Name: "unit", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/"+m.File() {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	old := ModelBaseURL
	ModelBaseURL = srv.URL
	t.Cleanup(func() { ModelBaseURL = old })
	return m, srv, &hits
}

func TestDownloadVerifiesAndIsIdempotent(t *testing.T) {
	m, _, hits := fakeModel(t, "pretend whisper weights")
	p := filepath.Join(t.TempDir(), "models", m.File())
	got, err := Download(context.Background(), nil, p, m, io.Discard)
	if err != nil || !got {
		t.Fatalf("first: %v %v", got, err)
	}
	got, err = Download(context.Background(), nil, p, m, io.Discard)
	if err != nil || got || *hits != 1 {
		t.Fatalf("second: downloaded=%v err=%v hits=%d", got, err, *hits)
	}
	// a damaged file with the right size is detected once the stamp no longer matches
	os.WriteFile(p, []byte(strings.Repeat("x", int(m.Size))), 0o600)
	if ok, _ := Verified(p, m); ok {
		t.Fatal("damaged model verified")
	}
}

func TestDownloadRejectsWrongSHA(t *testing.T) {
	m, _, _ := fakeModel(t, "weights")
	m.SHA256 = strings.Repeat("0", 64)
	p := filepath.Join(t.TempDir(), m.File())
	if _, err := Download(context.Background(), nil, p, m, io.Discard); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("a file was left behind")
	}
	if _, err := os.Stat(p + ".part"); !os.IsNotExist(err) {
		t.Error("a .part was left behind")
	}
}

func TestSampleIsOggOpus(t *testing.T) {
	if !IsOggOpus(Sample) {
		t.Fatal("embedded sample is not Ogg Opus")
	}
}

func TestRoundTrips(t *testing.T) {
	ogg := append([]byte("OggS"), make([]byte, 30)...)
	ogg = append(ogg, []byte("OpusHead....")...)
	var lang string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/audio/transcriptions":
			if f, _, err := r.FormFile("file"); err != nil || f == nil {
				http.Error(w, "no file", 400)
				return
			}
			io.WriteString(w, `{"text":" Hello. "}`)
		case "/v1/audio/speech":
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), `"lang_code":"en"`) {
				lang = "en"
			}
			w.Write(ogg)
		}
	}))
	defer srv.Close()
	txt, err := Transcribe(context.Background(), nil, srv.URL+"/v1", Sample)
	if err != nil || txt != "Hello." {
		t.Fatalf("transcribe %q %v", txt, err)
	}
	if _, err := Synthesize(context.Background(), nil, srv.URL+"/v1", "m", "af_heart", "en", "ok"); err != nil || lang != "en" {
		t.Fatalf("synthesize: %v lang=%q", err, lang)
	}
}

func TestFind(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "tool"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(d, "plain"), []byte("x"), 0o644)
	if p, ok := Find([]string{"/nonexistent", d}, "tool"); !ok || p != filepath.Join(d, "tool") {
		t.Errorf("tool: %q %v", p, ok)
	}
	if _, ok := Find([]string{d}, "plain"); ok {
		t.Error("non-executable found")
	}
}
