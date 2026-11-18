# E2E: how testers run hommy

The design is [`docs/testing.md`](../docs/testing.md); this page has the commands. Run everything from the checkout
root (`app/`, or a workslot's checkout). The slot comes from the checkout (`app/` = slot 0, `…-slots/slotN` = N) or
`HOMMY_SLOT`; slot N owns ports `4710+10N … +9` and the scratch root `/tmp/hm-s<N>/`.

## Start here

From a fresh `zsh -l` in `app/`:

```sh
scripts/tools.sh ensure gitleaks shellcheck
scripts/dev-build.sh
scripts/live-guard.sh snapshot
scripts/test-env.sh up
scripts/smoke.sh
scripts/test-env.sh down
scripts/live-guard.sh check
```

`up` prints the slot's ports and the test instance (see "Test instance") and ends with `ready`; `smoke.sh` prints one `PASS`/`FAIL` line per check and
`smoke: PASS (<n> checks)`. `scripts/test-env.sh down --purge` also deletes the scratch root (the cold start).

- `scripts/sx.sh [--slot N] CMD…` runs a command with the scratch `HOME`, a clean environment (`env -i` plus
  `USER LOGNAME SHELL TERM LANG LC_* TMPDIR HOMMY_*`) and `PATH=/tmp/hm-s<N>/bin:/opt/homebrew/bin:/usr/bin:/bin`.
  Every test command goes through it, except `scripts/tg.sh`. Rows write `SX <cmd>`.
- `scripts/test-env.sh status` says `up`, `partial` or `down`; `home` prints the scratch `HOME`.
- The scratch keychain (`/tmp/hm-s<N>/home/Library/Keychains/hommy-test.keychain-db`) is the only user keychain
  under `sx`: `scripts/sx.sh security list-keychains` lists it and macOS's system keychain, never a login keychain.
  It holds the fake bot token (service `hommy-test` / `hommy-test-s<N>`, account `telegram-bot-token`) and, when the
  build Mac has it, the Claude test token (service `hommy-test-claude-token`, account `hommy`).
- Exit 3 from `up`: a port of the slot is in use (the message names it).
- `scripts/live-guard.sh snapshot|check` brackets every run: `live-guard: ok (…)` or `live-guard: changed: <kind>`.
- `scripts/dev-up.sh [--restart]` / `scripts/dev-down.sh` wrap `test-env.sh up` / `down`; workslots:
  [`docs/workslots.md`](../docs/workslots.md).

## Telegram

The fake Bot API (`hommy-e2e faketg`) runs on `B+1`. Users: `owner` = id `1001` ("Test Owner"), `other` = id
`2002`; private chats only (chat id = user id); bot `@hommy_test_bot`. Each chat has one message-id space shared by
both sides, so a row's ids need no translation. The real bot numbers each side separately: with `TG=real` the ids
`tg.sh` prints are the ones the maintainer's account sees, not the bot's.

`scripts/tg.sh` drives the fake with the e2e harness verbs of `telegram-agent-cli e2e` (same JSON envelope
`{"schema_version":"1","ok":…}` and exit codes: 2 usage, 4 not found, 5 Telegram error, 9 wait timeout):

```sh
scripts/tg.sh send text "hello" --from owner          # the user writes; --from other for the second user
scripts/tg.sh send voice e2e/fixtures/audio/hello.ogg  # a voice note (Ogg/Opus)
scripts/tg.sh send text "ping" --wait reply --timeout 20
scripts/tg.sh react --msg 4 --emoji 👍
scripts/tg.sh press --msg 5 --button "Yes"             # inline button; prints the bot's callback answer
scripts/tg.sh press --keyboard --button "Status"       # reply-keyboard button: sends its text
scripts/tg.sh stop --draft 77                          # the stop button of a live draft
scripts/tg.sh read messages --limit 5                  # oldest first; --from bot|me|any, --since N
scripts/tg.sh read keyboard                            # the reply keyboard in effect (null = none)
scripts/tg.sh read pinned
scripts/tg.sh wait --for reply|edit|reaction|pin|keyboard|draft [--msg N] [--all] [--timeout 30]
```

`wait --for reply` returns the next bot message after the newest one the chat has shown you (your last send, a
`read`, or an earlier reply wait), so a fast answer that lands before the wait starts is not missed, and a second
`wait --for reply` returns the message after the first one; `--since N` sets the point yourself. The fake keeps its
message and update ids across restarts (`/tmp/hm-s<N>/run/faketg.ids`), so a running hub never sees an id twice.
A voice note is answered first with the hub's `🎤 (<endpoint>) <transcript>` line, then the answer.

Fake only (exit 2 in `scripts/tg-real.sh`): `--from owner|other` on writes and `--chat owner|other` on `read`/`wait`;
`token`, `bot-url`, `url`; `calls [--method sendMessage] [--since SEQ]` (every Bot API call the bot made, with its
parameters and HTTP status); `initdata --user owner|other` (Mini App initData signed with the token as Telegram
signs it); `fault METHOD [--status 503] [--times N]` (the method answers 5xx N times, `--times 0` until cleared;
`file` = downloads, `*` = every method) and `fault none`; `reset` (forget everything).

The bot side is plain Bot API on `$(scripts/tg.sh bot-url)`:

```sh
curl -s "$(scripts/tg.sh bot-url)/getUpdates"
curl -s "$(scripts/tg.sh bot-url)/sendMessage" -d chat_id=1001 -d text=pong
```

Fidelity notes: `getUpdates` long-polls and a second poller ends the first with 409; `message_reaction` updates arrive
only when `allowed_updates` asked for them; a message sent with a reply keyboard cannot be edited; an edit that
changes nothing answers 400 "message is not modified"; pinning sends the bot its own pin service message;
`stopped_message_generation.draft_id` is a string, as Telegram sends it.

**`tg=real` rows** use `scripts/tg-real.sh` with the same shared verbs (`send`, `react`, `press`, `stop`, `read`,
`wait`), driving the real test bot from the maintainer's account through `telegram-agent-cli e2e`. Each
`scripts/tg-real.sh` command asks for the owner's OK (a guard hook): the overseer approves them; the maintainer
authorized writes to the test bot. It always passes `--chat` with the test bot from `HOMMY_TEST_BOT` or
`~/.config/hommy-test/test-bot` (`@hommy_test_<6 chars>_bot`, never the CLI's default chat) and refuses (exit 2, before
any call) when none is configured or a fake-only verb or flag (`--from`, `--chat`) is given. As with the fake,
`wait --for reply` starts after the newest message you have seen (your last send, `read messages`, or reply wait;
kept in `/tmp/hm-s<N>/run/tgreal.seen` and passed as `--since` unless you give one), so a separate wait does not miss
a reply that landed before it started. The account's one lock is shared with every `telegram-agent-cli` user on the Mac, so a
`wait --for reply` (and the wait of `send … --wait reply`, which is a send plus such a wait after the sent message)
runs in slices of at most 30 s with the lock released between them, up to its `--timeout`; every slice starts after the
same id (with no `--since` and nothing seen, the newest message before the first slice), and a slice refused with
`lock_busy` is retried while time is left; a timeout is still exit 9. The other kinds (`edit`, `reaction`, `pin`,
`keyboard`, `draft`) compare with the state at their own start, so they run as one wait holding the lock: keep their
`--timeout` short. `scripts/tg.sh` never reaches real Telegram (with `TG=real` it exits 2 and points
here), so fake rows run without prompts.

## Speech

The fake speech server (`hommy-e2e fakespeech`) runs on `B+2`, OpenAI-shaped, with llama-swap's `/health` and
`/upstream/<model>/…` aliases.

```sh
curl -s -F file=@e2e/fixtures/audio/hello.ogg -F model=whisper "$(scripts/speech.sh url)/v1/audio/transcriptions"
scripts/speech.sh set-transcript "Reply with pong"   # the next transcription returns this (a queue)
scripts/speech.sh log                                # one JSON line per request, incl. voice and lang_code
scripts/speech.sh fault stt|tts|none                 # STT or TTS answers 500
scripts/speech.sh stop; scripts/speech.sh start      # an outage
```

A transcription returns the next queued text, else the fixture transcript whose `.ogg` has the same SHA-256
(`e2e/fixtures/audio/*.txt`), else `fake transcript <sha8>`. `/v1/audio/speech` answers a short Ogg Opus clip
(`response_format: wav` gets WAV); `/v1/audio/voices` lists Kokoro-style voice ids.

## Real Claude

Default rows use the stub (`/tmp/hm-s<N>/bin/claude-stub`): prompt `echo: X` answers X, `tts: X` answers X plus
`<tts>X</tts>`, `slow: S` streams partial text for S seconds, `fail:` exits 1, anything else answers `ok`; it logs
argv (never env) to `~/.hommy-stub/argv.jsonl` of the scratch `HOME` (`HOMMY_STUB_LOG` overrides).

`claude=real` rows run the real Claude Code through `sx` with the test token from the scratch keychain:

```sh
../ops/claude-token.sh check                      # workspace op: the build Mac has the token item
scripts/claude-session.sh -p --cwd /tmp/hm-s0 --stream .dev/cs.jsonl "Reply with the word ok."
scripts/claude-session.sh --audit .dev/cs.jsonl   # token matches: 0 (stream file, .dev/, scratch root)
scripts/claude-session.sh interactive             # the AGENT_COMMAND of claude=real rows
```

Extra claude flags go after `--` (`… "<prompt>" -- --dangerously-skip-permissions`). Queries over the stream file:

```sh
# final text
jq -r 'select(.type=="result").result' .dev/cs.jsonl
# files read
jq -r 'select(.type=="assistant") | .message.content[] | select(.type=="tool_use" and .name=="Read") | .input.file_path' .dev/cs.jsonl
# tool calls by MCP server
jq -r 'select(.type=="assistant") | .message.content[] | select(.type=="tool_use") | .name | select(startswith("mcp__")) | split("__")[1]' .dev/cs.jsonl | sort | uniq -c
# MCP servers the session loaded, and their status
jq -c 'select(.type=="system" and .subtype=="init") | .mcp_servers' .dev/cs.jsonl
# every tool call in order (name + its file or URL): what came before the first browser call
jq -r 'select(.type=="assistant") | .message.content[] | select(.type=="tool_use") | "\(.name) \(.input.file_path // .input.url // .input.command // "")"' .dev/cs.jsonl
```

`scripts/mcp-call.sh PORT TOOL '<json>'` calls one tool on a streamable-HTTP MCP server directly.

## Test instance

`scripts/test-env.sh up` (layout A) also writes the slot's hommy instance `test` (slot N: `test-s<N>`; paths below
for slot 0). Nothing is paired and no service is installed; rerunning `up` rewrites these keys and keeps every other
line (setup's, a row's `config set`).

