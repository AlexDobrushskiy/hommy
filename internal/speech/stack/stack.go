// Package stack is the local speech stack (master spec §8.2): the llama-swap
// gateway config, the whisper model with a pinned SHA-256, the tools it needs,
// and the live round trips that warm it and that `doctor --deep` runs.
package stack

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Model is one whisper.cpp model file.
type Model struct {
	Name   string
	SHA256 string
	Size   int64
}

// File is the model's file name (ggml-<name>.bin).
func (m Model) File() string { return "ggml-" + m.Name + ".bin" }

// URL is where the model downloads from.
func (m Model) URL() string { return ModelBaseURL + "/" + m.File() }

// ModelBaseURL is the whisper.cpp model repository on Hugging Face.
var ModelBaseURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main"

// Models are the sizes setup offers, pinned by SHA-256 (the Hugging Face LFS object ids).
var Models = map[string]Model{
	"small":          {Name: "small", SHA256: "1be3a9b2063867b937e64e2ec7483364a79917e157fa98c5d94b5c1fffea987b", Size: 487601967},
	"medium":         {Name: "medium", SHA256: "6c14d5adee5f86394037b4e4e8b59f1673b6cee10e3cf0b11bbdbee79c156208", Size: 1533763059},
	"large-v3-turbo": {Name: "large-v3-turbo", SHA256: "1fc70f774d38eb169993ac391eea357ef47c88757ef72ee5943879b7e8e2bc69", Size: 1624555275},
}

// ModelFor looks a size up.
func ModelFor(name string) (Model, error) {
	m, ok := Models[name]
	if !ok {
		return Model{}, fmt.Errorf("unknown whisper model %q (small, medium, large-v3-turbo)", name)
	}
	return m, nil
}

// ModelPath is <share>/models/ggml-<name>.bin.
func ModelPath(shareDir string, m Model) string { return filepath.Join(shareDir, "models", m.File()) }

// Tools are the executables the speech service runs, with their install lines.
var Tools = []struct{ Name, Fix string }{
	{"llama-swap", "brew install mostlygeek/llama-swap/llama-swap"},
	{"whisper-server", "brew install whisper.cpp"},
	{"ffmpeg", "brew install ffmpeg"},
	{"mlx_audio.server", "install.sh (re-run): uv tool install \"mlx-audio[server,tts]\" --with \"misaki[en]\""},
}

