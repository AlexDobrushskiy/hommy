package speech

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/textx"
)

var geminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// whisperClient dials with a short timeout so an offline endpoint fails fast
// (~5s) instead of hanging toward the overall 120s request timeout — CPU
// transcription on a live endpoint can legitimately take a while.
var whisperClient = &http.Client{
	Timeout: 120 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	},
}

var geminiClient = &http.Client{Timeout: 120 * time.Second}

// Transcribe converts Telegram voice audio (ogg/opus) to text, trying each
// configured whisper endpoint in order and falling back to Gemini, when a key
// is set, if all of them fail. Without a key nothing names Gemini at all.
func Transcribe(ctx context.Context, cfg TranscribeConfig, audio []byte) (string, string, error) {
	var failures []string
	for _, ep := range cfg.Endpoints {
		text, err := transcribeWhisper(ctx, ep, audio)
		if err == nil {
			return text, ep.Name, nil
		}
		// Log every fallback, so a degraded chain shows instead of hiding behind
		// "voice messages still work".
		log.Printf("transcribe: endpoint %q (%s) failed, trying next: %v", ep.Name, ep.URL, err)
		failures = append(failures, fmt.Sprintf("%s: %v", ep.Name, err))
	}
	if cfg.GoogleAPIKey == "" {
		return "", "", fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	log.Printf("transcribe: all %d whisper endpoint(s) failed, falling back to gemini (%s)",
		len(cfg.Endpoints), strings.Join(failures, "; "))
	text, geminiErr := transcribeGemini(ctx, cfg, audio)
	if geminiErr != nil {
		failures = append(failures, fmt.Sprintf("gemini: %v", geminiErr))
		return "", "", fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return text, "gemini", nil
}

func transcribeWhisper(ctx context.Context, ep WhisperEndpoint, audio []byte) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	// upload as audio.ogg — Telegram's .oga extension confuses some servers
	fw, err := w.CreateFormFile("file", "audio.ogg")
	if err != nil {
		return "", err
	}
	fw.Write(audio)
	w.WriteField("model", ep.Model)
	w.Close()

	url := strings.TrimRight(ep.URL, "/") + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := whisperClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper HTTP %d: %s", resp.StatusCode, textx.Tail(string(raw), 200))
	}
	var res struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("bad whisper response: %v", err)
	}
	// whisper.cpp joins segments with newlines mid-sentence; clean up (voicenotes pattern)
	text := strings.TrimSpace(res.Text)
	text = strings.ReplaceAll(text, "\n ", " ")
	text = strings.ReplaceAll(text, "\n", "")
	return text, nil
}

func transcribeGemini(ctx context.Context, cfg TranscribeConfig, audio []byte) (string, error) {
	payload := map[string]any{
		"contents": []any{map[string]any{"parts": []any{
			map[string]any{"text": "Transcribe this audio accurately. Output only the transcription, nothing else."},
			map[string]any{"inline_data": map[string]any{
				"mime_type": "audio/ogg",
				"data":      base64.StdEncoding.EncodeToString(audio),
			}},
		}}},
	}
	buf, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/models/%s:generateContent", geminiBaseURL, cfg.GeminiModel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", cfg.GoogleAPIKey)
	resp, err := geminiClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gemini HTTP %d: %s", resp.StatusCode, textx.Tail(string(raw), 200))
	}
	var res struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("bad gemini response: %v", err)
	}
	if len(res.Candidates) == 0 {
		return "", fmt.Errorf("gemini returned no candidates")
	}
	var sb strings.Builder
	for _, p := range res.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	return strings.TrimSpace(sb.String()), nil
}
