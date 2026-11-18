# hommy: testing

How every phase's E2E rows run, and why they can never touch the maintainer's own setup. Testers' commands live in
`e2e/README.md`; this file is the design. Master: §10, §14 D-13, D-18, D-19.

## Development

- [docs/specs/](specs/): the master spec and the phase specs. [docs/protocol.md](protocol.md): the hub, host
  and agent protocol.
- Commits are scanned for personal data before they are made and pushed (`scripts/leak-scan.sh`, see the leak-gate spec);
  forks run the self-test and the secret scan without the identifier list.

## 1. Targets and tags

A row runs against fakes unless its `#` cell carries a tag:

| Tag | Means | Default without the tag |
|---|---|---|
| `tg=real` | the real **test bot** on `api.telegram.org`, driven from the maintainer's Telegram account with `telegram-agent-cli e2e` (authorized for the test bot only: build plan › rulings) | the fake Bot API (§4) |
| `speech=real` | the instance's own speech stack (llama-swap → whisper-server, mlx_audio.server) on the slot's port (05 on) | the fake speech server (§5) |
| `claude=real` | real Claude Code, scratch `HOME`, the long-lived test token (§6) | the `claude` stub (§6) |

CI runs unit tests and Go integration tests over the same fakes and the stub; it has no agterm, no Claude and no real
Telegram. Real-Claude rows are few on purpose (01-E13, 02-E5/E10/E12 on one session, 02-E17, 04-E6, 05-E9,
06-E4, 06-E8, the VMs' doctor): each costs subscription usage.

## 2. Slots, ports, labels

Slot `N` owns ports `B = APP_PORT_BASE + SLOT_PORT_STEP·N` … `B+9` (`ops/pipeline.env`: 4710 + 10·N) and the backend
range `B+10000` … `B+10009`, nothing else. Phases: slot 0 = the phase checkout `app/` (01, 02, 05, 06); 03 works in its
own repo with slot 1's ports and a harness worktree at `$W/hommy-harness`; 04 in workslot 2 and 07's prep in workslot 3
(`scripts/slot.sh`, `docs/workslots.md`).

| Port | Layout A (02, 04–06) | Layout B (03: `--layout browsers`) |
|---|---|---|
| `B` | test hub | browsers lanes MCP |
| `B+1` | fake Telegram Bot API | browsers identity MCP |
| `B+2` | fake speech | browsers identity-a MCP |
| `B+3` | Mini App (rows that enable it) | lanes Chrome debug |
| `B+4` | speech gateway (05+) | identity Chrome debug |
| `B+5` | booking fixture site (04+) | identity-a Chrome debug |
| `B+6` | browsers lanes MCP (pool 0) | static test page |
| `B+7` | browsers identity MCP | — |
| `B+8` | lanes Chrome debug | — |
| `B+9` | identity Chrome debug | — |

Instance `test` (slot 0) / `test-s<N>`; launchd labels `dev.hommy.test[-sN].{hub,host,speech}` and
`dev.hommy.test[-sN].browsers.*` (`TEST_LAUNCHD_PREFIX`); Keychain service `hommy-test[-sN]` inside the scratch keychain
(`KEYCHAIN_TEST_PREFIX` is for the build's own test secrets in the build Mac's Keychain); agterm workspace `hm-test-s<N>`;
Agent Chrome bundle id `dev.hommy.test[-sN].agent-chrome`. `scripts/test-env.sh up` refuses (exit 3) when any port of
its slot is in use, naming it.

## 3. Scratch root, `sx`, keychain

`scripts/test-env.sh up|down|status|home [--slot N] [--layout browsers] [--tg real] [--no-start]
[--instance X --join] [--purge]`:
- Scratch root `/tmp/hm-s<N>/` (short: unix socket paths stay far below macOS's 104 bytes; outside every
  `CLAUDE.md` ancestry, so a test session never loads the build workspace's instructions): `home/` (the scratch
  `HOME`), `vault/`, logs, `kc.pw` (0600). `down --purge` removes it.
- **`scripts/sx.sh [--slot N] [--agterm-session ID] <cmd…>`**: `env -i` plus an allowlist (`USER`, `LOGNAME`,
  `SHELL`, `TERM`, `LANG`, `LC_*`, `TMPDIR`, `HOMMY_*`), `HOME=/tmp/hm-s<N>/home`, `PATH=/tmp/hm-s<N>/bin:/opt/homebrew/bin:
  /usr/bin:/bin` (`bin/` holds links to the real `claude`, `uv`, `node`, `npm`, `git`, resolved at `up`),
  `HOMEBREW_NO_AUTO_UPDATE=1`; `--agterm-session` keeps `AGTERM_SESSION_ID`. Every test command goes through it
  (`scripts/hm.sh` and `claude-session.sh` use it) except `scripts/tg.sh` and `scripts/tg-real.sh` (the latter is the
  maintainer's own `telegram-agent-cli`). Workers run inside Claude sessions that carry the maintainer's `CLAUDE_CONFIG_DIR` and
  wrapper variables; without `sx`, `claude plugin …` would act on his config. Sessions the test host opens in agterm
  get the scratch `HOME` from the host itself (master §14 D-20).
- A scratch keychain `home/Library/Keychains/hommy-test.keychain-db`, unlocked, the scratch user's default and **only**
  search-list entry: a test cannot read or write the real login keychain (01-E9). `test-env.sh` copies into it, over
  stdin, the test bot token (`hommy-test-bot-token` for `--tg real`; the fake's token otherwise) as the instance's
  `telegram-bot-token`, and the Claude test token (`hommy-test-claude-token`).
- `home/Library/Application Support/agterm/agterm.sock` → a symlink to the real agterm socket (`agtermctl` locates
  agterm through `HOME`).
- `home/.claude.json` seeded with `hasCompletedOnboarding: true` (the token stands in for the user's sign-in; no theme
  or login screens in agterm sessions).
- launchd test agents get `HOME` in their plist environment; plists live in `home/Library/LaunchAgents` and are
  bootstrapped from there (they never survive a reboot).

## 4. Fake Telegram Bot API (`hommy-e2e faketg`)

- Serves `/bot<token>/<method>` for every method the hub and `send` use: `getMe`, `getUpdates` (long poll, offsets,
  `allowed_updates`), `sendMessage`, `sendMessageDraft`, `editMessageText`, `editMessageReplyMarkup`, `deleteMessage`,
  `sendVoice`, `sendChatAction`, `setMessageReaction`, `pinChatMessage`, `unpinChatMessage`, `answerCallbackQuery`,
  `setMyCommands`, `setChatMenuButton`, `getFile`, and `/file/bot<token>/<path>`. Bot API shapes, synthetic values;
  bot `@hommy_test_bot`.
- Users: `owner` = id `1001` ("Test Owner"), `other` = id `2002`; private chats only, chat id = user id; one
  message-id space per chat (so row ids need no translation; the real bot numbers each side separately, see
  `e2e/README.md`).
- Control API under `/_fake/` (used only by `scripts/tg.sh`): inject updates (text, voice, reply, reaction, inline press,
  `stopped_message_generation`), read the chat as the user sees it (messages, reactions, keyboards, drafts), the log of
  every Bot API call, and one fault (`stt`-style: a method answers 5xx) for robustness rows.
- `scripts/tg.sh` verbs mirror `telegram-agent-cli e2e` with the same JSON and exit codes: `send text|voice
  [--reply-to ID] [--wait reply|reaction|draft]`, `react`, `press`, `stop --draft ID`, `read messages|keyboard`, `wait
  --for reply|edit|reaction|draft` (`reply` = the next new bot message, any). Fake-only: `--from owner|other`, `token`,
  `bot-url`, `calls [--method M]`, `initdata --user owner|other` (signed with the token as Telegram signs). `tg=real`
  rows use `scripts/tg-real.sh`, which passes the shared verbs to `telegram-agent-cli e2e` (the test bot; each call
  asks for the owner's OK); fake-only verbs exit 2 there. `scripts/tg.sh` is fake-only (`TG=real` exits 2).

## 5. Fake speech (`hommy-e2e fakespeech`)

`POST /v1/audio/transcriptions` → `{"text": <the next queued transcript, else the fixture's .txt matched by the audio's
SHA-256, else "fake transcript <sha8>">}`; `POST /v1/audio/speech` → a short valid Ogg Opus file; `GET /health` and
`/upstream/<model>/…` aliases so the hub's default URLs work unchanged; a request log. Control: `scripts/speech.sh
url|set-transcript TEXT|log|stop|start|fault stt|tts|none`.

## 6. Claude: stub and real

- **Stub** (`hommy-e2e fakeclaude`, at `/tmp/hm-s<N>/bin/claude-stub`, the test instance's `CLAUDE_BIN` by default):
  accepts the flags the host passes to `claude -p --output-format stream-json --verbose --include-partial-messages
  [--resume ID] [--append-system-prompt …] [--dangerously-skip-permissions]`, reads the prompt from stdin and streams
  stream-json events. Modes by prompt prefix: `echo: X` → X; `tts: X` → X plus `<tts>X</tts>`; `slow: S` → partial
  text for S seconds; `fail:` → exit 1; anything else → `ok`. `--version` and `auth status` answer like Claude Code
  (logged in); `plugin …` and `mcp …` are forwarded to the real binary (setup's plugin steps run against a stub
  `CLAUDE_BIN`). It keeps a session id per `--resume` chain and logs argv (never env).
- **Real** (`scripts/claude-session.sh [--slot N]`): the real `claude` through `sx` (scratch `HOME`, so none of the
  maintainer's settings, skills, plugins or MCP servers load) with `CLAUDE_CODE_OAUTH_TOKEN` read from the scratch
  keychain into that process's environment only. Forms: `-p --cwd DIR --stream FILE "<prompt>"` (stream-json saved;
  `e2e/README.md` › "Real Claude" lists the `jq` queries: files read, tool calls by MCP server, final text);
  `interactive` (the absolute-path `AGENT_COMMAND` of `claude=real` rows; the host appends the permission flag; one hook source per session: the scratch HOME's installed `hommy@hommy`
  plugin when it is there (05 on, `plugins-env.sh` or `install.sh`), else `--settings /tmp/hm-s<N>/claude-hooks.json`); `--audit FILE` (reads the token itself, prints only match counts over the file, `.dev/` and the scratch
  root). The token never appears in argv, a file outside the keychains, a log, a report or a transcript. Token sessions
  cannot start Remote Control, so `claude.ai` links are absent in tests (expected).
- **The token:** the long-lived token the build plan names (private companion), imported once by the workspace op
  `ops/claude-token.sh import` into the build Mac's Keychain item `hommy-test-claude-token` (an ops step under
  `KEYCHAIN_TEST_PREFIX`, not a test). In a VM it is set only in that VM's environment for the run (`launchctl setenv`
  inside the VM, plus the ssh session) and the VM is deleted at the end: it never enters an image that outlives the test.

## 7. Never the live setup

- Different everything: binary name and paths, launchd prefix `dev.hommy.test`, ports 4710–4809 (+ 10000), scratch
  root and keychain, agterm workspace `hm-test-s<N>`, the typed prefix `[Telegram · tg-` (another bridge's hooks ignore
  it), the test bot only, own speech stack, own browsers install (bundle id, labels, ports, state dir), every command
  under `sx`.
- `scripts/live-guard.sh snapshot|check` (in every phase's rows): with the workspace present it sources
  `ops/live-guard.env` (private) and records (1) the set of loaded launchd labels matching the private pattern and the
  PIDs of those that are `KeepAlive` (interval jobs change PIDs by design and are compared by presence only),
  (2) SHA-256 of the maintainer's Claude settings and plugin registry files (`settings.json`,
  `plugins/installed_plugins.json`, `plugins/known_marketplaces.json` in each of his config dirs), (3) that his ports
  answer, (4) that no `dev.hommy.*` label outside `dev.hommy.test*` is loaded and no `~/.config/hommy*` or
  `~/Library/LaunchAgents/dev.hommy.*` of the real `HOME` appeared. `check` fails on any difference. Without the
  workspace it prints `live-guard: no private config, skipped`.
- Tests never run `brew install`/`upgrade` on the build Mac (05 uses `--brew-check-only`; tools come from
  `scripts/tools.sh`), never pull Ollama models (browsers without `--with-captcha`), never write the real Claude config
  or login keychain.
- Tests never read what the machine has installed: every lookup a test asserts on (agtermctl, a tool on PATH or in a
  service dir, an app bundle) goes through a probe the test fakes, and a tool a test really runs (shfmt for the
  WhatsApp guard) comes from `scripts/tools.sh`. CI runs on GitHub-hosted runners, which have no agterm or shfmt.

## 8. Tart VM (05, 06, 07)

- Tart from `scripts/tools.sh ensure tart` (the pinned release of `openai/tart`, FSL licence: test use allowed; nothing
  through the build Mac's Homebrew). Image `ghcr.io/cirruslabs/macos-golden-gate-vanilla:latest` (≈ 30 GB download,
  ≈ 35 GB on disk; no Homebrew; `admin`/`admin`, auto-login, SSH on, passwordless sudo). The overseer pulls it during
  01 (side task). `scripts/vm.sh create NAME` refuses below **60 GB free** (`df -g ~`), clones (APFS, sparse), runs
  headless (`tart run --no-graphics`), waits `tart ip --wait 120`; `ssh`/`copy`/`run` use a throwaway known-hosts file;
  each ssh session unlocks the VM's login keychain (a recorded non-user step); `delete` stops and deletes. One hommy VM
  at a time (Apple's limit is two). The host's login keychain must be unlocked for headless runs.
- Inside a VM the fakes run from the artifacts on the VM's loopback; the tester reaches them with `scripts/vm.sh ssh
  NAME -L` forwards. `tg=real` rows need no forward.
- Lifecycle: 05 deletes its VM, 06 its VMs, 07's go step its VM **and the cached image**: nothing remains.

## 9. Real speech (05 on)

The instance's own stack: llama-swap on `B+4` with `whisper` (model `small` on the build Mac, 0.5 GB; the VM uses the
default `large-v3-turbo`) and `kokoro` (mlx-audio in the scratch `HOME`'s uv tools; Hugging Face cache in the scratch
`HOME`). Never the maintainer's llama-swap or models (design: nothing linked to his machines).

## 10. hommy-browsers test installs

Per instance (master §3.1): `HB_HOME=/tmp/hm-s<N>/home/.local/share/hommy-test[-sN]-browsers` (a checkout of the tree
under test), `config.env` from the layout (§2), pool 0 (layout A) or 1 (layout B), labels `dev.hommy.test[-sN].browsers`,
no captcha models. Layout A installs come from `scripts/plugins-env.sh up --slot N` (04: the engine from its
`origin/main`, then the plugins `hommy`, `hommy-browsers` and `retainer` in the scratch `HOME`; `e2e/README.md` ›
Plugins). `install.sh --uninstall` and `test-env.sh down` remove it; `browser-reaper --json` must list no Chrome
of the test install at the end of a row that opened one.

## 11. CI

`ci.yml`: `go` (gofmt, vet, `go test -count=1 ./...`), `shellcheck`, `leak`, `identities` on every PR and push (also
`main`). Integration tests start `faketg`, `fakespeech` and the stub in-process on random loopback ports, and a hub and
host in temp dirs with `HOME` under `$RUNNER_TEMP` (S7). No test sleeps for wall-clock time: the source's injected
clock in the hub/host packages and event waits. Runner: S7 (`vars.CREW_RUNS_ON` while private; GitHub-hosted after 07).

## 12. The real test bot

Created once, before 02's `tg=real` row, by the overseer (or 02's implementer) from the maintainer's account with
`telegram-agent-cli` (authorized for this only): `/newbot` at `@BotFather`, name "hommy test", a free username
`hommy_test_<6 random>_bot`; the token goes straight into the build Mac's Keychain item `hommy-test-bot-token` (account
`hommy`) without being printed; then `TGAC_READONLY=0 telegram-agent-cli e2e allow @<username>` (the owner's OK is the
build plan's ruling), and its `@username` goes into `~/.config/hommy-test/test-bot` (one line), the only place
`scripts/tg-real.sh` takes the bot from. `ops/logins.tsv` row `telegram-test-bot` then lists phases 02, 05–07.
