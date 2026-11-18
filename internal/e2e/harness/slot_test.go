package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSlotReleaseWithSameNamedTag runs scripts/slot.sh claim + release in a throwaway repo (its own origin, a stub
// test-env.sh) where a tag has the slot branch's name: release must remove the worktree and the branch, keep the tag.
func TestSlotReleaseWithSameNamedTag(t *testing.T) {
	dir := t.TempDir()
	work, origin := filepath.Join(dir, "work"), filepath.Join(dir, "origin.git")
	env := []string{"PATH=/opt/homebrew/bin:/usr/bin:/bin", "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1",
		"HOMMY_SCRATCH_BASE=" + filepath.Join(dir, "scratch")}
	run := func(cwd, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir, cmd.Env = cwd, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
		return string(out)
	}
	for _, f := range []string{"slot.sh", "lib/env.sh"} {
		b, err := os.ReadFile(filepath.Join("../../../scripts", f))
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(work, "scripts", f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "scripts", "test-env.sh"), []byte("#!/bin/bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(dir, "git", "init", "-q", "--bare", "-b", "main", origin)
	run(work, "git", "init", "-q", "-b", "main")
	run(work, "git", "add", "-A")
	run(work, "git", "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "init")
	run(work, "git", "remote", "add", "origin", origin)
	run(work, "git", "push", "-q", "origin", "main")

	slotDir := strings.TrimSpace(run(work, "scripts/slot.sh", "claim", "phase-04", "--slot", "1"))
	run(work, "git", "tag", "slot1/phase-04")
	if got := run(work, "scripts/slot.sh", "list"); !strings.Contains(got, "\tslot1/phase-04\t") {
		t.Errorf("list shows %q, want the bare branch name", got)
	}
	run(work, "scripts/slot.sh", "release", "1")
	if _, err := os.Stat(slotDir); !os.IsNotExist(err) {
		t.Errorf("worktree %s still exists", slotDir)
	}
	refs := run(work, "git", "for-each-ref", "--format=%(refname)")
	if strings.Contains(refs, "refs/heads/slot1/phase-04") || !strings.Contains(refs, "refs/tags/slot1/phase-04") {
		t.Errorf("after release: refs %q, want the branch gone and the tag kept", refs)
	}
}