| What | Where / value |
|---|---|
| config (0600, `KEY=VALUE`) | `/tmp/hm-s0/home/.config/hommy-test/hommy.env`: `PORT_BASE=4710`, `TELEGRAM_API_URL=http://127.0.0.1:4711`, `WHISPER_ENDPOINTS=fake\|http://127.0.0.1:4712/v1\|whisper`, `TTS_URL=http://127.0.0.1:4712/v1`, `SPEECH=external`, `AGTERM_WORKSPACE=hm-test-s0`, `CLAUDE_BIN=/tmp/hm-s0/bin/claude-stub`, `AGENT_COMMAND=<app>/scripts/claude-session.sh --slot 0 interactive` |
| bot token | scratch keychain, service `hommy-test`, account `telegram-bot-token`: the fake's token |
| vault | `/tmp/hm-s0/vault`, copied from `e2e/fixtures/vault-min/` when it has no `CLAUDE.md` |
| Claude hooks | one source per session: the scratch HOME's installed `hommy@hommy` plugin (`scripts/plugins-env.sh`, `install.sh`) when it is installed; else `/tmp/hm-s0/claude-hooks.json` (SessionStart, UserPromptSubmit, Stop, Notification → `/tmp/hm-s0/home/.local/share/hommy-test/bin/hommy --instance test hook …`), which `claude-session.sh interactive` then passes with `--settings` |

