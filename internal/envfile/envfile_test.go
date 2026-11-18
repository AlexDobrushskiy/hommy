package envfile

import "testing"

func TestGet(t *testing.T) {
	t.Setenv("EF_SET", "v")
	t.Setenv("EF_EMPTY", "")
	if Get("EF_SET", "def") != "v" || Get("EF_EMPTY", "def") != "def" || Get("EF_UNSET_X", "def") != "def" {
		t.Error("Get")
	}
}
