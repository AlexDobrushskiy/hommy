package harness

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The cask template (master §8.3, S4): darwin/arm64 only, the cask never uploaded before the public go, its commits
// authored by an allowed identity, the quarantine strip in its install steps, nothing published.
func TestGoreleaserCaskTemplate(t *testing.T) {
	b, err := os.ReadFile("../../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	y := string(b)
	for _, want := range []string{"goos: [darwin]", "goarch: [arm64]", "skip_upload: true", "release:\n  disable: true",
		"name: homebrew-tap", "binaries: [hommy]", `"com.apple.quarantine"`, "CGO_ENABLED=0",
		"internal/version.Version={{ .Version }}", `name_template: "hommy_{{ .Version }}_darwin_arm64"`} {
		if !strings.Contains(y, want) {
			t.Errorf(".goreleaser.yaml lacks %q", want)
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
