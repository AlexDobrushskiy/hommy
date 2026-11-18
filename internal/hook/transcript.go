package hook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

// finalText reads a turn's final assistant text from the session transcript,
// for hook inputs without last_assistant_message. The transcript may lag the
// Stop hook by a moment, so an empty read is retried briefly.
func finalText(path string) string {
	if path == "" {
		return ""
	}
	for i := 0; ; i++ {
		if t := claudeFinalText(path); t != "" || i == 5 {
			return t
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// tailLines returns the complete lines in the last max bytes of path.
func tailLines(path string, max int64) [][]byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := st.Size() - max
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var out [][]byte
	first := off > 0
	for sc.Scan() {
		if first { // a partial line
			first = false
			continue
		}
		out = append(out, append([]byte(nil), sc.Bytes()...))
	}
	return out
}

// claudeFinalText: the text blocks of the last assistant message after the
// last real user prompt in a Claude Code transcript (one JSON object per line;
// a message's blocks may span several lines with the same message.id).
func claudeFinalText(path string) string {
	var lastID string
	var parts []string
	for _, line := range tailLines(path, 4<<20) {
		var e struct {
			Type    string `json:"type"`
			Message struct {
				ID      string          `json:"id"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Type {
		case "user":
			if isPrompt(e.Message.Content) {
				lastID, parts = "", nil
			}
		case "assistant":
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(e.Message.Content, &blocks) != nil {
				continue
			}
			var texts []string
			for _, b := range blocks {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					texts = append(texts, b.Text)
				}
			}
			if len(texts) == 0 {
				continue
			}
			if e.Message.ID != lastID {
				lastID, parts = e.Message.ID, nil
			}
			parts = append(parts, texts...)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// isPrompt: a user entry that is a typed prompt, not a tool result.
func isPrompt(content json.RawMessage) bool {
	c := bytes.TrimSpace(content)
	if len(c) > 0 && c[0] == '"' {
		return true
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(c, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return false
		}
	}
	return len(blocks) > 0
}
