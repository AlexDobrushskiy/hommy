package harness

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The cask template (master §8.3, S4): darwin/arm64 only; a tag release (the go step) publishes the release and the
// cask, with CHANGELOG.md's notes, its commits by an allowed identity; snapshots publish nothing and get no token; the
// quarantine strip in its install steps.
func TestGoreleaserCaskTemplate(t *testing.T) {
	b, err := os.ReadFile("../../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	y := string(b)
	for _, want := range []string{"goos: [darwin]", "goarch: [arm64]", "skip_upload: false", "release:\n  github:\n    owner: AlexDobrushskiy\n    name: hommy",
		"  mode: keep-existing\n", "  replace_existing_artifacts: true\n",
		"changelog:\n  # snapshots: none; a tag release takes its notes from CHANGELOG.md (--release-notes; a disabled changelog drops them)\n  disable: \"{{ .IsSnapshot }}\"",
		"name: homebrew-tap", "binaries: [hommy]", `"com.apple.quarantine"`, "CGO_ENABLED=0",
		"internal/version.Version={{ .Version }}", `name_template: "hommy_{{ .Version }}_darwin_arm64"`} {
		if !strings.Contains(y, want) {
			t.Errorf(".goreleaser.yaml lacks %q", want)
		}
	}
	snap, err := os.ReadFile("../../../scripts/release-snapshot.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-u GITHUB_TOKEN", "release --snapshot", "--skip=publish"} {
		if !strings.Contains(string(snap), want) {
			t.Errorf("scripts/release-snapshot.sh lacks %q", want)
		}
	}
	m := regexp.MustCompile(`commit_author:\s*\n\s*name: .*\n\s*email: (\S+)`).FindStringSubmatch(y)
	if m == nil {
		t.Fatal("no commit_author email")
	}
	ids, _ := os.ReadFile("../../../.github/commit-identities")
	allowed := false
	for _, l := range strings.Split(string(ids), "\n") {
		allowed = allowed || (strings.TrimSpace(l) == m[1] && !strings.HasPrefix(l, "#"))
	}
	if !allowed || !strings.HasSuffix(m[1], "@users.noreply.github.com") {
		t.Errorf("commit_author %q is not the noreply identity of .github/commit-identities", m[1])
	}
}
