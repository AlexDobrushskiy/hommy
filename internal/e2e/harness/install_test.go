package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// installRig runs ../../../install.sh against fakes: a Homebrew prefix whose brew logs its calls, uv, claude, hommy,
// uname/sw_vers/id, and --source artifacts with a hommy-browsers checkout. Nothing real is installed or called.
type installRig struct {
	t                      *testing.T
	home, prefix, bin, log string
	src                    string
	env                    []string
}

const fakeBrew = `#!/bin/bash
echo "brew $*" >>"$FAKE_LOG"
case $1 in
  shellenv) exit 0 ;;
  --repository) echo "$HOMMY_BREW_PREFIX/repo" ;;
  tap-new) mkdir -p "$HOMMY_BREW_PREFIX/repo/Library/Taps/hommy-local/homebrew-tap" ;;
  list) [[ "$2 $3" != "--cask hommy" || -n ${FAKE_CASK_INSTALLED:-} ]] || exit 1 ;;
  outdated) [[ -z ${FAKE_CASK_OUTDATED:-} ]] || { echo hommy; exit 1; } ;; # like brew: exit 1 when something is outdated
  install | upgrade) [[ $2 != --cask ]] || cp "$FAKE_CASK_BIN" "$HOMMY_BREW_PREFIX/bin/hommy" ;;
  bundle) sed 's/^/bundlefile: /' "$3" >>"$FAKE_LOG"; [[ -n ${FAKE_BUNDLE_READS_STDIN:-} ]] && cat >/dev/null; for t in $FAKE_BUNDLE_INSTALLS; do printf '#!/bin/sh\n' >"$HOMMY_BREW_PREFIX/bin/$t"; chmod +x "$HOMMY_BREW_PREFIX/bin/$t"; done ;;
esac
exit 0
`

const fakeUV = `#!/bin/bash
echo "uv $*" >>"$FAKE_LOG"
case "$1 $2" in
  "tool list") [[ -f $HOME/.uv-mlx ]] && echo "mlx-audio v0.6.6"; exit 0 ;;
  "tool install") touch "$HOME/.uv-mlx"; exit 0 ;;
esac
exit 0
`

// fakeHommy stands in for hommy: setup "installs" the signed copy and its source stamp, as service install does.
const fakeHommy = `#!/bin/bash
echo "hommy $*" >>"$FAKE_LOG"
args=("$@"); inst=''
[[ $1 == --instance ]] && { inst=$2; args=("${@:3}"); }
case ${args[0]} in
  version) echo "hommy 9.9.9" ;;
  config) echo "$HOME/hb" ;;
  browsers) echo "config.env: unchanged ($HOME/hb/config.env)"; echo "ports: lanes 4716, identity 4717; labels dev.hommy.$inst.browsers.*" ;;
  setup)
    d=$HOME/.local/share/hommy-$inst/bin
    n=0
    [[ " ${args[*]} " == *" --token-stdin "* || " ${args[*]} " == *" --token-stdin=true "* ]] && { IFS= read -r tok || true; echo "setup token-stdin: ${#tok} chars" >>"$FAKE_LOG"; }
    if ! cmp -s "$0" "$d/hommy"; then mkdir -p "$d"; cp "$0" "$d/hommy"; shasum -a 256 <"$0" | cut -c1-64 >"$d/.hommy.sha256"; n=1; fi
    echo "setup: $n changes" ;;
esac
`

