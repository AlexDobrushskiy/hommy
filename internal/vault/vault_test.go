package vault

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitWritesTemplate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "v")
	n, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(Files()) || n < 15 {
		t.Fatalf("wrote %d of %d files", n, len(Files()))
	}
	for _, p := range []string{"AGENTS.md", "CLAUDE.md", "prompt/book-local-service.md", "_templates/capture-note.md"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "embed.go")); err == nil {
		t.Error("embed.go copied")
	}
	if _, err := Init(dir); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("second init: %v", err)
	}
}

func TestInitEmptyDir(t *testing.T) {
	if _, err := Init(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

// add-recipes never overwrites: an existing CLAUDE.md and a user's own recipe stay byte for byte.
func TestAddRecipesNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# mine\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "prompt"), 0o755)
	os.WriteFile(filepath.Join(dir, "prompt", "email-triage.md"), []byte("my own triage\n"), 0o644)
	added, rows, err := AddRecipes(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range added {
		if p == "CLAUDE.md" || p == "prompt/email-triage.md" {
			t.Errorf("overwrote %s", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); string(b) != "# mine\n" {
		t.Errorf("CLAUDE.md = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "prompt", "email-triage.md")); string(b) != "my own triage\n" {
		t.Errorf("email-triage.md = %q", b)
	}
	for _, r := range []string{"book-local-service.md", "find-and-buy.md"} {
		if _, err := os.Stat(filepath.Join(dir, "prompt", r)); err != nil {
			t.Errorf("recipe %s missing", r)
		}
	}
	joined := strings.Join(rows, "\n")
	for _, r := range []string{"prompt/book-local-service.md", "prompt/email-triage.md", "prompt/find-and-buy.md"} {
		if !strings.Contains(joined, r) {
			t.Errorf("no routing row for %s in %q", r, joined)
		}
	}
	again, _, err := AddRecipes(dir)
	if err != nil || len(again) != 0 {
		t.Fatalf("re-run added %v (%v)", again, err)
	}
}

func TestAddRecipesNeedsFolder(t *testing.T) {
	if _, _, err := AddRecipes(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("absent folder accepted")
	}
}
