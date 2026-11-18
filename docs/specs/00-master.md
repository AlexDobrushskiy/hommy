# hommy: master spec

Status: r2 by `hm-spec`, 2026-11-17, for `hm-overseer` (spec approval delegated); revised after critique round 1 (§15).
**Authority:** the design [`docs/design.md`](../../../docs/design.md) (every Decision fixed) > the build plan
[`docs/build-plan.md`](../../../docs/build-plan.md) > the stage-0 rulings `docs/decisions/2026-11-17-stage0.md` (S1–S8)
> this spec > phase specs. Changes to the plan: §14. Test detail: [`../testing.md`](../testing.md). Machine-specific
values the build needs (source paths, the maintainer's live labels) are in the workspace's private companion
`docs/spec-private.md`, never here: **this repo goes public and names no person, host, path, account or LAN of the
maintainer.** Examples use invented names (the barber is "Barbearia Exemplo"; the test owner is Telegram user `1001`).

## 1. Invariants

| # | Invariant |
|---|---|
| I1 | **No personal data in any repo, at any time, in any commit.** Blobs, path names, commit headers and messages of `hommy`, `hommy-browsers` and the tap are scanned (§9) at pre-commit, pre-push, lane commit, ship and CI, and every served ref before the public go (S8). A hit stops the commit; the fix replaces the data with synthetic values. |
| I2 | **The maintainer's live setup is never touched, at any phase**: his bridge, bot, hub, launchd agents, Claude config dirs (settings, plugins, skills), browsers, vault, llama-swap, his private browsers repo and its clones, his Homebrew. Tests run under `scripts/sx.sh` (§10) with test bots, `dev.hommy.test*` labels, slot ports, scratch `HOME`s and their own speech stack; `scripts/live-guard.sh` proves it per phase. |
| I3 | **Nothing linked to the maintainer's machines** in the product, docs, tests or CI: no host name, LAN address, domain or account of his, no runtime dependency on any service of his. |
| I4 | **Secrets live only in the macOS Keychain** (§3.4): never in the config file, a plist, argv, a log, a transcript or a test report. Config files are 0600 and hold no secret. |
| I5 | **The installer is idempotent.** A re-run with nothing new upstream changes no file, plist, Keychain item or service (same PIDs); a re-run with a newer hommy upgrades it and keeps every user value. |
| I6 | **Every third-party tool installs from its own source** (§8), never vendored. **Claude Code is never bundled**: Anthropic's official installer runs only when `claude` is missing; the user signs in with their own subscription. |
| I7 | **Agent permissions default to skip** (`--dangerously-skip-permissions` on hommy-started sessions); `hommy config permissions ask\|skip` changes it. |
| I8 | **Only the paired owner reaches an agent.** Before pairing the hub accepts nothing but a private-chat `/start <code>` and sends nothing at all; after it, only the owner's private chat. Everything else is dropped without a reply. |
| I9 | **Delivery as the source has it** (§4.1): a job is stored before it is pushed, acked after an fsynced ledger write, re-pushed until acked, idempotent per `event_id`; `run_p` is at-most-once; a hook run never prints and always exits 0. |
| I10 | **macOS on Apple Silicon only** (arm64, macOS 14+): launchd, Keychain, TCC, agterm and mlx are the platform. No Linux host installer, no server path, no tmux backend (design › Not built). |

## 2. Architecture

```
Telegram ──▶ hub (dev.hommy.hub, 127.0.0.1:8470) ──WS──▶ host (dev.hommy.host) ──agtermctl──▶ agterm session: claude in the vault
   ▲            │  SQLite: events, jobs, owner, pings          │ ledger, hook relay (unix socket), typing queue        │
   │            │  STT/TTS ──▶ speech gateway (dev.hommy.speech: llama-swap → whisper-server, mlx_audio.server)   │
   └── text + voice ◀── hub ◀── hook_answer ◀── host relay ◀── `hommy hook stop` (plugin hook) ◀──────────────────────┘
```

- **One binary `hommy`** (Go module `github.com/AlexDobrushskiy/hommy`, `cmd/hommy`), copied from the source bridge's
  tree (no history) and renamed. Packages keep the source's split: `internal/hub`, `host`, `proto`, `agterm`, `hook`,
  `agentcli`, `claude`, `speech`, `sanitize`, `textx`, `ping`, `envfile`, `version`; new: `internal/config` (instance,
  paths, keys), `internal/secret` (Keychain), `internal/setup`, `internal/doctor`, `internal/service` (launchd, stable
  copy, signing), `internal/pairing`; the vault template is embedded with `go:embed` (an `embed.go` inside
  `vault-template/`, never copied into a vault). The source's no-subcommand single-host bot, its `send.py`
  compatibility, its Codex hook installer and its personal bot commands are not carried over (§14 D-9).
- **Single-Mac topology (default):** hub and host on one Mac under launchd, hub on loopback, host named after the
  Mac's `LocalHostName` (lowercased, `[a-z0-9-]`). **More hosts** (another Mac) run `hommy host` only, joined with
  `hommy setup --join` (§4.6) after the hub Mac sets `HUB_LISTEN` to a LAN address (plain WebSocket + host-key bearer,
  as the source; the LAN path is documented in the README and proven over loopback, §14 D-17). No server, Docker,
  Ansible, tunnel or egress proxy.
- **Reference flow** (design › What, with the invented barber): voice note → hub transcribes (whisper behind the speech
  gateway) → `new_session` job → host opens an agterm session (workspace `hommy`) running `claude` in the vault with a
  brief folder → the vault's routing table → `prompt/book-local-service.md` → lane browser (hommy-browsers) → asks the
  owner (`hommy send --buttons`) → books → writes `log.md` → Stop hook → host relay → hub → Telegram text, then voice
  (kokoro behind the speech gateway).
- **Repos:** `hommy` (this: Go, plugin, vault template, installer, specs), `hommy-browsers` (the generic browser
  engine + its own plugin, design D8), the tap `AlexDobrushskiy/homebrew-tap` (the `hommy` cask from phase 07 only, S4).

### 2.1 Repo layout (hommy)

| Path | What |
|---|---|
| `cmd/hommy/`, `internal/…` | the binary |
| `cmd/hommy-e2e/`, `e2e/` | test harness: fake Telegram Bot API, fake speech, `claude` stub, booking fixture, static page; `e2e/README.md`; fixtures |
| `plugin/`, `.claude-plugin/marketplace.json` | the Claude Code plugin and its marketplace (§5) |
| `vault-template/` | the vault template (§6), embedded in the binary |
| `install.sh`, `Brewfile`, `.goreleaser.yaml` | installer and release (§8) |
| `scripts/` | leak gate (`scripts/leak/`), dev/test helpers (`sx.sh`, `test-env.sh`, `tg.sh`, …), `tools.sh` (pinned tool downloads), `ci-preflight.sh`, `ci-probe.sh` |
| `docs/` | `specs/`, `testing.md`, `protocol.md` (02), `flaky.md`, `clean-room.md` (06), `release.md` (07) |

## 3. Configuration

### 3.1 Instance

`--instance NAME` (env `HOMMY_INSTANCE`; `[a-z0-9-]{1,24}`) parametrises every path, label, Keychain service, the port
base and the browsers install, so a test install never collides with a real one on the same Mac. Default instance = no
suffix.

| Thing | Default instance | Instance `X` |
|---|---|---|
| Config dir | `~/.config/hommy/` | `~/.config/hommy-X/` |
| State dir | `~/.local/state/hommy/` (`hub/`, `host/`, `speech/`, `spool/`, `inbox/`, logs) | `~/.local/state/hommy-X/` |
| Share dir | `~/.local/share/hommy/` (`bin/hommy` signed copy, `briefs/`, `models/`) | `~/.local/share/hommy-X/` |
| launchd labels | `dev.hommy.hub`, `dev.hommy.host`, `dev.hommy.speech` | `dev.hommy.X.hub` … |
| Keychain service | `hommy` | `hommy-X` |
| Signing keychain | `~/Library/Keychains/hommy-signing.keychain-db` | `hommy-X-signing.keychain-db` |
| agterm workspace | `hommy` | `AGTERM_WORKSPACE` (tests: `hm-test-s<N>`) |
| Ports | `PORT_BASE=8470` | set by setup (`--port-base`); tests: the slot base (testing.md §2) |
| hommy-browsers | `HB_HOME=~/.local/share/hommy-browsers`, engine defaults (8931/9331, `dev.hommy.browsers.*`, bundle `dev.hommy.agent-chrome`) | `~/.local/share/hommy-X-browsers`, MCP `PORT_BASE+6`, debug `PORT_BASE+8`, `dev.hommy.X.browsers.*`, bundle `dev.hommy.X.agent-chrome`, pool 0 |

Test instances are `test` (slot 0) and `test-s<N>`; their labels therefore start `dev.hommy.test`
(`TEST_LAUNCHD_PREFIX` in `ops/pipeline.env`, as are `APP_PORT_BASE`, `SLOT_PORT_STEP`, `KEYCHAIN_TEST_PREFIX`).

### 3.2 Config file

`<config dir>/hommy.env`: `KEY=VALUE` lines (the source's `internal/envfile` format), `#` comments, 0600, written by
`hommy setup` / `hommy config set`, read by every subcommand. Precedence: process env > file > default. `--config F`
(env `HOMMY_CONFIG`) reads another file. An unknown key logs `unknown setting KEY` and is ignored; an invalid value
fails start naming the key. Keys and defaults (source names kept where they existed; `*_FILE` secret keys removed):

| Key | Default | Used by |
|---|---|---|
| `PORT_BASE` | `8470` | derives the ports below when they are unset |
| `HUB_LISTEN` | `127.0.0.1:$PORT_BASE` | hub |
| `HUB_URL` | `ws://127.0.0.1:$PORT_BASE/ws` | host, `send`, `inbox`, `pair`, `doctor` |
| `HUB_DB` | `<state>/hub/hub.sqlite3` | hub |
| `TELEGRAM_API_URL` | `https://api.telegram.org` (tests: the fake) | hub, `send` fallback, `doctor` |
| `ALLOWED_UPDATES` | as the source | hub |
| `DEFAULT_TARGET` | `new@$HOST_NAME:$VAULT_DIR:interactive` | hub |
| `PAIRING_TTL` | `30m` | hub |
| `OWNER_CHAT_ID` | written after pairing (not a secret) | `send` direct fallback only; the hub reads the owner from its DB |
| `HOST_SILENT_AFTER`, `QUEUED_NOTICE_AFTER` | `2m`, `2m` | hub |
| `DRAFT_EVERY`, `DRAFT_KEEPALIVE`, `NEW_LINK_WAIT` | `1100ms`, `4s`, `15s` | hub |
| `MINIAPP_LISTEN`, `MINIAPP_URL` | empty (off); opt-in `127.0.0.1:$PORT_BASE+3` + an `https://` URL | hub |
| `LOG_UPDATES` | `0` | hub |
| `WHISPER_ENDPOINTS` | `local\|http://127.0.0.1:$PORT_BASE+4/upstream/whisper/v1\|whisper` | hub |
| `GEMINI_MODEL` | `gemini-2.5-flash` (used only when Keychain `gemini-api-key` exists) | hub |
| `TTS_URL` | `http://127.0.0.1:$PORT_BASE+4/upstream/kokoro/v1`; `off` disables voice | hub |
| `TTS_MODEL`, `TTS_DEFAULT_VOICE` | `mlx-community/Kokoro-82M-bf16`, `af_heart` | hub |
| `TTS_LANG_BACKENDS` | es/fr/it/pt/hi on the same URL and model with voices `ef_dora`, `ff_siwis`, `if_sara`, `pf_dora`, `hf_alpha` | hub |
| `TTS_MIN_CHARS`, `TTS_TIMEOUT` | `400`, `600s` | hub |
| `HOST_NAME` | the Mac's `LocalHostName`, lowercased | host, hub default target |
| `HOST_DATA_DIR`, `MEDIA_DIR`, `SPOOL_DIR`, `HOOK_SOCKET` | `<state>/host`, `<state>/host/media`, `<state>/spool`, `<state>/host/hook.sock` | host, hook client |
| `VAULT_DIR` | `~/hommy-vault` (chosen in setup) | host, hub default target |
| `AGENT_BRIEFS_DIR` | `<share>/briefs` | host |
| `NEW_SESSION_ROOTS`, `NEW_SESSION_PINNED` | `~/Projects,~/src`, `$VAULT_DIR` | host |
| `CLAUDE_BIN` | absolute path of `claude` found at setup (`--claude-bin`) | host, doctor |
| `AGENT_PERMISSIONS` | `skip` (`ask`) | host |
| `AGENT_COMMAND` | `$CLAUDE_BIN` (absolute) | host: agterm runs `/bin/zsh -l -c 'env HOME=<the host's HOME> [CLAUDE_CONFIG_DIR=<the host's, when set>] <AGENT_COMMAND> [--dangerously-skip-permissions]; exec /bin/zsh -l'` (source shape; the flag per `AGENT_PERMISSIONS`). agterm starts sessions with its own environment, so the host passes its `HOME` on: the same for a user, the scratch HOME for a test instance. Setup's own agterm sessions (§4.6 steps 1, 11) do the same. The host logs its effective command line at start |
| `AGTERM_WORKSPACE` | `hommy` | host |
| `CLAUDE_TIMEOUT`, `HOST_PROGRESS_EVERY`, `CLAUDE_KILL_GRACE` | `30m`, `500ms`, `5s` | host |
| `SPEECH` | `local` (`external`, `off`) | setup, service, doctor |
| `SPEECH_LISTEN` | `127.0.0.1:$PORT_BASE+4` | speech service |
| `WHISPER_MODEL` | `large-v3-turbo` (`small`, `medium`) → `<share>/models/ggml-<m>.bin` | speech service |
| `HB_HOME` | per §3.1 | setup, doctor |

`TTS_LANG_BACKENDS` keeps the source format `lang|url|model|voice`; the hub now also sends `lang_code` (the block's
`lang`, default `en`) in every `/v1/audio/speech` request, which mlx-audio's Kokoro needs and other OpenAI-compatible
servers ignore. Japanese, Chinese (extra G2P packages) and Russian (another engine) are optional extras added by
`TTS_LANG_BACKENDS` lines (README › Voices), not installed by default (§14 D-7).

**Claude environment for runs:** the host's `run_p` child keeps the source's clean environment but **keeps auth
variables** (`CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`, `CLAUDE_CONFIG_DIR`) and drops only session-identity ones
(`CLAUDECODE`, `CLAUDE_CODE_ENTRYPOINT`, `CLAUDE_CODE_SESSION_*`, `CLAUDE_CODE_BRIDGE_SESSION_ID`, …); the source drops
every `CLAUDE_CODE_*`, which would sign out a token-authenticated user.

### 3.3 Ports

| Port | Default instance | Test slot `N` (base `B = APP_PORT_BASE + SLOT_PORT_STEP·N`) |
|---|---|---|
| hub host API (`/ws`, `/healthz`, `/api/*`) | 8470 | `B` |
| Mini App (opt-in) | 8473 | `B+3` |
| speech gateway (llama-swap) | 8474 | `B+4` |
| speech backends (llama-swap `startPort`) | `PORT_BASE+10000` = 18470… | `B+10000` (14710… for slot 0) |
| hommy-browsers MCP / Chrome debug | 8931… / 9331… (engine defaults) | `B+6`, `B+7` / `B+8`, `B+9` (testing.md §2) |

### 3.4 Secrets (Keychain)

`internal/secret` uses `/usr/bin/security` (no cgo; values go in through `security -i` on stdin, never argv). Generic
passwords, service = the instance's service (§3.1), account = item:

| Item | Holder | Written by |
|---|---|---|
| `telegram-bot-token` | hub; `send` direct fallback | setup (kept when present and valid) |
| `host-key` | host (its own key, ≥ 32 chars) | setup / `--join` |
| `hub-host-keys` | hub (`name key` lines, as the source's keys file) | setup, `hommy hub host-key add` |
| `gemini-api-key` | hub (optional STT fallback) | setup |
| `signing-keychain-password` | `hommy service install` | service install |

Tests run with a scratch `HOME` whose keychain search list holds only a scratch keychain (testing.md §3): a test can
neither read nor write the real login keychain. Workspace ops scripts (not tests) keep the build's own test secrets as
`hommy-test-*` items in the build Mac's Keychain (`ops/accounts.md`).

## 4. Interfaces

### 4.1 Hub ↔ host protocol (as the source has it)

Unchanged wire format (`internal/proto`): one WebSocket `GET /ws` with `Authorization: Bearer <host key>` and
`X-Hommy-Host: <name>` (renamed header). Frames `hello | job | result | event | heartbeat | ack`. Jobs
`run_p | save_file | cancel | type | inbox_append | new_session | peek | files | pause | done` →
`Result{job_id, status: ok|inbox|no_session|error, data, error}`; unknown type → `error`. Events `started | progress |
type_outcome | hook_answer | hook_waiting | hook_resumed | spool`. Heartbeat every 10 s (sessions, composer states,
`hooks`, brief folders, and new: `checks` = `{"vault":"ok|missing|denied","agterm":"ok|down"}`), read timeout 35 s.
Delivery, lanes, at-most-once `run_p`, stop/cancel, drafts, routing rules, inbox, typing, handover: as the source's
README describes (copied into `docs/protocol.md` by phase 02, de-personalised).

Changes in hommy: the typed prefix is **`[Telegram · tg-<msgid>[ · re "<quote>"]] <text>`** (no owner name; the relay
regex matches only this form); brief folders under `AGENT_BRIEFS_DIR`; `state.json` `created_by: "hommy"`; the
fallback inbox `<state>/inbox/`; the agterm workspace from config; new sessions run `AGENT_COMMAND`; the owner is read
live from the hub DB everywhere the source used its fixed allowed user id (routing, notices, pings, Mini App auth).

### 4.2 Hub HTTP (on `HUB_LISTEN`)

| Endpoint | Auth | Answer |
|---|---|---|
| `GET /healthz` | none | `200 {"ok":true,"version":…}` when the DB answers and the Bot API was reached in the last 3 min; else `503` |
| `GET /api/status` | host key | `{"version","telegram":"ok|stale|unauthorized","paired":bool,"owner":{"first_name"},"hosts":[{"name","online","last_seen","version","checks"}]}` |
| `POST /api/pairing` | host key, loopback only | `{}` → new code `{"code":"ABCD-EFGH","expires_at","bot":"@name","link":"https://t.me/<bot>?start=<code>"}` (replaces a pending one); `{"reset":true}` also clears the owner; `{"owner_id":N}` binds directly (operator path, no code) |
| `GET /api/pairing` | host key | `{"paired":bool,"pending":bool,"expires_at"}` |
| `POST /api/send`, `POST /api/ack` | host key | as the source (`hommy send`, `inbox ack`) |
| `GET /ws` | host key | §4.1 |

### 4.3 Pairing

- Code: 8 Crockford-base32 characters shown `XXXX-XXXX` (40 bits), accepted case-insensitively with or without the
  dash; stored as a SHA-256 hash with `expires_at` (`PAIRING_TTL`); single use.
- Unpaired hub: a private-chat message `/start <code>` from any user with the pending code → that user becomes the owner
  (`owner(user_id, chat_id, first_name, paired_at)`, at most one row), the code is consumed, the bot answers
  `✅ Paired with <bot>. Send a text or a voice note.` plus the help text and the reply keyboard, and sets its
  commands. A wrong or expired code, a group chat or any other update → dropped without a reply, one hub log line
  `dropped update <id>: <reason>` (user id only). **Per user**, 5 wrong codes make the hub ignore that user's `/start`
  until a new code is made, so nobody can burn the owner's code. An unpaired hub sends nothing to anyone (no notices,
  no menu button).
- Paired hub: only the owner's private chat (source behaviour, owner from the DB). A `/start <code>` from anyone else
  is dropped. Re-pairing: `hommy pair --reset` at the Mac.
- After pairing `hommy pair`/`setup` write `OWNER_CHAT_ID` into the config for the `send` direct fallback.

### 4.4 CLI

Global flags on every subcommand: `--instance NAME`, `--config FILE`. Exit codes: 0 ok, 1 failed check / not ready,
2 usage, 3 refused (state the caller must change first).

| Command | Does |
|---|---|
| `hommy setup [flags]` | §4.6 |
| `hommy pair [--reset] [--owner-id N] [--status] [--no-wait]` | new code via the hub, prints it and the `t.me` link, waits until paired (Ctrl-C leaves it pending); `--status` prints the state |
| `hommy doctor [--deep]` | §4.7 |
| `hommy hub`, `hommy host` | the daemons (launchd runs them; foreground for tests) |
| `hommy service install\|uninstall [--purge]\|start\|stop\|restart\|status [hub\|host\|speech\|all]` | §4.8 |
| `hommy config get\|set\|unset\|list [KEY] [VALUE]`; `hommy config permissions ask\|skip`; `hommy config path` | edits the config file (validates the value; restarts the affected service) |
| `hommy hub host-key add NAME` | a new host's key into `hub-host-keys`; prints the key once for `setup --join` and the `HUB_LISTEN` reminder |
| `hommy vault init PATH` / `hommy vault add-recipes PATH` | writes the embedded template into an empty folder (exit 3 if not empty) / copies the example recipes and missing template files into an existing vault without touching existing files, prints the routing rows to add |
| `hommy send …`, `hommy inbox list\|ack\|pending\|wait` | as the source (`agentcli`), config from the instance |
| `hommy hook session-start\|user-prompt-submit\|stop\|notify` | §4.5 |
| `hommy compose-state`, `hommy agterm-job` | dev path, as the source |
| `hommy version` | version string |

### 4.5 Hook contract

- Claude Code runs the plugin's hooks (§5); each calls `${CLAUDE_PLUGIN_ROOT}/bin/hommy-hook <event>`, a POSIX `sh`
  shim: instance from `CLAUDE_PLUGIN_OPTION_INSTANCE` (plugin `userConfig`) else `HOMMY_INSTANCE`; binary
  `~/.local/share/hommy[-X]/bin/hommy`, else `hommy` on `PATH`, else exit 0; runs `hommy [--instance X] hook <event>`
  with stdin passed through, stdout/stderr to `/dev/null`, always exit 0.
- `hommy hook <event>` keeps the source contract: never prints, exit 0, stdin and post give up after 2.5 s, watchdog
  exit at 4 s; local turns stay local (only a prompt starting `[Telegram · tg-` opens a Telegram turn); host down →
  a Telegram turn's requests are spooled (`<state>/host/hookspool/`); one line per run in `<state>/host/hook.log`.
- Events: `session-start` (SessionStart), `user-prompt-submit` (UserPromptSubmit), `stop` (Stop: `last_assistant_message`,
  fallback the transcript), `notify` (Notification except `idle_prompt`/`auth_success`; the source's Claude mapping).
- `<tts lang="…">…</tts>` in an answer adds a voice note after the text (source behaviour).

### 4.6 `hommy setup`

Interactive by default; every prompt has a flag, and `--non-interactive` fails (exit 2) on a missing required value.
Re-run = keep every existing value unless a flag changes it (idempotent). Steps:

1. Preflight: macOS ≥ 14 on arm64; agterm running (`open -a agterm` when it is not, then wait ≤ 60 s for its socket);
   `claude` found (`--claude-bin`), signed in (`claude auth status`) and past first-run onboarding (`~/.claude.json`
   `hasCompletedOnboarding`), else setup opens `claude` in an agterm session for the user to sign in and finish, and
   waits for both.
2. Instance dirs and config (`--instance`, `--port-base`, `--host-name`).
3. Bot token: kept when the Keychain item exists and `getMe` accepts it; else asked (`--token-stdin`), validated with
   `getMe` against `TELEGRAM_API_URL` (`--telegram-api`) and stored. Prints the bot's `@username`.
4. Vault (`--vault PATH`, default `~/hommy-vault`): missing or empty → `hommy vault init`; an existing vault is kept and
   `add-recipes` runs. A path under `~/Documents`, `~/Desktop`, `~/Downloads` or iCloud prints the TCC note. (Phase 02
   builds the folder choice, creation and the note; 05 adds `init` / `add-recipes`.)
5. Permissions (`--permissions skip|ask`, default skip). With skip, `skipDangerousModePermissionPrompt: true` is merged
   into the Claude user settings (`$CLAUDE_CONFIG_DIR` or `~/.claude`, `settings.json`; a user-scope-only key; backup
   beside the file when it changes).
6. Speech (`--speech local|external|off`, `--stt-url`, `--tts-url`, `--whisper-model`): `local` writes
   `<config>/llama-swap.yaml` (§8.2), downloads the whisper model if missing (pinned SHA-256), warms both models;
   `external` writes the URLs; `off` sets `TTS_URL=off` and an empty `WHISPER_ENDPOINTS` (voice notes answered
   "voice is off").
7. Keys: a new random host key into `host-key` and `hub-host-keys` (single Mac) when missing.
8. Services: `hommy service install all` (§4.8).
9. Browsers and plugins (`--plugin-source PATH|owner/repo`, default the public repo): hommy-browsers configured for the
   instance (§3.1) at `HB_HOME` (installed by `install.sh` stage 7); the marketplaces and `hommy@hommy`
   (`--config instance=X`), `hommy-browsers@hommy-browsers` (ports from §3.1), `retainer@retainer` installed and enabled.
10. Pairing: `hommy pair` (skipped with `--no-pair-wait`; resumable with `hommy pair`).
11. Context (`--seed-context`, default asked): opens an agterm session in the vault whose first message asks the agent
    to run the retainer interview into `context/` and `active/Dashboard.md` without rewriting `AGENTS.md` (`--dry-run`
    prints the command and message only).
12. `hommy doctor`; prints `bot @<name> · vault <path> · owner <first name|not paired> · services <list>` and what to
    send first.

`hommy setup --join WS_URL --name NAME --key-stdin` is the host-only form for another Mac: steps 1, 2, 4, 5, 7 (the
given key), 8 (host only), 9; no token, no hub, no pairing.

### 4.7 `hommy doctor`

One line per check, `✓ <id> <detail>` or `✗ <id> <detail>` followed by `  fix: <command or action>`; `!` marks an
optional piece that is absent (never red). Exit 0 when nothing is red, 1 otherwise. `--deep` adds live speech round
trips. Phase = where the check lands.

| id | Checks | fix line | Phase |
|---|---|---|---|
| `platform` | macOS ≥ 14, arm64 | `hommy needs macOS 14+ on Apple Silicon` | 02 |
| `agterm` | `agtermctl version` answers | `brew install --cask umputun/apps/agterm` / `open -a agterm` | 02 |
| `claude` | `CLAUDE_BIN` runs `--version` | `curl -fsSL https://claude.ai/install.sh \| bash` | 02 |
| `claude-auth` | `claude auth status` exit 0 and onboarding complete | `claude auth login`, then run `claude` once | 02 |
| `config` | file present, 0600, every key valid | `hommy setup` | 02 |
| `secrets` | the Keychain items the role needs exist | `hommy setup` | 02 |
| `telegram` | `getMe` ok with the stored token | `token rejected: make a new one with @BotFather, then hommy setup --token-stdin` | 02 |
| `binary` | the signed copy exists, its version equals this `hommy`, its designated requirement is not cdhash-pinned | `hommy service install` | 02 |
| `services` | each label loaded and running (`launchctl print gui/<uid>/<label>`) | `hommy service install` / `hommy service restart <name>` | 02 |
| `hub` | `/healthz` 200 | `hommy service restart hub; tail <state>/hub/err.log` | 02 |
| `paired` | `/api/pairing` paired | `hommy pair` | 02 |
| `host` | this host online in `/api/status`, heartbeat < 30 s | `hommy service restart host` | 02 |
| `vault` | host check `vault: ok` (exists, readable by the host process, has `CLAUDE.md`) | `missing`: `hommy setup --vault PATH`; `denied`: `System Settings › Privacy & Security › Full Disk Access › add <share>/bin/hommy` | 02 |
| `hooks` | the relay socket accepts a connection | `hommy service restart host` | 02 |
| `speech` | the STT and TTS endpoints' gateway `/health` (or `SPEECH=off`) | `local`: `hommy service restart speech`; else `check WHISPER_ENDPOINTS / TTS_URL` | 02 |
| `speech-stack` | `whisper-server`, `ffmpeg`, `llama-swap`, `mlx_audio.server` on PATH, model file + SHA-256 | `install.sh` (re-run) / the missing tool's install line | 05 |
| `speech-deep` (`--deep`) | transcribes the embedded 1 s sample; synthesizes "ok" → Ogg Opus | `hommy service restart speech; tail <state>/speech/err.log` | 05 |
| `plugin` | `claude plugin list --json`: `hommy@hommy` enabled with this instance | `claude plugin install hommy@hommy --config instance=<X>` / `claude plugin enable hommy@hommy` | 05 |
| `browsers` | Google Chrome present, `HB_HOME` present, the lanes MCP answers `initialize` | `brew install --cask google-chrome` / `$HB_HOME/install.sh` | 05 |
| `retainer` | `retainer@retainer` installed | `claude plugin install retainer@retainer` | 05 |
| `whatsapp`, `telegram-user` | `wacli` / `telegram-agent-cli` on PATH (optional, `!` when absent) | `install.sh --with-whatsapp` / `--with-telegram-user` | 05 |

### 4.8 Services, stable binary, signing

`hommy service install`: (1) copy the running binary to `<share>/bin/hommy` when its SHA-256 differs (atomic rename);
(2) make sure the signing identity exists: a self-signed code-signing certificate (CN `hommy local signing`, EKU
codeSigning) imported into the instance's own signing keychain, whose random password is Keychain item
`signing-keychain-password`; the keychain is unlocked, its key partition list set for `codesign`
(`security set-key-partition-list -S apple-tool:,apple:`) so no dialog appears, and it is added to the user keychain
search list only for the `codesign` call and restored after (the source's pattern). Sign the copy with identifier
`dev.hommy`; verify the designated requirement names the identifier and the certificate leaf, not a cdhash (fail loudly
otherwise); (3) write `~/Library/LaunchAgents/<label>.plist` for hub, host and (with `SPEECH=local`) speech:
`ProgramArguments` = the signed copy + `hub|host --instance X` (speech: `llama-swap --config <config>/llama-swap.yaml
--listen $SPEECH_LISTEN --watch-config`), `RunAtLoad`, `KeepAlive`, `ThrottleInterval 10`, logs
`<state>/<name>/{out,err}.log`, `EnvironmentVariables` `PATH=/opt/homebrew/bin:/usr/bin:/bin:<HOME>/.local/bin`
(absolute) and `HOME`; (4) `launchctl bootout` + `bootstrap gui/<uid>` only for plists whose bytes changed, else nothing
(I5). `uninstall` boots out and removes the plists; `--purge` also removes exactly the files and directories hommy
created (`hommy.env`, `llama-swap.yaml`, the state and share dirs, the signing keychain, its Keychain items) and never a
directory's other contents. The stable path keeps TCC grants (Full Disk Access for a vault in a protected folder; Local
Network for a joined host) across upgrades; with the default vault outside protected folders and everything on
loopback, no grant is needed.

### 4.9 Telegram commands

`/start` (pairing, then help), `/new_session` (fresh `-p` session for a vault/`p@` target; for a session target it
switches back to `DEFAULT_TARGET`, so the next message starts a new session), `/status`, `/voice`, `/target`, and the
source's session commands (`/spawn`, `/peek`, `/files`, `/pause`, `/done`).

## 5. Claude Code plugins

- **`hommy`** (this repo): `.claude-plugin/marketplace.json` (marketplace `hommy`, plugin entry `source: "./plugin"`);
  `plugin/.claude-plugin/plugin.json` (`name: hommy`, MIT, `userConfig.instance`: string, not sensitive, default
  empty); `plugin/hooks/hooks.json`: SessionStart, UserPromptSubmit, Stop, Notification → `bin/hommy-hook`; agterm
  status (UserPromptSubmit/PostToolUse `active`, Notification `permission_prompt` and PermissionRequest `blocked`,
  SessionEnd `idle`) → `bin/agterm-status` (agterm's bundled status script when present, else nothing); PreToolUse
  guards: `bin/wacli-guard` and `telegram-agent-cli hook claude-pretooluse` (each a no-op when its tool is not on
  PATH). Skills: `notify` (`hommy send`), `whatsapp`, `telegram`, `shopping-research`, de-personalised from the source
  (`docs/spec-private.md` › `SRC_SKILLS`). No plugin secret exists: the only `userConfig` value is the instance (§14 D-5).
- **`hommy-browsers`** (its own repo, its own marketplace at the repo root, design D8): the `agent-browsers` skill,
  the identity-lease hooks, and `.mcp.json` registering `pw-lane`, `pw-lane-a…d`, `identity`, `identity-a…d` over HTTP
  at `http://127.0.0.1:${user_config.lanes_port}/mcp` etc. Installed from the local engine checkout (`HB_HOME`), so
  plugin and engine versions match.
- **`retainer`**: from its own public marketplace (`AlexDobrushskiy/retainer`).

## 6. Vault template (`vault-template/`)

Retainer-compatible shape (the retainer interview fills it, design D4): `AGENTS.md` (kernel: structure, routing table,
passive learning, index/log/decisions rules, inbox flow, and hommy's conventions: Telegram turns and the typed prefix,
`<tts>` voice replies, brief folders, `hommy send` etiquette, confirm before anything that commits, close what you
open, no screenshots in the vault), `CLAUDE.md` = `@AGENTS.md`, `index.md`, `log.md` (header with entry format and the
"would I want this in 6 months?" bar), `context/{bio,goals,preferences,places}.md` (each `## TL;DR` + `⚠️ to capture`),
`active/Dashboard.md`, `decisions/README.md`, `inbox/README.md`, `archive/README.md`, `_templates/capture-note.md`,
`prompt/voice-replies.md`, and three recipes routed from `AGENTS.md`: `prompt/book-local-service.md` (business from
`context/places.md` → booking page in a lane → fill → **ask the owner with buttons before the final submit** → submit
on yes → `log.md` + answer with `<tts>`), `prompt/email-triage.md` (read-only summary through the user's mail
connector; never sends without OK), `prompt/find-and-buy.md` (the de-personalised shopping research; stops before
payment). No folder is empty.

## 7. hommy-browsers (phase 03)

The generic engine from the source (`SRC_BROWSERS`), clean first commit, no `sites/`: `serve` (Playwright MCP servers),
`agent-chrome`, `pw-identity` (leases, pool, cookie sync), `plain-chrome`, `captcha/` (solver; models opt-in),
`browser-reaper`, `creds` (Keychain only; login identity from config), launchd templates, the plugin (§5). Config
`<HB_HOME>/config.env`: `HB_PORT_BASE` (8931: lanes, +1 identity, +2… pool copies), `HB_DEBUG_BASE` (9331, same
layout), `HB_POOL` (0–4, default 4), `HB_LABEL_PREFIX` (`dev.hommy.browsers`), `HB_BUNDLE_ID`
(`dev.hommy.agent-chrome`), `HB_SITES_DIR` (empty), `HB_LOGIN_EMAIL` (empty), `HB_NODE`, `HB_STATE_DIR`
(`~/.local/state/hommy-browsers`, all runtime state under it). `install.sh [--with-captcha] [--no-launchd]
[--uninstall]`: idempotent, restarts a server only when its plist changed, fails on a failed smoke test. Requires
Google Chrome (`install.sh` stage 3 installs the cask).

## 8. Installer (`install.sh`)

### 8.1 Stages

`curl -fsSL https://raw.githubusercontent.com/AlexDobrushskiy/hommy/main/install.sh | bash` (public from phase 07).
Before that, and in tests, `install.sh --source DIR` installs from local artifacts (a GoReleaser snapshot `dist/`, a
`hommy-browsers` tarball, the repo for the plugin marketplace). Flags: `--instance`, `--port-base`, `--with-whatsapp`,
`--with-telegram-user`, `--with-captcha`, `--whisper-model`, plus every `hommy setup` flag, passed through; for the
build Mac only: `--brew-check-only` (stages 2–3 verify each entry with `command -v` or its `.app`, never calling
brew) and `--hommy-binary PATH` (stage 6 without
brew). Each stage prints `[n/9] <stage>: present|installed|upgraded|changed|skipped (<detail>)` and appends
`stage, command, source=<origin>, exit` lines to `<state>/install.log` (no secrets); the summary prints the change count.

| # | Stage | Source |
|---|---|---|
| 1 | Preflight: macOS ≥ 14, arm64, admin user, ≥ 10 GB free | — |
| 2 | Homebrew if missing | the official `install.sh` from `Homebrew/install` (interactive sudo) |
| 3 | `brew bundle --file Brewfile --no-upgrade`: `ffmpeg`, `whisper-cpp` (core), `llama-swap` (`mostlygeek/llama-swap` tap), `uv`, `node`, `git`, casks `umputun/apps/agterm`, `google-chrome` | Homebrew |
| 4 | mlx-audio: `uv tool install "mlx-audio[server]" --with "misaki[en]" --with <spaCy en_core_web_sm wheel URL>` | PyPI / the spaCy release |
| 5 | Claude Code only when `claude` is missing | `curl -fsSL https://claude.ai/install.sh \| bash` |
| 6 | hommy: `brew install --cask AlexDobrushskiy/tap/hommy` (from 07); before: a local tap generated from `dist/` | the tap / local |
| 7 | hommy-browsers: clone or `git pull --ff-only` into `HB_HOME`, write its `config.env` for the instance (§3.1), run its `install.sh` | `AlexDobrushskiy/hommy-browsers` |
| 8 | optional: `brew install openclaw/tap/wacli shfmt`; `brew install --cask AlexDobrushskiy/tap/telegram-agent-cli` | their own taps |
| 9 | `hommy setup` (vault, browsers config, plugins incl. retainer, speech, services, pairing, doctor) | — |

Re-run = upgrade: hommy (`brew upgrade --cask hommy`, then `hommy service install`), hommy-browsers (`git pull` + its
install), the plugins (`claude plugin update`). Third-party tools are installed when missing and never upgraded by us
(`--no-upgrade`), so a user's own pins survive.

### 8.2 Speech stack (`SPEECH=local`)

`<config>/llama-swap.yaml`: `startPort` per §3.3, models `whisper` (`whisper-server --model <file> --host 127.0.0.1
--port ${PORT} --convert --inference-path /v1/audio/transcriptions --language auto`, `checkEndpoint: /health`,
`ttl: 300`) and `kokoro` (`mlx_audio.server --host 127.0.0.1 --port ${PORT} --log-dir <state>/speech`,
`checkEndpoint: /`, `ttl: 600`), one group with `swap: false`. The launchd `PATH` includes Homebrew (ffmpeg for
`--convert` and for Ogg Opus output). The whisper model downloads from `huggingface.co/ggerganov/whisper.cpp` with a
pinned SHA-256 per size; Kokoro weights download on first use (warmed by setup).

### 8.3 Release

GoReleaser v2: darwin/arm64 build, `homebrew_casks` (binary `hommy`, the quarantine-strip `install_steps`,
`commit_author` = the noreply identity), archive with `LICENSE` and `README.md`; `skip_upload: true` until the public
go (S4), then the go step runs GoReleaser on the build Mac for `v0.1.0`. No Developer ID (design): `hommy service
install` re-signs the stable copy with the local identity (§4.8).

## 9. Leak gate (S1–S3, S6, S8)

- **Scanner** `scripts/leak/leak_scan.py` (Python 3 stdlib), wrapper `scripts/leak-scan.sh`, in `hommy` and (copied
  byte-for-byte) in `hommy-browsers`. Modes: `[<paths>]` (git work trees: tracked + untracked-not-ignored),
  `--staged`, `--push <local> <remote>` (every blob, path, commit header and message the push sends), `--history
  [<repo or mirror>]` (everything reachable from every ref the remote serves: branches, tags, `refs/pull/*/head`),
  `--self-test`. Exit 0 clean, 1 hit, 2 no identifiers list where one is required.
- **Output** only `path:line: leak #N (category)`, `commit <sha12>: message: leak #N`, `commit <sha12>: <author|committer>
  email not allowed`; never a matched text. A hit **in a path name** prints `blob <sha12>: path name: leak #N`, never
  the path.
- **Identifiers** (S1): `$LEAK_IDENTIFIERS` (`ops/pipeline.env`; default `~/.config/hommy-build/leak-identifiers.txt`, outside every
  instance's config dir), lines `<category>\t<identifier>`, lowercase, `#` comments; line number =
  `#N`. Matching: text and identifiers NFC-normalised and case-folded; a hit needs a **boundary** (the characters before
  and after are not Unicode letters or digits). Text sources: every UTF-8 file; XML inside `.docx/.xlsx/.odt` with tags
  removed and runs joined; `.zip` members; `.gz` decompressed; any other binary through its printable runs (≥ 4 chars,
  like `strings`), which covers SQLite and image metadata. Images and PDFs additionally need an entry (path + SHA-256,
  reviewed by the implementer) in `scripts/leak/images.txt`; an unlisted one is a hit. Category `phone`: every
  phone-like run (`[+(]?\d[\d\s().-]{6,}\d`) is reduced to digits and hits when it contains the identifier's digits, or
  their last 9 when the identifier has ≥ 10.
- **Canary:** a synthetic canary pair (category `canary`: a word and a phone) is in the list for the gate's end-to-end
  rows; tests build those strings at run time and the repo never holds them contiguous. `--self-test` instead builds
  its own temporary list, so it runs anywhere (contributor clones, fork CI).
- **Required or not:** a clone with `git config hommy.leak.required true` (the build's clones; CI sets the env) fails
  closed without the list; elsewhere the identifier scan is skipped with a notice and gitleaks + identities still run.
- **Secrets:** `gitleaks` (pinned, from `scripts/tools.sh`; `protect --staged` at pre-commit, `detect --log-opts
  <range>` at pre-push and CI) with an allowlist for the test fixtures' synthetic tokens.
- **Commit identities** (S2): author and committer emails of every pushed commit must be in `.github/commit-identities`
  (the owner's GitHub noreply address and `noreply@github.com`). Clones set `user.email` to the noreply address; the
  ship step merges with `gh pr merge --squash --author-email <noreply>`; the `identities` job also runs on every push to
  `main`, so a squash commit with another author turns `main` red at once; GoReleaser's cask commits use the noreply
  identity. The tap is shared and already public: its identities are checked only for commits after the HEAD recorded
  when phase 07 starts.
- **Where it runs:** `.githooks/pre-commit` (staged blobs, paths + gitleaks) and `.githooks/pre-push` (blobs, paths,
  headers, messages; refuses `main`) in both repos; the lane commit gate `ops/leak-gate-hook.py` and the ship step
  through `ops/leak-scan.sh`; CI jobs `leak` and `identities` on every PR and push. **The judge is never the diff under
  judgement:** the workspace wrapper and CI both run the scanner, `.github/commit-identities` and the gitleaks config
  **as merged on `origin/main`** (fallback: the working tree until phase 01 merges them). CI reads the identifiers from
  the Actions secret `LEAK_IDENTIFIERS` (S6; a fork PR without secrets runs gitleaks + identities and says the
  identifier scan was skipped; a push event without the secret fails). Phase 07 runs `--history` on mirror clones of
  `hommy`, `hommy-browsers` and the tap, scans PR titles, bodies and comments, release notes, tag and branch names
  through `gh api`, and purges Actions run logs (S8). **Not** on this workspace (S3: `WS_LEAK_GATE=0`).
- **False positive:** the overseer narrows or removes the identifier (`python3 ops/leak_patterns.py allow N` comments
  line N out with a date and re-syncs both repos' Actions secrets); no allowlist of hashes exists.

## 10. Testing strategy (detail: [`../testing.md`](../testing.md))

- **E2E first:** every phase's rows run the real `hommy` binary under real launchd agents with test labels, in a
  scratch `HOME` (`/tmp/hm-s<N>/home`, outside every `CLAUDE.md` ancestry) with its own keychain, against the **fake
  Telegram Bot API** (default) or the **real test bot** (rows tagged `tg=real`, driven from the maintainer's account
  through `telegram-agent-cli e2e`, authorized), the **fake speech server** (default) or the instance's **own speech
  stack** (`speech=real`, 05 on), the **`claude` stub** (default) or **real Claude Code** (`claude=real`, few: scratch
  `HOME` + the long-lived test token).
- **`scripts/sx.sh`** (scratch exec) runs every test command except the Telegram driver: `env -i` plus an allowlist
  (`USER`, `LOGNAME`, `SHELL`, `TERM`, `LANG`, `LC_*`, `TMPDIR`, `HOMMY_*`), `HOME` = the slot's scratch home, a fixed
  `PATH` (the scratch `bin/` with links to the real `claude`, `uv`, `node`, `npm`, `git`; `/opt/homebrew/bin`; system),
  `HOMEBREW_NO_AUTO_UPDATE=1`; `--agterm-session ID` keeps `AGTERM_SESSION_ID`. Nothing of the worker's Claude
  session (`CLAUDE_CONFIG_DIR`, `CLAUDECODE`, `CLAUDE_CODE_*`, `ANTHROPIC_*`, wrapper variables) passes. Rows write
  `SX <cmd>`. `scripts/tg.sh` runs outside it (its `TG=real` mode is the maintainer's `telegram-agent-cli`). Go tests
  that exercise setup, doctor, service or plugin code set `HOME` to `t.TempDir()` and clear `CLAUDE_CONFIG_DIR`
  themselves, and `ci-preflight.sh` runs `go test` through `sx`.
- **CI** (`.github/workflows/ci.yml`): `go` (gofmt, vet, `go test ./...` incl. hub↔host integration on the fakes and
  the stub), `shellcheck`, `leak`, `identities`. No agterm, no Claude. Runner per S7 (`vars.CREW_RUNS_ON` while
  private, GitHub-hosted `macos-latest` after phase 07 removes it); the workflow names no host.
- **VM:** Tart, from a pinned release archive (`scripts/tools.sh`; nothing installed into the build Mac's Homebrew),
  image `ghcr.io/cirruslabs/macos-golden-gate-vanilla:latest` for the fresh-Mac rows of 05 and 06 and 07's post-go
  proof; ≥ 60 GB free before a pull; every VM deleted at the end of its phase, the cached image by 07.
- **Never the live setup** (I2): `scripts/live-guard.sh snapshot|check` around every phase's run.

## 11. Milestones

| M | Reached when | Means |
|---|---|---|
| M1 | 01 ships | every later commit is gated; the harness runs |
| M2 | 02 ships | hommy works on one Mac: pair, text, voice, agterm session, answer with voice |
| M3 | 04 ships | the kit's pieces exist: browsers engine, plugin, vault template, booking recipe proven |
| M4 | 05 ships | one command installs and wires it all on a fresh Mac (VM) |
| M5 | 06 ships | clean-room voice errand proven, over-engineering pass done |
| M6 | 07's go step runs | public, `v0.1.0`, the public one-liner proven (after Alex's OK) |

## 12. Phases

| # | slug | goal | depends | slot | milestone | exit criteria | agent wall-clock (impl + review + e2e + ship) |
|---|---|---|---|---|---|---|---|
| 01 | `leak-gate` | leak gate wherever it runs, repo skeleton + CI, the test harness (fakes, stub, `sx`, test env, workslots, live guard, pinned tools) | — | 0 | M1 | 01-E1…E14; CI green | ~2 h |
| 02 | `core` | the bridge copied, renamed, decoupled; instance, config, Keychain, pairing, single-Mac, setup, doctor, services, signing | 01 | 0 | M2 | 02-E1…E17 | ~3.5 h |
| 03 | `hommy-browsers` | new repo with the generic engine, config-driven, its plugin and installer; the private source untouched | 01 | own repo; ports of slot 1 | — | 03-E1…E10 | ~2 h (parallel with 02) |
| 04 | `plugin-vault` | hommy plugin + marketplace, vault template + recipes, booking fixture; the recipe proven with a real agent | 01, 03 | 2 | M3 | 04-E1…E7 | ~1.5 h (parallel with 02) |
| 05 | `installer` | `install.sh`, Brewfile, cask, speech stack, upgrades; setup's browsers/plugin/vault/context steps and doctor checks; proven in a scratch HOME and a fresh VM | 02, 03, 04 | 0 | M4 | 05-E1…E12 | ~3 h |
| 06 | `clean-room` | one command → setup → real voice note → real errand in a fresh VM, then re-run, extras, upgrade in that VM; over-engineering review | 05 | 0 | M5 | 06-E1…E10 | ~2 h |
| 07 | `go-public` | README, full-history scans, release prep, `ops/go-public.sh`; the public switch after Alex's OK | 05 (prep runs parallel with 06; the go step requires 06 shipped) | 3 | M6 | 07-E1…E4 pre-go; P1–P2 after the go | ~1 h before the go |

Slot 0 is the phase checkout `app/`; slots 2 and 3 are workslots of the same repo (`scripts/slot.sh claim`, 01) whose
lanes ship by PR like any lane (the overseer adapts the flow's ship step for a slot: no `git switch main` in a
worktree; the phase tag goes on `gh pr view --json mergeCommit`). Critical path 01 → (02 ‖ 03 → 04) → 05 → 06 → go:
about **10 h** of agent wall-clock (each phase can use its 4 h implement timebox plus review, a blind e2e and ship)
against the approved 5–6 h: **the overseer takes this number to Alex** before building. The Tart image pull (≈ 30 GB)
runs as an overseer side task during 01, so 05 never waits on it.

## 13. Traceability

| Requirement | Proved by |
|---|---|
| D1 whole kit, standalone; Alex not switched | 05-E7 (one install brings transport, plugins, vault, browsers); I2 guard in every phase's rows (02-E16, 03-E10, 04-E7, 05 build-Mac rows, 06-E10) |
| D2 transport stays the bridge (renamed) | 02-E3…E7 (source behaviour under the new name) |
| D3 one monorepo | 04-E2 (marketplace at the root), 05-E6 (the cask from this repo) |
| D4 generic skills + 3 recipes | 04-E2 (skills listed), 04-E4 (recipes routed), 04-E7 (leak-clean), 04-E6 (barber recipe works) |
| D5 permissions default skip; `config permissions`; no `cld` | 02-E1 (`AGENT_PERMISSIONS` = `skip` after setup), 02-E5 (the real session runs with the flag), 02-E11; 02-E9 (no `cld`) |
| D6 name, MIT, CLI names, tap | 01-E12 (LICENSE), 02-E1 (`hommy` subcommands), 07-P1 (tap cask) |
| D7 agterm only, `Ctl` unchanged | 02-E5 (sessions through agterm) |
| D8 hommy-browsers new repo, clean first commit, private repo untouched | 03-E1, 03-E2, 03-E10 |
| Claude Code never bundled | 05-E8, 05-E7 (official installer in the VM) |
| Every tool from its own source | 05-E8 (install log names each source) |
| Single-Mac default; more hosts run `hommy host` | 02-E1 (loopback), 02-E15 (a second host over loopback; the LAN listen is README-only, §14 D-17) |
| Mini App off by default, opt-in | 02-E14 |
| Speech: whisper behind llama-swap, kokoro, `lang_code`; extras optional; Gemini used only with a key | 05-E3, 05-E4, 02-E4; 02-E8 (no Gemini key: doctor green, no Gemini call in the fake speech log); §14 D-7 |
| Repos private until the last phase, after a full-history scan and Alex's OK | 07-E2, 07-E3, 07-E4 (the go step waits and checks), 07-P1 |
| Nothing linked to the maintainer's machines | 02-E9, 04-E7, 07-E2 (scans); testing.md (own speech stack, no maintainer service) |
| Signing self-signed, no Developer ID | 05-E5, 02-E1 (designated requirement not cdhash) |
| Coupling: Telegram user id → pairing | 02-E2 |
| Coupling: LAN IPs → loopback defaults, tripwire test removed | 02-E9, 02-E1 |
| Coupling: host names / default target | 02-E1 (`HOST_NAME` from the Mac; `DEFAULT_TARGET` new@this host) |
| Coupling: vault, briefs, plist and hook paths | 02-E1, 02-E5 (briefs dir), 02-E9 |
| Coupling: launchd labels, signing identifier, keychain, bundle id → `dev.hommy.*` | 02-E1, 03-E4 |
| Coupling: domains, voices in defaults | 02-E9, 02-E4 (`af_heart`) |
| Coupling: dev/live split → `--instance` | 02-E15 (instances `test` and `test-h2` side by side), 03-E4, 05-E1 |
| Coupling: README intro and Setup rewritten | 02-E9, 07-E1 |
| Plan 01 done-when (planted identifier blocked at commit, push, CI; clean tree passes; CI green) | 01-E2, E3, E4, E1, E8 |
| Plan 02 done-when (pair, text, voice, new session, text + voice reply; doctor green, red with a fix line when speech is down) | 02-E2…E5, 02-E17 (real bot), 02-E8 (fake speech down); the real llama-swap stopped: 05-E4 (§14 D-16) |
| Plan 03 done-when (scratch install, lane + identity, lane flow, clean scan, source untouched) | 03-E3…E6, 03-E2, 03-E1, 03-E10 |
| Plan 04 done-when (plugin loads in scratch HOME; recipe → lane → confirm step, no submit) | 04-E2, 04-E6; hooks through the plugin: 05-E9 |
| Plan 05 done-when (install finishes, second run changes nothing, doctor green) | 05-E1, E2, E7; the VM re-run 06-E6 (a VM replaces the scratch user: §14 D-2) |
| Plan 06 done-when (recorded with timings, fixes in repo, VM deleted) | 06-E1…E10 |
| Plan 07 done-when (fresh VM runs the public one-liner, no GitHub login) | 07-P2 |
| S1 list format and matching | 01-E5 |
| S2 commit metadata (incl. squash merges) | 01-E4 (CI rejects a foreign author); the `identities` job on every push to `main` (01's `ci.yml`) after each ship's `--author-email` merge (flow, overseer); 07-E2 |
| S3 no workspace scan | 01-E7 |
| S4 nothing in the tap before the go | 05-E6 |
| S5 superseded by D8 (the private repo is never renamed) | 03-E1 (source untouched) |
| S6 CI identifiers from a secret | 01-E4 |
| S7 self-hosted runner while private, removed before the switch | 01-E8, 07-E4 (dry run lists it), 07-P1 |
| S8 every served ref scanned, PR/release text, logs purged | 07-E2, 07-E3, 07-P1 |

## 14. Decisions (changes to the plan; calls this spec makes)

| # | Decision | Why |
|---|---|---|
| D-1 | The test harness (fakes, stub, `sx`, test env, real-Claude wrapper, live guard, pinned tools) is built in **01**. | 02 is the largest phase; the harness does not depend on the bridge code, and 03/04 need it while 02 runs. |
| D-2 | Phase 05 proves install in a scratch **HOME** on the build Mac and on a **fresh Tart VM**, not a scratch macOS user. | Creating a user needs an admin password; a never-logged-in user has no GUI session for agterm or `gui/<uid>` launchd; Homebrew's prefix belongs to the build Mac's user. The VM is the real fresh Mac. |
| D-3 | Phase-04 booking target: a **local fixture site** (`hommy-e2e booking-site`, "Barbearia Exemplo") served by the harness. | Deterministic (fixed slots, nothing third-party changes), never hits a third party from tests or CI, and its request log proves "reached the confirm step, never submitted", which a public demo cannot show. |
| D-4 | Vault kernel in `AGENTS.md`, `CLAUDE.md` = `@AGENTS.md` (retainer's shape). | The retainer interview fills the template instead of fighting it; Claude Code reads it the same. |
| D-5 | No plugin secrets: `userConfig` holds only `instance`. | Nothing in the plugin needs a secret; hub/host secrets are the binary's (Keychain). The plan's "secrets through userConfig" has nothing to carry. |
| D-6 | hommy-browsers is its own plugin (skill + lease hooks + MCP registrations) from its own repo, installed from the local engine checkout. | One source for the skill and the engine; versions always match; no cross-repo fetch while repos are private. |
| D-7 | Default TTS languages EN, ES, FR, IT, PT, HI; JA, ZH and RU are documented extras. | JA/ZH need extra G2P packages (hundreds of MB); RU needs another engine. The design calls RU optional; the same rule keeps the default install light. |
| D-8 | Captcha solver models are opt-in (`--with-captcha`). | Several GB (torch, a VLM); the escalation ladder works without the solver rung. |
| D-9 | Not carried over: the legacy single-host bot, `send.py` compatibility and its parity test, the Codex hook installer, the personal bot commands. | One path per user; Codex is outside the design (D7); they exist only for the maintainer's history. |
| D-10 | Default vault `~/hommy-vault`, briefs under the share dir. | Outside TCC-protected folders, so the host needs no grant by default. |
| D-11 | Default target `new@<host>:<vault>:interactive`; `/new_session` on a session target returns to it. | The reference flow starts a session per errand; without the Mini App this is how the next one starts. |
| D-12 | The typed prefix drops the owner's name: `[Telegram · tg-<id>`. | No name in the product; a prefix no other bridge on the same Mac reacts to. |
| D-13 | Real-Claude rows use the long-lived test token in a scratch `HOME` (build plan › rulings), on the build Mac as well as in the VM. | Full isolation from the maintainer's Claude config; the same token the VM uses. |
| D-14 | The plan's old phase 07 (moving the maintainer onto the kit) is gone (design D1); "Go public" is 07; alex-needs shrinks to the public go. | Alex's rulings, 2026-11-17. |
| D-15 | **04 depends on 01 + 03, not 02**: setup's browsers/plugin/vault/context steps, `hommy vault`, the plugin/browsers/retainer doctor checks and the hooks-through-plugin rows move to **05**. **07 depends on 05**: its prep runs while 06 runs; the go step requires 06 shipped. | Takes 04 and 07's prep off the critical path (about 2.5 h) without splitting a phase or dropping a done-when. |
| D-16 | 02's "doctor red when llama-swap is stopped" is proven with the fake speech server in 02 (E8) and with the real llama-swap in 05 (E4). | The instance has no llama-swap until 05; tests never touch the maintainer's. |
| D-17 | Multi-host: `setup --join` and `hub host-key add` are built and proven over loopback (02-E15); the LAN listen, the macOS firewall prompt and the Local Network grant are documented in the README, not tested. | A LAN row would need a second machine or a VM↔build-Mac network path; the protocol is the source's, unchanged. |
| D-18 | Scratch roots live in `/tmp/hm-s<N>/` and every test command runs through `scripts/sx.sh`. | Workers inherit the maintainer's `CLAUDE_CONFIG_DIR` and `CLAUDE_CODE_*`; a scratch root inside the workspace loads its private `CLAUDE.md` and overflows macOS's 104-byte socket paths in workslots. |
| D-19 | Build tools for tests (gitleaks, shellcheck, Tart, GoReleaser) come from pinned release archives with SHA-256 via `scripts/tools.sh` into `.dev/tools/`. | Never modify the build Mac's Homebrew; CI and local use the same versions. |
| D-20 | Sessions the host or setup opens in agterm get the host's `HOME` (and `CLAUDE_CONFIG_DIR` when set) on their command line. | agterm starts sessions with its own environment; without this a test instance's session would run Claude on the maintainer's config, and a user with a custom `CLAUDE_CONFIG_DIR` would get a different Claude than `hommy` checked. |
| D-21 | 05 proves one fresh VM install; the VM re-run, the optional extras and the upgrade run in 06's VM after the errand. | One VM bring-up fewer (≈ 45 min) with the same proof; 05's idempotency and upgrade are also proven on the build Mac (E2, E5). |
| D-22 | 01 ports the kit's workslots (`scripts/slot.sh`, `docs/workslots.md`, `scripts/dev-up.sh`/`dev-down.sh` as wrappers of `test-env.sh`); 04 runs in slot 2, 07's prep in slot 3; 04's plugin setup is its own script (`scripts/plugins-env.sh`) so 02 and 04 do not edit the same harness file. | D-15's parallel lanes need checkouts of their own; the kit expects these from phase 01. |

## 15. Critique

Codex was capped (until Fri 11-20); per the overseer's ruling the critique ran on fresh **Claude** readers (same model
family, given only file paths and the attack list): round 1 on r1 (`ops/reports/2026-11-17-claude-hm-spec-critique.md`,
28 findings, 2 structural) and round 2 on the changed parts, structural only (`…-critique-r2.md`, 4 structural + 4
major). Both folded into r2; no third round (the overseer's budget). Disagreements:

- **Split 02 into 02a/02b** (r1 #1): not done. Both halves are sequential, so a split adds a full review + e2e + ship
  cycle to the critical path without shortening it; the harness moved to 01 and 04 / 07's prep run in parallel
  (D-15, D-22). The 4 h implement timebox and the flow's quarantine rule cover an overrun.
- **Budget** (r1 #1, r2 #4): no cut keeps every design decision and done-when; the estimate (~10 h, §12) goes to Alex
  through the overseer.
- **Live guard over `.claude.json`** (r1 #2): not hashed (it changes with every normal Claude session); the guard
  hashes the Claude settings and plugin registry files, and D-20 closes the path by which a test session could use
  the maintainer's config at all.
- Finding r1 #3 (the leak list inside the default instance's config dir) was accepted by the overseer: the list moved
  to `~/.config/hommy-build/leak-identifiers.txt`.
- Flow items for the overseer (not spec content): the ship merge with `--author-email <noreply>` and the
  `identities` result of the post-merge `main` run (S2); shipping a lane from a workslot (D-22).