func newInstallRig(t *testing.T) *installRig {
	t.Helper()
	dir := t.TempDir()
	r := &installRig{t: t, home: filepath.Join(dir, "home"), prefix: filepath.Join(dir, "brew"), bin: filepath.Join(dir, "bin"),
		log: filepath.Join(dir, "calls.log"), src: filepath.Join(dir, "artifacts")}
	for _, d := range []string{r.home, filepath.Join(r.prefix, "bin"), r.bin, filepath.Join(r.src, "app"), filepath.Join(dir, "apps")} {
		os.MkdirAll(d, 0o755)
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(r.prefix, "bin", "brew"), fakeBrew)
	write(filepath.Join(r.bin, "uv"), fakeUV)
	write(filepath.Join(r.bin, "claude"), "#!/bin/sh\necho \"claude $*\" >>\"$FAKE_LOG\"\n")
	// curl fails, except for Homebrew's installer when FAKE_BREW_INSTALLER names a stand-in (copied to curl's -o path)
	write(filepath.Join(r.bin, "curl"), `#!/bin/bash
echo "curl $*" >>"$FAKE_LOG"
[[ -n ${FAKE_BREW_INSTALLER:-} && $* == *Homebrew/install* ]] && { cp "$FAKE_BREW_INSTALLER" "${@: -1}"; exit 0; }
exit 1
`)
	write(filepath.Join(r.bin, "uname"), "#!/bin/sh\necho \"${FAKE_ARCH:-arm64}\"\n")
	write(filepath.Join(r.bin, "sw_vers"), "#!/bin/sh\necho \"${FAKE_OS:-15.1}\"\n")
	write(filepath.Join(r.bin, "id"), "#!/bin/sh\necho staff admin\n")
	write(filepath.Join(r.src, "hommy"), fakeHommy)
	for _, tool := range []string{"ffmpeg", "llama-swap"} { // present; whisper-server is missing until brew bundle
		write(filepath.Join(r.prefix, "bin", tool), "#!/bin/sh\n")
	}
	write(filepath.Join(r.src, "app", "Brewfile"), `tap "mostlygeek/llama-swap"
tap "unused/apps"
brew "ffmpeg"        # check: ffmpeg
brew "whisper.cpp"   # check: whisper-server
brew "mostlygeek/llama-swap/llama-swap" # check: llama-swap-missing
cask "unused/apps/chrome-like" # check: Chrome Like.app
`)
	os.MkdirAll(filepath.Join(dir, "apps", "Chrome Like.app"), 0o755) // installed outside Homebrew: no cask record
	// hommy-browsers: a git checkout whose install.sh reports no change
	hb := filepath.Join(dir, "hbsrc", "hommy-browsers")
	os.MkdirAll(hb, 0o755)
	write(filepath.Join(hb, "install.sh"), "#!/bin/sh\necho \"hb-install $*\" >>\"$FAKE_LOG\"\necho 'changed: 0'\n")
	r.env = []string{"HOME=" + r.home, "PATH=" + r.bin + ":/usr/bin:/bin:/usr/sbin", "HOMMY_BREW_PREFIX=" + r.prefix,
		"HOMMY_APPLICATIONS=" + filepath.Join(dir, "apps"), "FAKE_LOG=" + r.log, "FAKE_BUNDLE_INSTALLS=whisper-server llama-swap-missing",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.invalid"}
	for _, c := range [][]string{{"git", "init", "-q", "-b", "main"}, {"git", "add", "-A"}, {"git", "commit", "-q", "-m", "hb"}} {
		r.cmd(hb, c...)
	}
	r.cmd(filepath.Dir(hb), "tar", "-czf", filepath.Join(r.src, "hommy-browsers.tar.gz"), "hommy-browsers")
	return r
}

func (r *installRig) cmd(dir string, args ...string) string {
	r.t.Helper()
	c := exec.Command(args[0], args[1:]...)
	c.Dir, c.Env = dir, r.env
	out, err := c.CombinedOutput()
	if err != nil {
		r.t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

// install runs install.sh with args and extra env; it returns the output, the exit code and the fakes' call log.
func (r *installRig) install(extra []string, args ...string) (string, int, string) {
	r.t.Helper()
	os.Remove(r.log)
	script, _ := filepath.Abs("../../../install.sh")
	c := exec.Command("/bin/bash", append([]string{script}, args...)...)
	c.Dir, c.Env = r.home, append(append([]string{}, r.env...), extra...)
	out, err := c.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatal(err)
	}
	calls, _ := os.ReadFile(r.log)
	return string(out), code, string(calls)
}

// installPiped runs install.sh the way the one-liner does (`curl … | bash -s -- args`): the script arrives on stdin.
// With tty, it runs under script(1), so the run has a controlling terminal, as in a user's Terminal.
func (r *installRig) installPiped(tty bool, extra []string, args ...string) (string, int, string) {
	r.t.Helper()
	os.Remove(r.log)
	script, _ := filepath.Abs("../../../install.sh")
	pipe := []string{"/bin/sh", "-c", `cat "$0" | /bin/bash -s -- "$@"`, script}
	if tty {
		pipe = append([]string{"/usr/bin/script", "-q", "/dev/null"}, pipe...)
	}
	c := exec.Command(pipe[0], append(pipe[1:], args...)...)
	c.Dir, c.Env = r.home, append(append([]string{}, r.env...), extra...)
	out, err := c.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatal(err)
	}
	calls, _ := os.ReadFile(r.log)
	return string(out), code, string(calls)
}

func (r *installRig) args(more ...string) []string {
	return append([]string{"--source", r.src, "--instance", "unit", "--non-interactive", "--no-pair-wait"}, more...)
}

func TestInstallRefusesIntelAndOldMacOS(t *testing.T) {
	r := newInstallRig(t)
	for _, tc := range []struct{ env, want string }{{"FAKE_ARCH=x86_64", "Apple Silicon"}, {"FAKE_OS=13.6", "macOS 14"}} {
		out, code, calls := r.install([]string{tc.env}, r.args()...)
		if code != 3 || !strings.Contains(out, tc.want) || strings.Contains(calls, "brew ") {
			t.Errorf("%s: exit %d, calls %q\n%s", tc.env, code, calls, out)
		}
	}
}

// --brew-check-only names a missing tool and installs nothing: brew is never called.
func TestInstallBrewCheckOnlyNeverCallsBrew(t *testing.T) {
	r := newInstallRig(t)
	out, code, calls := r.install(nil, r.args("--brew-check-only", "--hommy-binary", filepath.Join(r.src, "hommy"))...)
	if code != 3 || !strings.Contains(out, "missing: whisper.cpp (whisper-server not found)") || strings.Contains(calls, "brew ") {
		t.Fatalf("exit %d, calls %q\n%s", code, calls, out)
	}
}

// First run installs what is missing (one brew bundle, one uv tool install, the browsers clone); the second run
// changes nothing and calls neither brew bundle nor uv tool install. Claude Code is present: its installer never runs.
func TestInstallIdempotentStages(t *testing.T) {
	r := newInstallRig(t)
	out, code, calls := r.install(nil, r.args("--hommy-binary", filepath.Join(r.src, "hommy"))...)
	if code != 0 {
		t.Fatalf("first run exit %d\n%s", code, out)
	}
	for _, want := range []string{"[2/9] Homebrew: present", "[3/9] Brewfile: installed (whisper.cpp llama-swap)", "[4/9] mlx-audio: installed",
		"[5/9] Claude Code: present", "[6/9] hommy: installed", "[7/9] hommy-browsers: installed", "[9/9] hommy setup: changed (1 changes)"} {
		if !strings.Contains(out, want) {
			t.Errorf("first run lacks %q\n%s", want, out)
		}
	}
	if !strings.Contains(calls, "brew trust --formula mostlygeek/llama-swap/llama-swap") || strings.Contains(calls, "trust --formula whisper") {
		t.Errorf("trust calls:\n%s", calls)
	}
	if strings.Count(calls, "brew bundle --file") != 1 || strings.Count(calls, "uv tool install") != 1 || strings.Contains(calls, "curl") {
		t.Errorf("first run calls:\n%s", calls)
	}
	if !strings.Contains(calls, "hommy --instance unit setup --plugin-source ") || !strings.Contains(calls, "/artifacts/app --non-interactive --no-pair-wait") {
		t.Errorf("setup call:\n%s", calls)
	}
	log, _ := os.ReadFile(filepath.Join(r.home, ".local/state/hommy-unit/install.log"))
	if n := strings.Count(string(log), "source="); n != 5 { // whisper.cpp, llama-swap, mlx-audio, the hommy binary, the browsers clone
		t.Errorf("install.log has %d source= lines:\n%s", n, log)
	}

	out, code, calls = r.install(nil, r.args("--hommy-binary", filepath.Join(r.src, "hommy"))...)
	if code != 0 || !strings.Contains(out, "install: 0 changes") {
		t.Fatalf("second run exit %d\n%s", code, out)
	}
	for _, want := range []string{"[3/9] Brewfile: present", "[4/9] mlx-audio: present", "[6/9] hommy: present", "[7/9] hommy-browsers: present"} {
		if !strings.Contains(out, want) {
			t.Errorf("second run lacks %q\n%s", want, out)
		}
	}
	if strings.Contains(calls, "brew bundle") || strings.Contains(calls, "uv tool install") {
		t.Errorf("second run installed something:\n%s", calls)
	}

	// a newer hommy binary: stage 6 upgrades
	b, _ := os.ReadFile(filepath.Join(r.src, "hommy"))
	os.WriteFile(filepath.Join(r.src, "hommy"), append(b, []byte("# newer\n")...), 0o755)
	out, _, _ = r.install(nil, r.args("--hommy-binary", filepath.Join(r.src, "hommy"))...)
	if !strings.Contains(out, "[6/9] hommy: upgraded") {
		t.Errorf("newer binary:\n%s", out)
	}
}

// Stage 3 bundles only the entries the check found missing, plus the taps they need: an app installed outside
// Homebrew (no cask record) and a present formula never reach brew bundle.
func TestInstallBundlesOnlyMissingEntries(t *testing.T) {
	r := newInstallRig(t)
	out, code, calls := r.install(nil, r.args("--hommy-binary", filepath.Join(r.src, "hommy"))...)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{`bundlefile: brew "whisper.cpp"`, `bundlefile: brew "mostlygeek/llama-swap/llama-swap"`, `bundlefile: tap "mostlygeek/llama-swap"`} {
		if !strings.Contains(calls, want) {
			t.Errorf("bundle lacks %q:\n%s", want, calls)
		}
	}
	for _, not := range []string{`bundlefile: brew "ffmpeg"`, `bundlefile: cask "unused/apps/chrome-like"`, `bundlefile: tap "unused/apps"`} {
		if strings.Contains(calls, not) {
			t.Errorf("bundle has %q:\n%s", not, calls)
		}
	}
}

// install.sh's own flags in --name=value form act like --name value (the instance, the artifacts source, the binary).
func TestInstallFlagEqualsForms(t *testing.T) {
	r := newInstallRig(t)
	out, code, calls := r.install(nil, "--source="+r.src, "--instance=unit", "--hommy-binary="+filepath.Join(r.src, "hommy"),
		"--non-interactive", "--no-pair-wait", "--vault=/tmp/v")
	if code != 0 || !strings.Contains(calls, "hommy --instance unit setup ") || strings.Contains(calls, "--instance=unit") ||
		!strings.Contains(calls, "--vault /tmp/v") {
		t.Fatalf("exit %d, calls:\n%s\n%s", code, calls, out)
	}
	if _, err := os.Stat(filepath.Join(r.home, ".local/state/hommy-unit/install.log")); err != nil {
		t.Errorf("the unit instance's install.log: %v", err)
	}
}

// Piped from curl, install.sh never takes a Brewfile from the user's current directory: it fetches its own.
func TestInstallPipedIgnoresCwdBrewfile(t *testing.T) {
	r := newInstallRig(t)
	os.WriteFile(filepath.Join(r.home, "Brewfile"), []byte("brew \"userthing\" # check: userthing-missing\n"), 0o644)
	out, code, calls := r.installPiped(false, nil, "--instance", "unit", "--non-interactive", "--no-pair-wait",
		"--hommy-binary", filepath.Join(r.src, "hommy"))
	if strings.Contains(out, "userthing") || strings.Contains(out, "unbound variable") ||
		!strings.Contains(calls, "curl -fsSL https://raw.githubusercontent.com/AlexDobrushskiy/hommy/main/Brewfile") {
		t.Fatalf("exit %d, calls:\n%s\n%s", code, calls, out)
	}
}

// Piped from curl in a Terminal, Homebrew's installer gets the terminal as stdin (so it can ask for the password),
// not the pipe that carries install.sh.
func TestInstallHomebrewInstallerGetsTerminal(t *testing.T) {
	r := newInstallRig(t)
	os.Rename(filepath.Join(r.prefix, "bin", "brew"), filepath.Join(r.home, "brew.real"))
	inst := filepath.Join(r.home, "brew-install.sh")
	os.WriteFile(inst, []byte(`if [[ -t 0 ]]; then echo "brew-installer stdin=tty" >>"$FAKE_LOG"; else echo "brew-installer stdin=pipe" >>"$FAKE_LOG"; fi
cp "$HOME/brew.real" "$HOMMY_BREW_PREFIX/bin/brew"
`), 0o644)
	out, code, calls := r.installPiped(true, []string{"FAKE_BREW_INSTALLER=" + inst}, "--source", r.src, "--instance", "unit",
		"--no-pair-wait", "--hommy-binary", filepath.Join(r.src, "hommy"))
	if code != 0 || !strings.Contains(calls, "brew-installer stdin=tty") || !strings.Contains(out, "[2/9] Homebrew: installed") {
		t.Fatalf("exit %d, calls:\n%s\n%s", code, calls, out)
	}
}

// Piped from curl, a child that reads stdin (here brew bundle) must not swallow the rest of install.sh: bash has read
// the whole script before the first command runs, so the run still ends with its summary.
func TestInstallPipedSurvivesStdinReaders(t *testing.T) {
	r := newInstallRig(t)
	out, code, _ := r.installPiped(false, []string{"FAKE_BUNDLE_READS_STDIN=1"}, "--source", r.src, "--instance", "unit",
		"--non-interactive", "--no-pair-wait", "--hommy-binary", filepath.Join(r.src, "hommy"))
	if code != 0 || !strings.Contains(out, "[9/9] hommy setup:") || !strings.Contains(out, "install: 5 changes") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// install.log names the stage that trusted a tap entry (stage 8 for an optional tool, not stage 3).
func TestInstallTrustLoggedUnderItsStage(t *testing.T) {
	r := newInstallRig(t)
	out, code, _ := r.install(nil, r.args("--hommy-binary", filepath.Join(r.src, "hommy"), "--with-whatsapp")...)
	log, _ := os.ReadFile(filepath.Join(r.home, ".local/state/hommy-unit/install.log"))
	if code != 0 || !strings.Contains(string(log), "stage=8 brew trust --formula openclaw/tap/wacli") ||
		!strings.Contains(string(log), "stage=3 brew trust --formula mostlygeek/llama-swap/llama-swap") {
		t.Fatalf("exit %d\n%s\n%s", code, log, out)
	}
}

// --token-stdin (the unattended form; also as --token-stdin=true): the token on install.sh's stdin reaches setup, even
// when an earlier stage's child reads stdin (here brew bundle; in the clean-room VM a real installer stage swallowed it).
func TestInstallTokenStdinReachesSetup(t *testing.T) {
	for _, flag := range []string{"--token-stdin", "--token-stdin=true"} {
		r := newInstallRig(t)
		os.Remove(r.log)
		script, _ := filepath.Abs("../../../install.sh")
		c := exec.Command("/bin/bash", append([]string{script}, r.args(flag, "--hommy-binary", filepath.Join(r.src, "hommy"))...)...)
		c.Dir, c.Env = r.home, append(append([]string{}, r.env...), "FAKE_BUNDLE_READS_STDIN=1")
		c.Stdin = strings.NewReader("123456:synthetic-token-for-tests\n")
		out, err := c.CombinedOutput()
		calls, _ := os.ReadFile(r.log)
		if err != nil || !strings.Contains(string(calls), "setup token-stdin: 32 chars") {
			t.Fatalf("%s: err %v\n%s\ncalls:\n%s", flag, err, out, calls)
		}
	}
}

// Stage 8 lists each optional tool once, ", "-separated.
func TestInstallOptionalLine(t *testing.T) {
	r := newInstallRig(t)
	out, code, calls := r.install(nil, r.args("--with-whatsapp", "--hommy-binary", filepath.Join(r.src, "hommy"))...)
	if code != 0 || !strings.Contains(out, "[8/9] optional: wacli installed, shfmt installed\n") {
		t.Fatalf("exit %d\n%s\ncalls:\n%s", code, out, calls)
	}
}

// Stage 6 from a local tap (--source DIR without --hommy-binary): the cask points at the archive of the cask's own
// version, even when DIR/dist also holds an older one, and an outdated cask is upgraded (brew outdated exits 1 then).
func TestInstallLocalTapUpgrade(t *testing.T) {
	r := newInstallRig(t)
	dist := filepath.Join(r.src, "dist")
	os.MkdirAll(dist, 0o755)
	for _, v := range []string{"0.1.0", "0.2.0"} {
		os.WriteFile(filepath.Join(dist, "hommy_"+v+"_darwin_arm64.tar.gz"), []byte(v), 0o644)
	}
	os.WriteFile(filepath.Join(dist, "hommy.rb"), []byte("cask \"hommy\" do\n  version \"0.2.0\"\n"+
		"  url \"https://example.invalid/hommy_0.2.0_darwin_arm64.tar.gz\"\nend\n"), 0o644)
	old, _ := os.ReadFile(filepath.Join(r.src, "hommy"))
	os.WriteFile(filepath.Join(r.prefix, "bin", "hommy"), old, 0o755) // the installed cask's binary
	env := []string{"FAKE_CASK_INSTALLED=1", "FAKE_CASK_OUTDATED=1", "FAKE_CASK_BIN=" + filepath.Join(r.src, "hommy")}
	out, code, calls := r.install(env, r.args()...)
	if code != 0 || !strings.Contains(out, "[6/9] hommy: upgraded") || !strings.Contains(calls, "brew upgrade --cask hommy-local/tap/hommy") {
		t.Fatalf("outdated cask: exit %d\n%s\ncalls:\n%s", code, out, calls)
	}
	cask, _ := os.ReadFile(filepath.Join(r.prefix, "repo/Library/Taps/hommy-local/homebrew-tap/Casks/hommy.rb"))
	real, _ := filepath.EvalSymlinks(dist) // install.sh resolves --source with pwd -P
	if want := "url \"file://" + real; !strings.Contains(string(cask), want+"/hommy_0.2.0_darwin_arm64.tar.gz\"") {
		t.Fatalf("local tap cask:\n%s", cask)
	}
}
