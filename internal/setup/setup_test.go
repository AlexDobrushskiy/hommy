package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMergeSkipPrompt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".claude", "settings.json")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if ch, err := MergeSkipPrompt(p, now); err != nil || !ch {
		t.Fatalf("new file: %v %v", ch, err)
	}
	os.WriteFile(p, []byte(`{"model":"x","env":{"A":"1"}}`), 0o600)
	if ch, err := MergeSkipPrompt(p, now); err != nil || !ch {
		t.Fatalf("merge: %v %v", ch, err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"model": "x"`) || !strings.Contains(string(b), `"skipDangerousModePermissionPrompt": true`) ||
		!strings.Contains(string(b), `"A": "1"`) {
		t.Fatalf("merged: %s", b)
	}
	if bak, _ := os.ReadFile(p + ".bak-20260102-030405"); string(bak) != `{"model":"x","env":{"A":"1"}}` {
		t.Fatalf("backup %q", bak)
	}
	if ch, err := MergeSkipPrompt(p, now.Add(time.Hour)); err != nil || ch {
		t.Fatalf("second run must change nothing: %v %v", ch, err)
	}
	os.WriteFile(p, []byte(`{bad`), 0o600)
	if _, err := MergeSkipPrompt(p, now); err == nil {
		t.Fatal("broken JSON must not be overwritten")
	}
}

func TestTCCNote(t *testing.T) {
	for path, want := range map[string]bool{"/h/Documents/v": true, "/h/Desktop": true, "/h/Library/Mobile Documents/x": true,
		"/h/hommy-vault": false, "/h/Documentsx": false} {
		if got := TCCNote(path, "/h", "/h/bin/hommy") != ""; got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}
