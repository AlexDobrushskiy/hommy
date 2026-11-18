package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDoesNotOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("# c\nEF_A=one   # note\nEF_B=\"two\"\nnot a pair\n"), 0o644)
	t.Setenv("EF_B", "set")
	os.Unsetenv("EF_A")
	t.Cleanup(func() { os.Unsetenv("EF_A") })
	Load(path)
	if os.Getenv("EF_A") != "one" || os.Getenv("EF_B") != "set" {
		t.Errorf("EF_A=%q EF_B=%q", os.Getenv("EF_A"), os.Getenv("EF_B"))
	}
	Load(filepath.Join(t.TempDir(), "missing")) // no panic
	if Get("EF_UNSET_X", "def") != "def" {
		t.Error("Get default")
	}
}