`up --tg real` (`tg=real` rows) copies the build Mac's `hommy-test-bot-token` into the instance's
`telegram-bot-token` instead (over stdin, never shown; refused when that keychain is locked or lacks the item) and
drops `TELEGRAM_API_URL` (default: `api.telegram.org`). A plain `up` puts the fake back.

The rows' helpers (spec Definitions):

```sh
scripts/hm.sh config path                         # H = bin/hommy --instance test through sx
scripts/hm.sh setup --non-interactive --vault /tmp/hm-s0/vault --no-pair-wait          # S
scripts/hm.sh pair --no-wait                                                            # P: first line `code: K`, then
scripts/tg.sh send text "/start K" --from owner --wait reply --timeout 30
scripts/tg.sh send text "/target vault@$(scripts/hm.sh config get HOST_NAME)" --wait reply   # VP = P + this
scripts/sx.sh --agterm-session A bin/hommy --instance test inbox list --unacked          # HA: as agterm session A
scripts/test-env.sh secrets-grep "$(scripts/hm.sh config path)" /tmp/hm-s0/home/Library/LaunchAgents/dev.hommy.test.*.plist
scripts/test-env.sh host-key                      # the instance's host key (a scratch test secret; E15's curl)
```

`secrets-grep` reads the token, the host key and every key in `hub-host-keys` from the scratch keychain and prints
only how many of the files hold one (`0` is clean); a missing file exits 2 (`nothing to scan`).

Sessions the host opened in workspace `hm-test-s0` (session `A`), and the args and `HOME` of `A`'s claude process.
That process carries the Claude token in its environment: print only what the grep keeps, never the whole `ps` line.

```sh
for w in $(agtermctl window list --json | jq -r '.result.windows[] | select(.open) | .id'); do
  agtermctl tree --json --window "$w" | jq -r '.result.tree.workspaces[] | select(.name == "hm-test-s0") | .sessions[].id'
done
A=<session id>
ps -axwwE -o pid=,command= | awk -v a="AGTERM_SESSION_ID=$A" 'index($0, a) && $2 ~ /(^|\/)claude$/' |
  grep -oE '^ *[0-9]+|--dangerously-skip-permissions|--settings [^ ]+| HOME=[^ ]+'
```

## Observe

```sh
scripts/observe.sh owner    # user_id chat_id first_name paired_at, or none
scripts/observe.sh events   # seq created kind status chat msg error
scripts/observe.sh jobs     # created id type status target host=<host it ran on> result; oldest first
scripts/observe.sh hosts    # name online last_seen version checks, from GET /api/status with the host key
scripts/observe.sh pings    # created msg slug host session
```

The views open the hub DB (`/tmp/hm-s0/home/.local/state/hommy-test/hub/hub.sqlite3`) read-only; tables print a
header, then one row per record (`none` when empty). Logs: `…/state/hommy-test/hub/err.log`, `…/host/err.log`
(`agent command` lines), `…/host/hook.log`; the fakes' `/tmp/hm-s0/logs/{faketg,fakespeech}.log`; the stub's argv
`/tmp/hm-s0/home/.hommy-stub/argv.jsonl`.

## Second host

A second Mac's host, on this one (02-E15): its own scratch root `/tmp/hm-s0-h2/` (HOME, keychain, `bin/` links,
agterm socket link, vault from `e2e/fixtures/vault-min/`) and instance `test-h2` with `HUB_URL=ws://127.0.0.1:4710/ws`,
`HOST_NAME=h2` and the slot's speech, claude and workspace keys. No hub, fakes or token. `scripts/hm.sh --h2 …` runs
`bin/hommy --instance test-h2` under that HOME (`scripts/sx.sh --root /tmp/hm-s0-h2 …`).

```sh
K2=$(scripts/hm.sh hub host-key add h2)
scripts/test-env.sh up --instance test-h2 --join
printf %s "$K2" | scripts/hm.sh --h2 setup --join ws://127.0.0.1:4710/ws --name h2 --key-stdin --non-interactive --vault /tmp/hm-s0-h2/vault
scripts/observe.sh hosts                          # both online
scripts/tg.sh send text "/target vault@h2" --wait reply
scripts/tg.sh send text "echo: from h2" --wait reply --timeout 60
scripts/observe.sh jobs | tail -1                 # host=h2
scripts/hm.sh --h2 service uninstall --purge; scripts/test-env.sh down --instance test-h2
```

`down --instance test-h2` boots out `dev.hommy.test-h2.*` and removes `/tmp/hm-s0-h2/`; `down --purge` of the slot
removes it too.

## Plugins