// Find resolves a tool in dirs (the services' PATH, in order).
func Find(dirs []string, name string) (string, bool) {
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// Params are what the gateway config needs.
type Params struct {
	StartPort   int    // the first backend port (PORT_BASE + 10000)
	WhisperBin  string // absolute paths
	MlxAudioBin string
	ModelFile   string
	LogDir      string // <state>/speech
}

// Render is <config>/llama-swap.yaml: the whisper and kokoro models in one
// group that never swaps (both stay loaded until their ttl).
func Render(p Params) []byte {
	var b strings.Builder
	q := strconv.Quote
	b.WriteString("# hommy speech gateway (llama-swap); written by hommy setup, rewritten on every setup run.\n")
	fmt.Fprintf(&b, "startPort: %d\n", p.StartPort)
	b.WriteString("healthCheckTimeout: 300\n")
	b.WriteString("models:\n")
	b.WriteString("  whisper:\n")
	fmt.Fprintf(&b, "    cmd: %s\n", q(strings.Join([]string{p.WhisperBin, "--model", p.ModelFile, "--host", "127.0.0.1",
		"--port", "${PORT}", "--convert", "--inference-path", "/v1/audio/transcriptions", "--language", "auto"}, " ")))
	b.WriteString("    checkEndpoint: /health\n")
	b.WriteString("    ttl: 300\n")
	b.WriteString("  kokoro:\n")
	fmt.Fprintf(&b, "    cmd: %s\n", q(strings.Join([]string{p.MlxAudioBin, "--host", "127.0.0.1", "--port", "${PORT}",
		"--log-dir", p.LogDir}, " ")))
	b.WriteString("    checkEndpoint: /\n")
	b.WriteString("    ttl: 600\n")
	b.WriteString("groups:\n")
	b.WriteString("  speech:\n")
	b.WriteString("    swap: false\n")
	b.WriteString("    exclusive: false\n")
	b.WriteString("    members: [whisper, kokoro]\n")
	return []byte(b.String())
}

// WriteIfChanged writes data atomically (mode 0600) when the file's bytes differ.
func WriteIfChanged(path string, data []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// stamp is the verified file's identity: a re-run trusts it instead of hashing a GB again.
func stamp(fi os.FileInfo, sum string) string {
	return fmt.Sprintf("%s %d %d\n", sum, fi.Size(), fi.ModTime().UnixNano())
}

// Verified reports whether path holds the model: its stamp matches, else (no
// stamp) a full SHA-256 that then writes the stamp.
func Verified(path string, m Model) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if fi.Size() != m.Size {
		return false, nil
	}
	if b, err := os.ReadFile(path + ".sha256"); err == nil && string(b) == stamp(fi, m.SHA256) {
		return true, nil
	}
	sum, err := fileSHA(path)
	if err != nil {
		return false, err
	}
	if sum != m.SHA256 {
		return false, nil
	}
	return true, os.WriteFile(path+".sha256", []byte(stamp(fi, sum)), 0o600)
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Download fetches the model into path unless it is already there and
// verified. It reports whether it downloaded. A wrong SHA-256 fails and leaves
// nothing behind.
func Download(ctx context.Context, client *http.Client, path string, m Model, progress io.Writer) (bool, error) {
	if ok, err := Verified(path, m); err != nil || ok {
		return false, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL(), nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("download %s: %w", m.File(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("download %s: HTTP %d", m.URL(), resp.StatusCode)
	}
	part := path + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer os.Remove(part)
	h := sha256.New()
	pw := &progressWriter{w: progress, total: m.Size, every: 100 << 20}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), resp.Body); err != nil {
		f.Close()
		return false, fmt.Errorf("download %s: %w", m.File(), err)
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != m.SHA256 {
		return false, fmt.Errorf("download %s: SHA-256 %s, want %s", m.File(), sum, m.SHA256)
	}
	if err := os.Rename(part, path); err != nil {
		return false, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(path+".sha256", []byte(stamp(fi, m.SHA256)), 0o600)
}

type progressWriter struct {
	w           io.Writer
	total, n    int64
	every, next int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	if p.w != nil && p.n >= p.next {
		fmt.Fprintf(p.w, "  %d / %d MB\n", p.n>>20, p.total>>20)
		p.next = p.n + p.every
	}
	return len(b), nil
}

// Sample is a 1 s voice note (Ogg Opus) saying "hello": the STT round trip.
//
//go:embed hello.ogg
var Sample []byte

// Transcribe posts the sample to an OpenAI-style STT base URL (…/v1) and
// returns the text.
func Transcribe(ctx context.Context, client *http.Client, baseURL string, audio []byte) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("file", "sample.ogg")
	if err != nil {
		return "", err
	}
	fw.Write(audio)
	w.WriteField("model", "whisper")
	w.WriteField("response_format", "json")
	w.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	b, code, err := do(client, req)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("transcription: HTTP %d %s", code, snippet(b))
	}
	var v struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return "", fmt.Errorf("transcription: not JSON: %s", snippet(b))
	}
	return strings.TrimSpace(v.Text), nil
}

// Synthesize asks an OpenAI-style TTS base URL (…/v1) for text as Ogg Opus,
// the way the hub does, and checks the answer is an Ogg stream.
func Synthesize(ctx context.Context, client *http.Client, baseURL, model, voice, langCode, text string) ([]byte, error) {
	in, _ := json.Marshal(map[string]any{"model": model, "voice": voice, "input": text, "response_format": "opus",
		"lang_code": langCode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/audio/speech",
		bytes.NewReader(in))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	b, code, err := do(client, req)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("synthesis: HTTP %d %s", code, snippet(b))
	}
	if !IsOggOpus(b) {
		return nil, fmt.Errorf("synthesis: not Ogg Opus (%d bytes)", len(b))
	}
	return b, nil
}

// IsOggOpus reports an Ogg stream whose first packet is an Opus header.
func IsOggOpus(b []byte) bool {
	return len(b) > 36 && bytes.HasPrefix(b, []byte("OggS")) && bytes.Contains(b[:min(len(b), 200)], []byte("OpusHead"))
}

func do(client *http.Client, req *http.Request) ([]byte, int, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return b, resp.StatusCode, err
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
