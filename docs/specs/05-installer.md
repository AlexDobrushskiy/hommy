# 05 · Installer, release, integration

**Goal:** one command installs and wires the whole kit on a fresh Apple-Silicon Mac: Homebrew if missing, the
third-party tools from their own sources (Google Chrome included), Claude Code only when missing, hommy, hommy-browsers
configured for the instance, the plugins, the speech stack, the vault and `hommy setup`. A second run changes nothing;
a run with a newer build upgrades. Answers travel through the plugin's hooks. Proven in a scratch HOME on the build
Mac and in one fresh Tart VM.
**Depends on:** 02, 03, 04. Slot 0. **Master:** I5, I6, §3.1 (browsers per instance), §4.6 steps 6, 9, 11, §4.7 (`speech-stack`,
`speech-deep`, `plugin`, `browsers`, `retainer`, optional tools), §8, §14 D-2, D-7, D-8, D-15, D-19.

## Scope
- **In:**
  - `install.sh` (Bash, `set -euo pipefail`, shellcheck-clean): master §8.1 stages, flags, logging; refuses Intel and
    macOS < 14 (exit 3); `--source DIR` (local artifacts: `DIR/dist/` GoReleaser snapshot, `DIR/hommy-browsers.tar.gz`,
    `DIR/app` for the plugin marketplace); before the public go, stage 6 makes a local tap from `dist/` (`brew tap-new
    hommy-local/tap --no-git` + the generated cask) — in the VM only; on the build Mac `--brew-check-only` (each
    Brewfile entry checked with `command -v` or for its `.app`; brew itself is never called) and `--hommy-binary`
    (master §8.1).
  - `Brewfile` (stage 3, with `google-chrome`) and `brew bundle --no-upgrade`.
  - Integration moved here from 04 (master §14 D-15): `hommy vault init|add-recipes` (embedded template), setup step 9
    (browsers config for the instance + the three plugins) and step 11 (`--seed-context [--dry-run]`), doctor checks
    `plugin`, `browsers`, `retainer`; `scripts/test-env.sh` switches `claude-session.sh interactive` from the
    `--settings` hook file to the scratch HOME's installed plugins; `scripts/hook-probe.sh turn` (E10).
  - Speech stack (master §8.2): `internal/speech/stack` renders `llama-swap.yaml`, downloads the whisper model with a
    pinned SHA-256 (`small`, `medium`, `large-v3-turbo`), installs/validates mlx-audio, warms both models; the
    `dev.hommy[.X].speech` service; doctor `speech-stack`, `speech-deep`.
  - `.goreleaser.yaml` (master §8.3): darwin/arm64, `homebrew_casks` with `skip_upload: true` and the noreply
    `commit_author`, archive contents, snapshot naming; `scripts/release-snapshot.sh` (snapshot → `dist/`).
  - Upgrade path: a re-run with a newer `dist/` upgrades hommy (cask), re-installs the signed copy, restarts services
    once, keeps config and Keychain; hommy-browsers fast-forward + its install; `claude plugin update` for the plugins.
  - VM harness (testing.md §8): `scripts/vm.sh create|ssh|copy|run|delete NAME` (Tart from `scripts/tools.sh`; ≥ 60 GB
    free before a pull; clone, headless start, wait for the IP; unlocks the VM's login keychain for ssh sessions and
    seeds Claude's onboarding + token for the run, both recorded as non-user steps), `scripts/vm-artifacts.sh` (bundles
    `dist/`, the hommy-browsers tarball, the repo, the fakes); README "Installer and VM".
- **Out:** the public one-liner from GitHub (07); the real voice errand in the VM (06).

## Tasks
1. `install.sh` stages, idempotency checks, logging; Brewfile.
2. Integration: `hommy vault`, setup steps 9/11, doctor checks, plugin-hook wiring in the harness.
3. Speech stack + service + doctor checks.
4. GoReleaser snapshot + local tap; upgrade path.
5. VM harness; prove rows on the build Mac, then in one VM.