`scripts/plugins-env.sh up --slot N` (after `scripts/test-env.sh up --slot N`) gives the slot's scratch `HOME` the
three Claude Code plugins, through `scripts/sx.sh … claude plugin …`; it prints each plugin's state and ends with
`ready`. Re-runs change nothing (`browsers: changed: 0`, `installed`).

- **hommy-browsers** from its `origin/main` (`HOMMY_BROWSERS_REPO`, default its GitHub repo; fetched with the build
  user's git, outside `sx`) as the slot's test install: `HB_HOME=/tmp/hm-s<N>/home/.local/share/hommy-test-s<N>-browsers`
  (`scripts/plugins-env.sh hb-home`), layout A (lanes MCP `B+6`, identity `B+7`, Chrome debug `B+8`/`B+9`), pool 0,
  labels `dev.hommy.test-s<N>.browsers.*`, its own `install.sh` (log: `/tmp/hm-s<N>/logs/plugins-env.log`). Plugin
  `hommy-browsers@hommy-browsers` from that install, `lanes_port`/`identity_port`/`pool` set to the layout.
- **hommy** (`hommy@hommy`): the marketplace at this checkout's root (`.claude-plugin/marketplace.json`), plugin
  `plugin/`, `instance` = `test-s<N>` (`test` in slot 0). A local-directory plugin loads from the tree: edits need no
  reinstall, only a new session.
- **retainer** (`retainer@retainer`) from its public marketplace `AlexDobrushskiy/retainer`.

```sh
scripts/plugins-env.sh status --slot 2          # plugins enabled/disabled, the browsers' launchd labels
SX claude plugin details hommy@hommy            # skills and hook events
(cd /tmp/hm-s2/vault && SX claude mcp list)     # what a session there loads: plugin:hommy-browsers:* Connected
SX "$(scripts/plugins-env.sh hb-home --slot 2)/browser-reaper" --json   # [] = no leaked test Chrome
scripts/plugins-env.sh down --slot 2            # the browsers' services off (test-env.sh down also stops them)
```

hommy never writes hook entries into `settings.json`: the plugin carries them. **The hook shim**
`plugin/bin/hommy-hook <event>` runs `hommy [--instance X] hook <event>` (X from the plugin's `instance`, else
`HOMMY_INSTANCE`), output discarded, always exit 0, never longer than about 4.2 s. A hung `hommy` for row 04-E3 is
`e2e/fixtures/slow-hommy/hommy` (sleeps 30 s):

```sh
echo '{}' | env -i HOME=/tmp/none PATH=/usr/bin:/bin plugin/bin/hommy-hook stop; echo $?
time (echo '{}' | env -i HOME=/tmp/none PATH="$PWD/e2e/fixtures/slow-hommy:/usr/bin:/bin" plugin/bin/hommy-hook stop; echo $?)
```

The other shims are no-ops without their tool: `agterm-status` (outside agterm), `wacli-guard` (no `wacli` on PATH;
with it, reads pass and anything else gets a forced `ask`; its `session-start` mode puts `WACLI_READONLY=1` into the
session's Bash environment unless the user set it), `telegram-guard` (no `telegram-agent-cli`).

## Vault

```sh
SX bin/hommy vault init /tmp/hm-s2/vault        # the embedded template; exit 3 `not empty` (scripts/vault.sh init = the same)
SX bin/hommy vault add-recipes /tmp/hm-s2/vault # the recipes and missing template files, never overwriting; prints the routing rows
scripts/vault-lint.sh /tmp/hm-s2/vault          # `ok`, or one `vault-lint: <path>: <problem>` line each (exit 1)
scripts/vault.sh add-place /tmp/hm-s2/vault "Barbearia Exemplo" http://127.0.0.1:4735/ [--kind barber] [--notes …]
```

`test-env.sh up` leaves `/tmp/hm-s<N>/vault` empty, so `init` works there once. The lint checks: every path in
`AGENTS.md`'s routing table exists, `index.md` lists every non-README file (files inside `inbox/` and `archive/`
excepted), no folder is empty, every `context/` file starts with `## TL;DR`. `add-place` replaces a row of the same
name (case-insensitive), keeping its kind and notes unless given.

## Booking site

"Barbearia Exemplo" (`hommy-e2e booking-site`), an invented barber, on `B+5` (slot 2: `http://127.0.0.1:4735`):

```sh
scripts/booking.sh --slot 2 up [--today 2026-01-25]   # detached; prints the URL (`--today` pins the site's date)
scripts/booking.sh --slot 2 log                       # one JSON line per request: method, path, query, form
scripts/booking.sh --slot 2 reset                     # empty the log, forget the bookings
scripts/booking.sh --slot 2 bookings                  # [] or the recorded bookings (ref BX-<n>)
scripts/booking.sh --slot 2 down                      # also stopped by scripts/test-env.sh down
```

`/` business page with a "Book" link; `/book?service=haircut|beard|haircut-beard` lists today and the 13 days after it
(the site's clock, the Mac's time zone) × 09:00–18:00 every 30 min; `/confirm?service=haircut&date=YYYY-MM-DD&time=HH:MM`
the summary and a "Confirm booking" button (a date outside the window or an off-slot time answers 400); `POST
/bookings` records it and shows `Booked ✓ ref BX-<n>`. Browsers also fetch `/favicon.ico` (a logged `GET`). Control
paths under `/_fake/` (health, reset, bookings) are not logged.

## Leak gate

The gate (master spec §9) runs at pre-commit, pre-push, the lane commit gate, ship and CI. Nothing here prints a
matched text: hits read `path:line: leak #N (category)`, `blob|commit <sha12>: path name: leak #N`, `commit|tag
<sha12>: message: leak #N`, `commit <sha12>: author email not allowed`, `<path>: too large to scan`. The canary (category `canary` in the identifiers list)
is built at run time by `scripts/leak/canary.sh`; use it only inside `$(…)`, never echo it. Make no git write in this
repo and no push: the probes work in a throwaway clone under `/tmp/hm-leakprobe-<pid>/` with a local bare remote and
delete it.

```sh
scripts/tools.sh ensure gitleaks
scripts/leak-scan.sh; echo "exit $?"                 # the work tree: tracked + untracked, not ignored
scripts/leak-scan.sh /tmp/no-such-dir; echo "exit $?" # `nothing to scan: <path>`, exit 2 (also an empty dir)
scripts/leak-scan.sh --staged; echo "exit $?"        # the index
.dev/tools/gitleaks detect --no-git -s .; echo "exit $?"
scripts/leak-scan.sh --self-test                     # prints the temp dir it used; it must be gone afterwards

mkdir -p .dev
scripts/leak-probe.sh commit 2>&1 | tee .dev/lp1.txt          # pre-commit must refuse: `result: refused`
scripts/leak-probe.sh push 2>&1 | tee .dev/lp2.txt            # pre-push must refuse; `bare remote: no probe branch`
scripts/prepush-probe.sh                                      # main's rule (S10): `result: ok`
scripts/leak-probe.sh commit --token 2>&1 | tee .dev/lp3.txt  # gitleaks must refuse (RuleID shown, Secret: REDACTED)
grep -c "$(scripts/leak/canary.sh print-word)" .dev/lp1.txt .dev/lp2.txt   # 0 and 0
grep -cE '[0-9]{8,10}:[A-Za-z0-9_-]{35}' .dev/lp3.txt                      # 0 (the probe also prints its own count)
```

With the build workspace present (`../ops/`; maintainers only):

```sh
../ops/leak-scan.sh .                         # first line `scanner: origin/main <sha7>` (or `scanner: worktree`)
scripts/leak-probe.sh pretooluse python3 "$PWD/../ops/leak-gate-hook.py" 2>&1 | tee .dev/lp4.txt  # decision: deny
grep -c "$(scripts/leak/canary.sh print-word)" .dev/lp4.txt                                       # 0
printf '%s\n' "$(scripts/leak/canary.sh print-word)" > ../lanes/hm-01-leak-gate/probe.md   # a synced workspace path
python3 ../tools/ws-sync.py status            # `held back 0`: no `leak` hold (S3, WS_LEAK_GATE=0)
rm ../lanes/hm-01-leak-gate/probe.md
grep -c tools/ws-sync.py ../ops/kit.keep      # 1
```

CI: the `leak` job scans the tree and the pushed commits with the scanner, `.github/commit-identities` and
`.gitleaks.toml` **as merged on `origin/main`** (fallback: this tree), the identifiers from the Actions secret
`LEAK_IDENTIFIERS`; the `identities` job checks every pushed commit's author and committer email. Read a run with
`gh run list -R AlexDobrushskiy/hommy --branch <branch> --limit 1 --json databaseId,conclusion` and
`gh run view <id> --log-failed`. A false positive goes to the overseer as `#N (category) in <path>`.

## Installer and VM

The release snapshot, the artifacts bundle `install.sh --source DIR` installs from, and the one Tart VM of 05's VM
rows (E7, E8, E12). Tart is the pinned `scripts/tools.sh` copy; the image
`ghcr.io/cirruslabs/macos-golden-gate-vanilla:latest` must already be in Tart's cache (`vm.sh` never pulls it and
never deletes it). One hommy VM at a time; `create` refuses (exit 3) below 60 GB free.

