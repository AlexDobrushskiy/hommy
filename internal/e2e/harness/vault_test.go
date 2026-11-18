package harness

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	vaulttemplate "github.com/AlexDobrushskiy/hommy/vault-template"
)

const (
	vaultSh   = "../../../scripts/vault.sh"
	vaultLint = "../../../scripts/vault-lint.sh"
)

func run(t *testing.T, name string, args ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func initVault(t *testing.T) string {
	t.Helper()
	v := filepath.Join(t.TempDir(), "vault")
	if out, code := run(t, vaultSh, "init", v); code != 0 {
		t.Fatalf("init: %d %s", code, out)
	}
	return v
}

func files(t *testing.T, fsys fs.FS) []string {
	t.Helper()
	var out []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestVaultInitIsTheEmbeddedTemplate(t *testing.T) {
	v := initVault(t)
	got, want := files(t, os.DirFS(v)), files(t, vaulttemplate.FS)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("init wrote %v\nembedded %v", got, want)
	}
	for _, f := range got {
		if strings.HasSuffix(f, ".go") {
			t.Errorf("%s copied into the vault", f)
		}
	}
	for _, f := range []string{"AGENTS.md", "CLAUDE.md", "index.md", "log.md", "context/places.md", "active/Dashboard.md",
		"decisions/README.md", "inbox/README.md", "archive/README.md", "_templates/capture-note.md",
		"prompt/voice-replies.md", "prompt/book-local-service.md", "prompt/email-triage.md", "prompt/find-and-buy.md"} {
		if _, err := fs.Stat(vaulttemplate.FS, f); err != nil {
			t.Errorf("template lacks %s", f)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(v, "CLAUDE.md")); string(b) != "@AGENTS.md\n" {
		t.Errorf("CLAUDE.md = %q", b)
	}
	if out, code := run(t, vaultLint, v); code != 0 || out != "ok\n" {
		t.Fatalf("lint of a fresh vault: %d %s", code, out)
	}
	// each recipe routed from exactly one line of the kernel
	agents, _ := os.ReadFile(filepath.Join(v, "AGENTS.md"))
	for _, r := range []string{"book-local-service", "email-triage", "find-and-buy"} {
		n := 0
		for _, l := range strings.Split(string(agents), "\n") {
			if strings.Contains(l, r) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s on %d lines of AGENTS.md", r, n)
		}
	}
}

func TestVaultInitRefusesANonEmptyDir(t *testing.T) {
	v := initVault(t)
	if out, code := run(t, vaultSh, "init", v); code != 3 || !strings.Contains(out, "not empty") {
		t.Fatalf("second init: %d %s", code, out)
	}
	d := t.TempDir()
	_ = os.WriteFile(filepath.Join(d, ".hidden"), []byte("x"), 0o600)
	if _, code := run(t, vaultSh, "init", d); code != 3 {
		t.Fatalf("init into a dir with a hidden file: %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(d, ".hidden")); string(b) != "x" {
		t.Fatal("a refused init touched the dir")
	}
}

func TestVaultLintCatchesEachBreak(t *testing.T) {
	breaks := map[string]func(v string){
		"routing table names prompt/email-triage.md": func(v string) { _ = os.Remove(filepath.Join(v, "prompt/email-triage.md")) },
		"does not list notes/idea.md": func(v string) {
			_ = os.MkdirAll(filepath.Join(v, "notes"), 0o755)
			_ = os.WriteFile(filepath.Join(v, "notes/idea.md"), []byte("x\n"), 0o644)
		},
		"empty folder": func(v string) { _ = os.MkdirAll(filepath.Join(v, "projects"), 0o755) },
		"does not start with ## TL;DR": func(v string) {
			_ = os.WriteFile(filepath.Join(v, "context/bio.md"), []byte("# Bio\n## TL;DR\n"), 0o644)
		},
		"routing row without a path":       func(v string) { appendRow(t, v, "| Something | ask the owner |") },
		"routing table names context/x.md": func(v string) { appendRow(t, v, "| X | context/x.md |") },
	}
	for want, brk := range breaks {
		v := initVault(t)
		brk(v)
		out, code := run(t, vaultLint, v)
		if code != 1 || !strings.Contains(out, want) {
			t.Errorf("%s: exit %d, %q", want, code, out)
		}
	}
	// files inside inbox/ and archive/ need no index entry
	v := initVault(t)
	_ = os.WriteFile(filepath.Join(v, "inbox/2026-01-02-0900-idea.md"), []byte("x\n"), 0o644)
	if out, code := run(t, vaultLint, v); code != 0 {
		t.Errorf("inbox capture: %d %s", code, out)
	}
}

// appendRow adds a row under the routing table's last row.
func appendRow(t *testing.T, v, row string) {
	t.Helper()
	p := filepath.Join(v, "AGENTS.md")
	b, _ := os.ReadFile(p)
	s := strings.Replace(string(b), "| A new capture, processing the inbox |", row+"\n| A new capture, processing the inbox |", 1)
	if s == string(b) {
		t.Fatal("routing table anchor not found")
	}
	_ = os.WriteFile(p, []byte(s), 0o644)
}

func TestVaultAddPlace(t *testing.T) {
	v := initVault(t)
	if out, code := run(t, vaultSh, "add-place", v, "Barbearia Exemplo", "http://127.0.0.1:4735/", "--kind", "barber"); code != 0 {
		t.Fatalf("add-place: %d %s", code, out)
	}
	run(t, vaultSh, "add-place", v, "Padaria Exemplo", "http://127.0.0.1:4799/")
	run(t, vaultSh, "add-place", v, "barbearia exemplo", "http://127.0.0.1:4735/book")
	b, _ := os.ReadFile(filepath.Join(v, "context/places.md"))
	s := string(b)
	if !strings.Contains(s, "| barbearia exemplo | barber | http://127.0.0.1:4735/book | — |\n| Padaria Exemplo | — | http://127.0.0.1:4799/ | — |") ||
		strings.Count(s, "xemplo |") != 2 {
		t.Fatalf("places.md:\n%s", s)
	}
	if _, code := run(t, vaultSh, "add-place", v, "A|B", "http://x/"); code == 0 {
		t.Error("a name with | was accepted")
	}
	if out, code := run(t, vaultLint, v); code != 0 {
		t.Errorf("lint after add-place: %s", out)
	}
}