## E2E cases
Definitions: build-Mac rows use slot 0, instance `test`, `SX` and `T` as in 02, `H` = 02's `scripts/hm.sh` running the
build `I` installed (`HOMMY_BIN=.dev/artifacts/hommy`), scratch HOME from `scripts/test-env.sh up --no-instance` (HOME,
keychain, fakes; no instance yet); `I` = `SX ./install.sh --source .dev/artifacts --instance test
--port-base 4710 --whisper-model small --brew-check-only --hommy-binary .dev/artifacts/hommy --non-interactive --vault
/tmp/hm-s0/vault --plugin-source "$PWD" --telegram-api http://127.0.0.1:4711 --no-pair-wait --claude-bin
/tmp/hm-s0/bin/claude-stub --agent-command "$PWD/scripts/claude-session.sh interactive" --agterm-workspace hm-test-s0` (artifacts from
`scripts/vm-artifacts.sh`; the fake's token pre-seeded by `test-env.sh`); `PAIR` = 02's pairing steps through the fake.
The VM rows (E7, E8, E12) run in one VM `hommy-05` cloned from `ghcr.io/cirruslabs/macos-golden-gate-vanilla:latest`
(its re-run, extras and upgrade are 06's: master §14 D-21); inside it the fakes run from the artifacts. Every build-Mac row ends with
`scripts/live-guard.sh check` → `ok`.

| # | Given | When | Then |
|---|---|---|---|
| E1 | build Mac, scratch HOME | `I`; `PAIR`; `H doctor` | exit 0; stages 2, 3, 5 `present` (a missing tool fails stage 3 naming it: nothing is installed into the build Mac's Homebrew), stage 4 `installed` (mlx-audio in the scratch HOME's uv tools), stage 6 `installed` (from the binary), stage 7 `installed` with test ports `4716`–`4719` and label prefix `dev.hommy.test.browsers`; doctor all `✓` incl. `plugin`, `browsers`, `retainer`, `speech-stack` |
| E2 | E1 | record `shasum` of the config, `llama-swap.yaml`, the browsers `config.env`, every `dev.hommy.test*` plist, and the PIDs of every `dev.hommy.test*` service; `I` again | exit 0, `install: 0 changes`; every checksum and PID unchanged |
| E3 `speech=real` | E1 | `H doctor --deep`; `T send text "/target vault@$(H config get HOST_NAME)" --wait reply`; `T send voice e2e/fixtures/audio/hello.ogg`; `T wait --for reply --timeout 120` | `speech-deep` `✓` (transcript contains `hello`, synthesis is Ogg Opus); the stub's reply (`ok`) arrives and `scripts/observe.sh prompts` shows the run's prompt with the transcript containing `hello` |
| E4 `speech=real` | E1 | `H service stop speech`; `H doctor; echo $?`; `H service start speech`; `H doctor; echo $?` | `✗ speech …` + `fix: hommy service restart speech`, exit 1; then all `✓`, exit 0 |
| E5 | E1 | `codesign -d -r- /tmp/hm-s0/home/.local/share/hommy-test/bin/hommy`; rebuild the artifacts with a bumped version; `I`; the same `codesign`; `H version` | the same designated requirement both times (identifier + certificate leaf, no cdhash); the new version |
| E6 | clean tree | `gh api repos/AlexDobrushskiy/homebrew-tap/commits/HEAD --jq .sha`; `scripts/release-snapshot.sh`; `ls dist/`; the tap HEAD again | `dist/` holds the darwin/arm64 archive and `hommy.rb`; the tap HEAD is unchanged (S4) |
| E7 | VM `hommy-05` fresh (no Homebrew, no Claude Code); artifacts copied | `scripts/vm.sh run hommy-05 -- ./install.sh --source artifacts --non-interactive --telegram-api http://127.0.0.1:4711 --no-pair-wait`; pair through the VM's fake (README "Installer and VM"); `scripts/vm.sh run hommy-05 -- hommy doctor` | exit 0; the log shows Homebrew `installed` from `Homebrew/install`, Claude Code `installed` via `claude.ai/install.sh`, agterm and Google Chrome casks `installed`, mlx-audio from PyPI, hommy from the local tap; doctor all `✓` |
| E8 | clean tree; E7's install log | `git ls-files \| xargs file \| grep -ciE 'Mach-O\|ELF'`; `scripts/vm.sh run hommy-05 -- grep -c 'source=' .local/state/hommy/install.log` | `0` vendored binaries; one `source=` line per installed tool, each naming its own origin |
| E9 `claude=real` | E1 + `PAIR` (default target `new@…:interactive`, set again with `/target new@<host>:/tmp/hm-s0/vault:interactive` after E3/E10; the scratch HOME's plugins carry the hooks, no `--settings` file) | `T send text "Reply with exactly the word pong and add a tts block that says pong."`; `T wait --for reply --timeout 180` until the answer and the voice; `grep -c stop /tmp/hm-s0/home/.local/state/hommy-test/host/hook.log` and the turn's lines of that log; `grep -c 'hommy hook' /tmp/hm-s0/home/.claude/settings.json` | the `🆕` line, `pong` as a reply, then a voice note; the session's claude runs with `HOME=/tmp/hm-s0/home` and no `--settings` (`ps eww`, master §14 D-20); `≥ 1` stop line from the plugin's hook and exactly one line per hook event of the turn (one hook source); `0` |
| E10 | E1 + `PAIR`; `/target vault@<host>`; `T send text "echo: probe" --wait reply` (its id → `M`) | `H service stop host`; `time scripts/hook-probe.sh turn --session probe-1 --msg M --text ok` (pipes a UserPromptSubmit starting `[Telegram · tg-M]` then a Stop payload through the installed plugin's `bin/hommy-hook`); `ls /tmp/hm-s0/home/.local/state/hommy-test/host/hookspool/`; `H service start host`; `T wait --for reply --timeout 60` | each hook run exits 0, no output, under 5 s; the spool holds the turn; after the start it is delivered (spool empty within 30 s) and the fake bot sends `ok` |
| E11 | E1 | `mkdir /tmp/hm-s0/v2 && echo '# mine' > /tmp/hm-s0/v2/CLAUDE.md`; `H vault add-recipes /tmp/hm-s0/v2`; `ls /tmp/hm-s0/v2/prompt`; `cat /tmp/hm-s0/v2/CLAUDE.md`; `H setup --non-interactive --seed-context --dry-run`; `SX claude plugin disable hommy@hommy`; `H doctor; echo $?`; `SX claude plugin enable hommy@hommy`; `rm -rf /tmp/hm-s0/v2` | the three recipes; `# mine` unchanged; the routing rows printed; the dry run prints an `agtermctl session new … --workspace-name hm-test-s0 --cwd /tmp/hm-s0/vault` line and a first message naming the retainer interview and "do not rewrite AGENTS.md"; `✗ plugin` + its fix line, exit 1 |
| E12 | all VM rows done | `scripts/vm.sh delete hommy-05`; `.dev/tools/tart list`; `df -g ~` | the VM is gone; free space within 2 GB of the value before E7 (the cached image stays for 06 and 07) |

**Unit:** stage functions' idempotency decisions with fake `brew`/`uv`/`claude` on PATH; browsers config per instance;
llama-swap config rendering; model SHA table; cask template; `vault add-recipes` never overwrites.

## Implementation notes

- Stage 4 installs `"mlx-audio[server,tts]"` (master §8.1 says `[server]`): mlx-audio 0.6.6 moved Kokoro's
  dependencies (`pillow`, `mistral-common`) into the `tts` extra; Python is uv's managed 3.12 (`--python 3.12`), logged as a
  source.
- `lang_code`: the real mlx-audio Kokoro (0.6.6) aliases the ISO codes the hub sends (`en→a`, `es→e`, `fr→f`, `it→i`,
  `pt→p`, `hi→h`; `pipeline.py` `ALIASES`) and accepts both forms (each answered 200 with Ogg Opus on the instance's stack),
  so `speech.LangCode` keeps the master's ISO codes; a unit test pins the set we send.
- The speech plist sets `WorkingDirectory` = `<state>/speech`: `whisper-server --convert` writes its temporary WAV into
  the working directory, and launchd's default `/` made every transcription fail (`FFmpeg conversion failed`).
- `hommy setup` prints `setup: N changes`; `install.sh` adds its own stages' changes for `install: N changes`. A stage
  counts `installed`, `upgraded` or `changed`; `present` and `skipped` count 0. Only installs write `source=` lines to
  the install log (E8).
- New flags: `hommy setup --agterm-workspace` (the tests' `hm-test-s0`; before, only `test-env.sh` wrote that key),
  `--agent-command`, `--plugin-source`, `--seed-context [--dry-run]`; `hommy browsers configure` (the instance's
  hommy-browsers `config.env`, shared by `install.sh` stage 7 and setup step 9); `hommy hub host-key remove NAME`
  (overseer note; refuses this Mac's own host, exit 3).
- hommy-browsers runs on Homebrew's Node: `config.env` gets `HB_NODE=/opt/homebrew/opt/node/bin/node` (overseer note:
  03's scratch install picked a version manager's Node from `PATH`).
- Agent Chrome's ad-hoc re-sign after a Chrome update (overseer note): Playwright launches it with `--use-mock-keychain
  --password-store=basic` (seen on the running lane Chrome), the scratch keychain holds no "Safe Storage" item, and
  no TCC grant is requested, so a changed cdhash has nothing to re-prompt; no signing change.
- The VM image's disk (41 GB, 6 GB free) is under stage 1's 10 GB: `scripts/vm.sh create` grows it to 80 GB
  (`tart set --disk-size`, then the in-guest `diskutil` resize), a recorded non-user step.
- agterm's first two launches show its Welcome alert and Sparkle's "check for updates automatically?" prompt; its
  control socket answers only after both (agterm 0.43.0). The VM pre-answers them in `vm.sh create` (recorded non-user
  steps); on a real Mac `hommy setup` names both dialogs and waits up to 3 min for agterm.
- Deviation (master §8.1 stage 3): `brew bundle --no-upgrade` runs over a temporary Brewfile holding only the entries
  the `# check:` found missing, plus the taps they come from. brew bundle judges "installed" by Homebrew's own records,
  so a whole-Brewfile bundle re-installed tools present another way (in the VM, Homebrew 7.0.14: git beside the
  Command Line Tools' git, and a Chrome installed from its DMG adopted as the `google-chrome` cask); master §8.1:
  installed only when missing.
- Under `curl … | bash` the script arrives on stdin: Homebrew's installer gets `/dev/tty` (else it sees no terminal,
  runs non-interactively and `sudo -n` aborts), and the Brewfile comes from `--source` or GitHub, never the cwd.
  `--name=value` reads as `--name value`.
- Deviation (Definitions): `test-env.sh up --no-instance` instead of `--no-start`: the fakes must run (setup's `getMe`
  goes to the fake), and `--no-start` means "fakes not started" (01). `H` runs the installed build, because 02's
  `bin/hommy` is a different build and doctor's `binary` check rightly compares versions. `I` adds
  `--agterm-workspace hm-test-s0` (the dry run in E11 names that workspace; nothing else sets it without the old
  instance file).
- Deviation (E3): the hub log never holds message text; the transcript passed to the run is read from the run's job
  (`scripts/observe.sh prompts`).
- Deviation (E7): `--source artifacts` (relative to the VM HOME, `vm.sh run`'s cwd): a literal `~/artifacts` is
  expanded by the build Mac's shell before ssh.
- Deviation (E9): the default target is set again after E3/E10's `/target vault@…`; the row also asserts the one-hook-
  source guard (overseer note: plugin hooks and 02's `--settings` file both firing).

## Done
E1–E12 green; README "Installer and VM"; CI green.

## Risks
- Homebrew's installer asks for the admin password on a real Mac; the vanilla VM has passwordless sudo, so the prompt
  is not exercised (the README says a password is asked once).
- mlx-audio's Kokoro needs `misaki[en]` and the spaCy model in its tool env; a bare install fails at the first
  synthesis (`doctor --deep` catches it).
- Model downloads are large (whisper up to 1.6 GB, Kokoro 0.3 GB): the build Mac uses `small`; the VM the default once.
- The Tart image (≈ 30 GB) is pulled by the overseer during 01 (side task), so E7 does not wait for it.