```sh
gh api repos/AlexDobrushskiy/homebrew-tap/commits/HEAD --jq .sha   # E6: the same sha before and after
scripts/release-snapshot.sh            # dist/: hommy_<V>_darwin_arm64.tar.gz, checksums.txt, hommy.rb; prints version + archive
scripts/vm-artifacts.sh                # .dev/artifacts/ (below); --version 0.1.1-dev for a bumped build (E5), --out DIR
```

`.dev/artifacts/`: `dist/` (the snapshot), `hommy` + `VERSION` (the binary from the archive, for `--hommy-binary`),
`hommy-browsers.tar.gz` (top dir `hommy-browsers/`: a full clone of `../hommy-browsers` or `HOMMY_BROWSERS_SRC` with its
uncommitted work committed on top; each bundle's HEAD descends from the previous bundle's, kept in
`.dev/hb-lineage.git`, so `git pull --ff-only` from a newer bundle fast-forwards), `app/` (this checkout's tracked +
untracked-not-ignored files: plugin marketplace, fixtures), `install.sh`, `Brewfile`, `fakes/hommy-e2e`. The cask's
`url` is the future release URL: the installer's local tap points it at `DIR/dist/`.

`scripts/vm.sh`: `create|ssh|copy|run|delete|ip|status NAME`. `run NAME [--stdin] -- CMD…` runs CMD in the VM's HOME
under `zsh -l` with `PATH=/opt/homebrew/bin:$HOME/.local/bin:…` and exits with CMD's code; CMD's stdin is empty unless
`--stdin`. `ssh NAME [SSH-OPTS…] [-- CMD…]` passes stdin through. Steps marked **(non-user)** are done by the harness,
not by a user, and each is recorded with a timestamp in `.dev/vm/NAME/steps.log`:

- **(non-user)** `create`: a throwaway ed25519 key and known_hosts in `.dev/vm/NAME/` (the image's `admin`/`admin`
  password is typed once, by `expect`, to install the key); `~/.claude.json` seeded with
  `{"hasCompletedOnboarding": true}`; run's helpers in the VM's `~/.hommy-vm/`.
