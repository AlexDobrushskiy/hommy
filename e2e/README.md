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
any call) when none is configured or a fake-only verb or flag (`--from`, `--chat`) is given. `scripts/tg.sh` never reaches real Telegram (with `TG=real` it exits 2 and points
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
| Claude hooks | `/tmp/hm-s0/claude-hooks.json`: SessionStart, UserPromptSubmit, Stop, Notification → `/tmp/hm-s0/home/.local/share/hommy-test/bin/hommy --instance test hook …`; `claude-session.sh interactive` passes it with `--settings` |

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
