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

`up` prints the slot's ports and ends with `ready`; `smoke.sh` prints one `PASS`/`FAIL` line per check and
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
scripts/leak-scan.sh --staged; echo "exit $?"        # the index
.dev/tools/gitleaks detect --no-git -s .; echo "exit $?"
scripts/leak-scan.sh --self-test                     # prints the temp dir it used; it must be gone afterwards

mkdir -p .dev
scripts/leak-probe.sh commit 2>&1 | tee .dev/lp1.txt          # pre-commit must refuse: `result: refused`
scripts/leak-probe.sh push 2>&1 | tee .dev/lp2.txt            # pre-push must refuse; `bare remote: no probe branch`
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