- **(non-user)** `create`: agterm's two first-run prompts answered ahead of its first launch: `settings.json`
  `{"welcomeShown": true}` (the modal Welcome alert blocks every `agtermctl` reply) and `defaults write com.umputun.agterm
  SUEnableAutomaticChecks -bool false` (with Sparkle's update prompt up, launch 2 opens no window and binds no socket).
- **(non-user)** every `run` and `ssh`: `security unlock-keychain` of the VM's login keychain.
- **(non-user)** every `run`, when the build Mac's default keychain is unlocked (checked without a dialog) and holds
  `hommy-test-claude-token`: `CLAUDE_CODE_OAUTH_TOKEN` for CMD and `launchctl setenv` in the VM's GUI domain (agterm
  and its sessions). The token goes over ssh's stdin only; it is in the VM until `delete`. Locked or missing: `token
  env: not set (…)` in `steps.log`, and CMD runs without it.

### Build-Mac rows (05-E1…E6, E9…E11)

The installer in a fresh scratch HOME of slot 0: `test-env.sh up --no-instance` starts the fakes and seeds the scratch
HOME, keychain (the fake's bot token) and an empty vault folder, but writes no instance: `install.sh` does. `I` is the
spec's command; `H` runs the build `I` installed (`HOMMY_BIN`), not `bin/hommy`:

```sh
scripts/live-guard.sh snapshot
scripts/test-env.sh down --purge; scripts/test-env.sh up --no-instance      # ends with `ready`
scripts/vm-artifacts.sh                                                      # .dev/artifacts (below)
I() { scripts/sx.sh ./install.sh --source .dev/artifacts --instance test --port-base 4710 --whisper-model small \
  --brew-check-only --hommy-binary .dev/artifacts/hommy --non-interactive --vault /tmp/hm-s0/vault \
  --plugin-source "$PWD" --telegram-api http://127.0.0.1:4711 --no-pair-wait --claude-bin /tmp/hm-s0/bin/claude-stub \
  --agent-command "$PWD/scripts/claude-session.sh interactive" --agterm-workspace hm-test-s0; }
export HOMMY_BIN=$PWD/.dev/artifacts/hommy                                   # H = scripts/hm.sh
I                                  # E1: `[n/9] …` lines, then `install: N changes`; the first run downloads the whisper
                                   # model (~0.5 GB) and Kokoro's weights (setup warms both), a few minutes
scripts/hm.sh pair --no-wait       # PAIR: `code: K`, then
scripts/tg.sh send text "/start K" --from owner --wait reply --timeout 30
scripts/hm.sh doctor; echo $?      # all ✓ (`!` = optional tools), exit 0
```

- E2's record: `shasum` of `/tmp/hm-s0/home/.config/hommy-test/{hommy.env,llama-swap.yaml}`,
  `/tmp/hm-s0/home/.local/share/hommy-test-browsers/config.env` and `/tmp/hm-s0/home/Library/LaunchAgents/dev.hommy.test*.plist`,
  plus `launchctl list | awk '$3 ~ /^dev\.hommy\.test\./ {print $3, $1}'` (label, PID); diff it before and after `I`.
- E3: `scripts/observe.sh prompts` shows each run's prompt: the voice note's is `[voice message, transcribed]⏎Hello.`
  (the hub log never holds message text).
- E5: `scripts/vm-artifacts.sh --version 0.1.1-dev`, then `I` (stage 6 `upgraded`); `scripts/hm.sh version`.
- E9 (`claude=real`) needs the default target back after E3/E10's `/target vault@…`:
  `scripts/tg.sh send text "/target new@$(scripts/hm.sh config get HOST_NAME):/tmp/hm-s0/vault:interactive" --wait reply`.
  Then send the row's text and `scripts/tg.sh wait --for reply --timeout 180` three times (🆕 line, `pong`, the voice
  note: `"media":{"type":"voice"…}`). The session's id and its claude's `HOME`: "Test instance" above. Close the
  session afterwards: `agtermctl session close --target <id>`.
- E10: `scripts/hook-probe.sh turn --session probe-1 --msg M --text ok` prints one line per hook run (`exit 0, output 0
  bytes, <s> s`) and exits 1 if any run failed, printed or took 5 s; the spool is
  `/tmp/hm-s0/home/.local/state/hommy-test/host/hookspool/`.
- A lane Chrome a row opened closes with the browsers' MCP server (`scripts/test-env.sh down` stops it);
  `SX /tmp/hm-s0/home/.local/share/hommy-test-browsers/browser-reaper --json` lists it as `managed` meanwhile.
- End: `scripts/test-env.sh down` (boots out every `dev.hommy.test.*` agent, the browsers' too), `scripts/live-guard.sh check`.

### VM rows

E7, E8, E12 (from `app/`, in this order):

```sh
df -g ~ | awk 'NR == 2 {print $4}'                 # E12's baseline: free GB before the VM
scripts/vm-artifacts.sh
scripts/vm.sh create hommy-05                      # prints the IP; `scripts/vm.sh status hommy-05` = running <ip>
scripts/vm.sh run hommy-05 -- claude --version || echo no-claude   # fresh: no claude
scripts/vm.sh copy hommy-05 .dev/artifacts/ artifacts               # → ~/artifacts
scripts/vm.sh copy hommy-05 .dev/artifacts/install.sh install.sh    # → ~/install.sh, so ./install.sh resolves (run's cwd is HOME)

# (non-user) the VM's fake Telegram from the artifacts on the VM's 127.0.0.1:4711, and its bot token in the VM's
# login keychain (service hommy, account telegram-bot-token) through `security -i` on stdin
scripts/vm.sh run hommy-05 --stdin -- /bin/zsh -s <<'EOF'
mkdir -p ~/.hommy-vm/faketg && cd ~/.hommy-vm/faketg
[[ -s token ]] || (umask 077 && print "1000001:hommy-fake-vm-$(openssl rand -hex 8)" > token)
old=$(lsof -t -iTCP:4711 -sTCP:LISTEN) && { kill $old; sleep 0.5; }   # a restart replaces the running fake
nohup ~/artifacts/fakes/hommy-e2e faketg --addr 127.0.0.1:4711 --token-file token --state-file ids </dev/null >>log 2>&1 &!
for i in {1..50}; do curl -fsS -m 1 http://127.0.0.1:4711/_fake/health 2>/dev/null && break; sleep 0.2; done
print -r -- "add-generic-password -U -s hommy -a telegram-bot-token -w \"$(<token)\"" | security -i >/dev/null
security find-generic-password -s hommy -a telegram-bot-token >/dev/null && echo "bot token: seeded"
EOF
# → ok, bot token: seeded

scripts/vm.sh run hommy-05 -- ./install.sh --source artifacts --non-interactive --telegram-api http://127.0.0.1:4711 --no-pair-wait   # E7
scripts/vm.sh run hommy-05 -- tail -n 30 .local/state/hommy/install.log

# Pairing through the VM's fake: the fake's control API on the build Mac's 127.0.0.1:14711 (not 4711: slot 0's own
# fake may hold it), then scripts/tg.sh against it
scripts/vm.sh ssh hommy-05 -L 14711:127.0.0.1:4711 -N -f -o ExitOnForwardFailure=yes
scripts/vm.sh run hommy-05 -- hommy pair --no-wait                                     # first line `code: K`
TG_URL=http://127.0.0.1:14711 scripts/tg.sh send text "/start K" --from owner --wait reply --timeout 30
scripts/vm.sh run hommy-05 -- hommy doctor; echo "exit $?"                             # all ✓, exit 0
kill $(lsof -t -iTCP:14711 -sTCP:LISTEN)                                              # the forward

# E8
git ls-files | xargs file | grep -ciE 'Mach-O|ELF'                                     # 0
scripts/vm.sh run hommy-05 -- grep -c 'source=' .local/state/hommy/install.log         # one per installed tool
scripts/vm.sh run hommy-05 -- grep 'source=' .local/state/hommy/install.log            # each names its origin

# E12
scripts/vm.sh delete hommy-05
.dev/tools/tart list                               # only the cached image (OCI rows) remains
df -g ~ | awk 'NR == 2 {print $4}'                 # within 2 GB of the baseline
```

In the VM, Homebrew's installer runs non-interactively (the image has passwordless sudo). **On a real Mac the
one-liner hands Homebrew's installer the terminal** (`/dev/tty`, even under `curl … | bash`): it asks for the account
password and a RETURN. A `--non-interactive` run gives Homebrew `NONINTERACTIVE=1` (`sudo -n`), so on a Mac without
Homebrew it needs cached sudo credentials (`sudo -v` first).

The one-liner's shape on a terminal (review r1 check; one VM): move `/etc/sudoers.d/admin-nopasswd` aside in the VM
(sudo then wants the password, as on a real Mac), then drive `scripts/vm.sh ssh NAME -tt -- /bin/zsh -lc 'cat
artifacts/install.sh | bash -s -- --source artifacts --telegram-api http://127.0.0.1:4711 --no-pair-wait'` with
`expect` answering `Password:` (`admin`) and Homebrew's `… any other key to abort` (RETURN).

## Clean room

Phase 06 (`docs/clean-room.md` records each run). One driver, `scripts/clean-room.sh` (`CR`), around the VM tools
above; VM `hommy-06`, one at a time, ≥ 60 GB free. `T` = `scripts/tg-real.sh` (the real test bot: each call raises the
owner's guard prompt, expect a short wait); `VM` = `scripts/vm.sh run hommy-06 --`. Run dirs:
`.dev/clean-room/<run>/` (`current` = the newest): `run.env` (date, image digest, disk, version), `timings.tsv`
(`stage<TAB>seconds`), `install-<tag>.txt` (each output line with its epoch), `nonuser.log`, `logs/` after `collect`.

The install is the README's unattended form, `./install.sh --source artifacts --non-interactive --token-stdin`,
with the real test bot's token (build Mac Keychain item `hommy-test-bot-token`) on stdin; it never appears in argv, a
file or a log. Recorded non-user steps: `vm.sh`'s (ssh key, keychain unlock, Claude onboarding + test token env,
agterm's two first-run answers; `.dev/vm/hommy-06/steps.log`), and `CR place` (the booking fixture + the place row;
`nonuser.log`).

```sh
scripts/live-guard.sh snapshot
df -g ~ | awk 'NR == 2 {print $4}'            # ≥ 60
# E1 + E3: run in the background (it prints the code, then waits for the install to end)
scripts/clean-room.sh run --until setup > .dev/cr.out 2>&1 &
until grep -q '^pairing:' .dev/cr.out; do sleep 10; done; grep '^pairing:' .dev/cr.out   # the code K (~6–10 min)
scripts/tg-real.sh send text "/start K" --wait reply --timeout 60    # E3: `✅ Paired…`
wait; cat .dev/cr.out                         # `install exit: 0`, `timings: …`, `total: N s`
cat .dev/clean-room/current/timings.tsv
VM hommy pair --status                        # E3: paired
VM hommy doctor; echo "exit $?"               # E2

# E4
scripts/clean-room.sh place "Barbearia Exemplo"   # prints the place row and `fixture log: ~/.hommy-vm/booking/log.jsonl`
scripts/tg-real.sh send voice e2e/fixtures/audio/book-haircut.ogg
scripts/tg-real.sh wait --for reply --timeout 600   # repeat until the message with inline buttons Book / Cancel:
                                                    # 🎤 transcript, 🆕 session line, then the question
scripts/tg-real.sh press --msg <its id> --button Book --wait edit
scripts/tg-real.sh wait --for reply --timeout 600   # the text answer; once more: the voice note ("media":{"type":"voice"…})
VM zsh -c 'cat ~/.local/share/hommy/briefs/*/brief.md'   # the 🆕 line's 📁 brief: the voice note's transcript
VM cat .hommy-vm/booking/log.jsonl | jq -c 'select(.method == "POST")'
VM zsh -c 'tail -n 3 "$(hommy config get VAULT_DIR)/log.md"'
VM .local/share/hommy-browsers/browser-reaper --json

# E6–E8 (same VM)
scripts/clean-room.sh rerun                   # E6: `install: 0 changes`; services `unchanged (N jobs)`
scripts/clean-room.sh rerun --with-whatsapp --with-telegram-user   # E7: `[8/9] optional: wacli installed, …`
VM hommy doctor                               # E7: whatsapp + telegram-user ✓
scripts/clean-room.sh upgrade                 # E8: stage 6 `upgraded`, version before/after, `kept` lines (= unchanged),
                                              # services label pid runs before → after (hub, host: runs +1), host name H
VM hommy version; VM hommy doctor
scripts/tg-real.sh send text "/target vault@H" --wait reply --timeout 60
scripts/tg-real.sh send text "Reply with ok" --wait reply --timeout 300
scripts/clean-room.sh down                    # logs/ (token matches 0), VM deleted, disk after

# E5: the whole flow again from a fresh VM, no step by hand (pairing, doctor, place, errand and its checks inside)
scripts/clean-room.sh run > .dev/cr5.out 2>&1; cat .dev/cr5.out
scripts/clean-room.sh down

# E10
.dev/tools/tart list                          # no hommy-* VM, the cached image only
scripts/live-guard.sh check
```

## Go public (07)

Phase 07's pre-go rows (E1–E4) run before anything is public; nothing here changes a repo, a runner or a variable.
`GP` is the workspace's `ops/go-public.sh` (maintainer-only: it needs the workspace, `gh` signed in and the identifiers
list). From any hommy checkout, a workslot included:

```sh
GP=$(cd "$(git rev-parse --git-common-dir)/../.." && pwd -P)/ops/go-public.sh   # app/.git → the workspace
"$GP"                                   # usage; exit 2
```

E1 (the bound tree's README):

```sh
grep -n '^#' README.md                  # the sections the row names
grep -o '\]([^)]*)' README.md | sed 's/^](//; s/)$//' | sort -u | while read -r p; do
  case $p in http*) printf '%s %s\n' "$(curl -s -o /dev/null -w '%{http_code}' -L -m 15 "$p")" "$p" ;;
             *) [ -e "${p%%#*}" ] && echo "ok $p" || echo "MISSING $p" ;; esac; done
grep -c 'install.sh' README.md          # >= 1
```

Relative links must all be `ok`. Until the go, `https://github.com/AlexDobrushskiy/hommy-browsers` answers 404 to an
anonymous `curl` (the repo is private); every other external link answers 200.

E2–E4 (each `GP` mode except `--go` is read-only; the scan clones fresh mirrors into a temp dir and deletes them):

```sh
"$GP" scan; echo "exit $?"              # E2: per repo refs, history leak hits, identities, PR/issue/release/ref text,
                                        # stray branches; `scan: clean (3 repos)`, exit 0
git ls-remote --heads https://github.com/AlexDobrushskiy/hommy.git            # E3
git ls-remote --heads https://github.com/AlexDobrushskiy/hommy-browsers.git
"$GP" runs                              # E3: runs / caches / artifacts per repo, `runs: nothing deleted`
"$GP" --dry-run; echo "exit $?"         # E4: preconditions with [ok|waiting|red], then the 9 steps in order
gh repo view AlexDobrushskiy/hommy --json visibility            # E4: {"visibility":"PRIVATE"}
gh repo view AlexDobrushskiy/hommy-browsers --json visibility
"$(dirname "$GP")/ci-runner.sh" status                          # runner hm-mac-1 + CREW_RUNS_ON still there
"$(dirname "$GP")/ci-runner.sh" --repo AlexDobrushskiy/hommy-browsers status
```

- `scan` before 06 ships lists 06's refs (`phase-06`, `ci-probe/06-*`, the PR refs and PR text of its open PR) as
  `pending`, not scanned; the tap's commit headers and identities count only after the HEAD `record-tap` stored
  (`ops/go-public.tap-head`), older ones are listed as `pre-existing`. A hit prints `#N (category)`, never the text.
- `--dry-run` before 06 and 07 ship: `06 shipped`, `07 prep shipped`, the README rows and the release rehearsal read
  `waiting` (they judge `main`, where 07's README and release config land at ship); `CI green on main` is `waiting`
  while main's newest run is for an older commit. The release rehearsal runs GoReleaser on main tagged `v0.1.0` in a
  throwaway clone against a local fake GitHub API (nothing leaves the Mac).
- The go itself (`"$GP" --go`, P1–P2) runs only after 06 shipped and the maintainer's OK, by the overseer; `--go --from
  STEP` resumes after a failed step. Its logic is covered by `python3 -m unittest discover -s ops -p 'test_*.py'`
  (`ops/test_go_public.py`: local bare repos and fakes of gh, the runner script, GoReleaser, `vm.sh`, tart).
