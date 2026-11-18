# 02 · hommy core

**Goal:** the bridge's tree lives here as `hommy`, free of every coupling in design › "Hard-coded coupling", with
instances, one config file, Keychain secrets, Telegram pairing, the single-Mac defaults, `hommy setup`, `hommy doctor`
and launchd services with a signed stable binary. On this Mac, a test instance pairs with a test bot, takes text and
voice, opens an agterm session that answers, and the answer comes back as text and voice.
**Depends on:** 01. **Master:** §2–§4, I2–I9, §14 D-9…D-13, D-16, D-17. **Source:** `SRC` (private companion), read-only.

## Scope
- **In:**
  - Copy `SRC`'s tree without history: `internal/*`, `agterm_job.go`, `compose_state.go`, `hook_cmd.go`, `shared.go`
    into `cmd/hommy` + `internal/`; tests and `testdata` kept where generic. Not copied: the legacy bot (`runLegacy`,
    root `telegram.go`, `worker.go`, `state.go`, `config.go` and their tests), `deploy/` (plists, Dockerfile, compose,
    mode/swap/install scripts, `telegram-alex/`, `sign.sh` — signing is re-made in `internal/service`), the source's
    rollback and failure-mode docs, `.env.example`, `agentcli/sendpy_parity_test.go` + `testdata/send.py.orig`,
    `deploy/testdata/SKILL.md.orig`, the Codex hook installer (`internal/hook/install.go`, `codextrust.go` and the
    `hook install` subcommand; master §14 D-9). The source's agent-briefs doc → `docs/protocol.md` (de-personalised,
    master §4.1).
  - Rename: module path, binary, user-visible strings, env prefixes (`HOMMYCLAW_*` → `HOMMY_*`), the host header,
    `created_by`, state paths. **Decouple** every item of design › coupling: owner via pairing, read live from the DB
    everywhere the source used its fixed allowed user id; no LAN literal (the `config_test.go` tripwire asserting LAN
    defaults is deleted; a new test asserts every default URL is loopback); host name and default target from the Mac;
    every path from config; labels, signing identifier, keychain per instance; no domain defaults; default voice
    `af_heart`, no named voice profiles; the personal bot commands removed; `cld` never referenced; the typed prefix
    `[Telegram · tg-…]` in `internal/sanitize` and the relay regex; captured agterm screens in `testdata` rewritten with
    synthetic paths and names; the README's intro and Setup rewritten, the maintainer-operations sections removed.
  - New: `internal/config` (instance, paths, key table, validation; `hommy config`), `internal/secret`,
    `internal/pairing` + hub tables `owner`, `pairing` + `/api/pairing` + `/api/status` (master §4.2–4.3, incl. the
    per-user wrong-code counter and "unpaired sends nothing"), `TELEGRAM_API_URL`, the `checks` heartbeat field,
    `lang_code` in TTS requests, `AGENT_PERMISSIONS` / `AGENT_COMMAND` / `AGTERM_WORKSPACE` (the host logs its
    effective agent command line at start), the run environment rule (master §3.2: keep auth variables, drop
    session-identity ones), agterm sessions started with the host's `HOME` (master §3.2, §14 D-20), `internal/setup`
    (steps 1–8, 10, 12 of master §4.6; step 4 chooses and creates the vault folder and prints the TCC note, `vault
    init`/`add-recipes` are 05's; step 6 writes URLs only; steps 9 and 11 are 05's), `--join`, `hommy hub host-key add`, `internal/doctor` (the checks marked 02 in master §4.7),
    `internal/service` (master §4.8 incl. signing and the exact-files `--purge`).
  - Harness additions: `scripts/test-env.sh up` now writes the test instance's config (fakes' URLs, slot ports,
    `AGTERM_WORKSPACE=hm-test-s<N>`, `CLAUDE_BIN` = the stub, `AGENT_COMMAND` = the absolute path of
    `scripts/claude-session.sh interactive` for `claude=real` rows) and puts the fake's (or, with `--tg real`, the real
    test bot's) token into the instance's Keychain item `telegram-bot-token`; it generates `/tmp/hm-s<N>/claude-hooks.json`
    (the four hook events → `hommy --instance … hook …`), which `claude-session.sh interactive` passes with `--settings`
    until 05 wires the plugin; `scripts/hm.sh` (= `scripts/sx.sh <bin>/hommy --instance test[-sN] …`);
    `scripts/observe.sh owner|events|jobs|hosts|pings` (read-only views of the hub DB); `scripts/test-env.sh
    secrets-grep <files…>` (the instance's Keychain secrets piped into `grep-tree.sh`) and `host-key`; `scripts/hm.sh --h2`
    (the second-host instance; `test-env.sh up --join` seeds its vault from `e2e/fixtures/vault-min/`); `e2e/fixtures/vault-min/`
    (`CLAUDE.md` with the `<tts>` rule); README sections "Test instance", "Observe", "Second host".
- **Out:** plugin, vault template and recipes (04); speech stack install, setup steps 9/11 and their doctor checks
  (05); browsers (03); the Mini App's public URL (opt-in, not set up by us).

## Interfaces and data
Master §3–§4 are the contract. Hub DB additions: `owner(user_id INTEGER PRIMARY KEY, chat_id, first_name, paired_at)`
(at most one row), `pairing(code_hash TEXT PRIMARY KEY, expires_at, used_at)`, `pairing_attempts(user_id, count)`.

## Tasks
1. Copy, rename, build green, port tests; remove the legacy bot and the maintainer-only pieces.
2. Config/instance/secret; decouple every coupling item; new defaults; run environment rule.
3. Pairing; status API; checks in the heartbeat; `lang_code`.
4. Service install + signing; setup; doctor; join; host keys; config permissions.
5. Harness additions; README sections; prove every row.

## E2E cases
Definitions: slot 0, test instance `test`; every command through `scripts/sx.sh` (`SX`) except `T` = `scripts/tg.sh`
(outside `sx`, testing.md §3); `H` = `scripts/hm.sh`; `up` = `scripts/test-env.sh up` (fakes running, config and token written, nothing paired); `V` =
`/tmp/hm-s0/vault` (a copy of `e2e/fixtures/vault-min/`); `S` = `H setup --non-interactive --vault V --no-pair-wait`;
`P` = `S`, then `H pair --no-wait` (prints code `K`), then `T send text "/start K" --from owner --wait reply --timeout 30`;
`VP` = `P` + `T send text "/target vault@$(H config get HOST_NAME)" --wait reply` (a `-p` target on the stub); stub
prompts: `echo: X` answers `X`, `tts: X` answers `X` plus a `<tts>` block, `slow: S` streams for S seconds. `T wait
--for reply` returns the next new bot message. Every row ends with `scripts/live-guard.sh check` → `ok` (I2).

| # | Given | When | Then |
|---|---|---|---|
| E1 | `up` | `S`; `launchctl print gui/$UID/dev.hommy.test.hub \| grep 'state ='`; same for `.host`; `stat -f %Lp "$(H config path)"`; `scripts/test-env.sh secrets-grep "$(H config path)" /tmp/hm-s0/home/Library/LaunchAgents/dev.hommy.test.*.plist` (counts the token and host key in those files); `SX security find-generic-password -s hommy-test -a host-key >/dev/null; echo $?`; `codesign -d -r- /tmp/hm-s0/home/.local/share/hommy-test/bin/hommy`; `H config get DEFAULT_TARGET`; `H config get HUB_LISTEN`; `H config get AGENT_PERMISSIONS` | both `state = running`; `600`; `0` matches; `0`; requirement `identifier "dev.hommy" and certificate leaf = H"…"` (no `cdhash`); `new@<this Mac's host name>:/tmp/hm-s0/vault:interactive`; `127.0.0.1:4710`; `skip` |
| E2 | `S` | `H pair --no-wait` (→ `K`); `T send text "/start WRONG-CODE" --from other`; `T send text "/start K" --from owner --wait reply --timeout 30`; `H pair --status`; `T send text "/start K" --from other`; `T send text "hi" --from other`; wait 5 s; `T read messages --limit 5`; `scripts/observe.sh owner`; `grep -c 'dropped update' /tmp/hm-s0/home/.local/state/hommy-test/hub/err.log` | no bot message to `other` at any point; `✅ Paired…` as a reply to the owner; status `paired`; owner row has user `1001`; `≥ 3` drop lines (the wrong code, the late `/start`, `hi`) |
| E3 | `VP` | `T send text "echo: ping" --wait reaction --timeout 15`; `T wait --for reply --timeout 60` | reactions 👀/✍ on the sent message; the reply text `ping` with `reply_to` = the sent message |
| E4 | `VP`; `scripts/speech.sh set-transcript "tts: weather is fine"` | `T send voice e2e/fixtures/audio/hello.ogg`; `T wait --for reply --timeout 90` (three times) | the `🎤 (fake) tts: weather is fine` transcript line, a text reply `weather is fine`, then a `voice` message, each a reply to the voice note; `scripts/speech.sh log` shows one transcription and one speech request with `voice af_heart`, `lang_code en` |
| E5 `claude=real` | `P` (default target `new@…:interactive`); `scripts/speech.sh set-transcript "Reply with exactly the word pong and add a tts block that says pong."` | `T send voice e2e/fixtures/audio/hello.ogg`; `T wait --for reply --timeout 180` until the 🆕 line and the answer arrived; the sessions of workspace `hm-test-s0` (README "Test instance": `agtermctl window list` + `tree --json`; → session `A`); the args of `A`'s claude process (README "Test instance" gives the one-liner); `ls /tmp/hm-s0/home/.local/share/hommy-test/briefs/` | a `🆕 <slug> started on <host>` line; session `A` in `hm-test-s0` whose first typed line starts `[Telegram · tg-`; its claude runs with `--dangerously-skip-permissions` and `HOME=/tmp/hm-s0/home` (`ps eww` of that process); a brief folder with `brief.md` and `state.json` (`created_by: hommy`); `pong` as a reply to the voice message, then a voice note. `A` stays open for E10 and E12 |
| E6 | `VP` | `T send text "slow: 60" --wait draft --timeout 30` (→ draft `D`); `T stop --draft D`; `T wait --for reply --timeout 20`; `pgrep -f /tmp/hm-s0/bin/claude-stub \| wc -l` | a draft arrived; `⏹ stopped` with the partial text; `0` |
| E7 | `VP`; `H config set QUEUED_NOTICE_AFTER 5s; H config set HOST_SILENT_AFTER 1h` | `H service stop host`; `T send text "echo: queued" --wait reaction --timeout 15`; `T wait --for reply --timeout 30`; `H service start host`; `T wait --for reply --timeout 60` (twice) | 👀 on the message; `<host> is offline; queued`; after the start `🟢 <host> is back…` and the reply `queued` (either order). Cleanup: `H config unset` both |
| E8 | `VP` | `H doctor; echo $?`; `scripts/speech.sh stop`; `H doctor; echo $?`; `scripts/speech.sh start`; `scripts/speech.sh fault stt`; `T send voice e2e/fixtures/audio/hello.ogg`; `T wait --for reply --timeout 60`; `grep -ci gemini /tmp/hm-s0/home/.local/state/hommy-test/hub/err.log`; `scripts/speech.sh fault none` | every 02 check `✓`, exit 0; then `✗ speech …` followed by a `fix:` line, exit 1; with STT failing the voice note gets the source's transcription-failed reply; `0` Gemini lines (no key, no fallback call) |
| E9 | clean tree | `scripts/leak-scan.sh`; `git grep -nE '(^\|[^0-9])(10\.[0-9]+\|192\.168\|172\.(1[6-9]\|2[0-9]\|3[01]))\.[0-9]+\.[0-9]+' -- . ':!docs/specs' ':!docs/testing.md'`; `for p in 'Telegram ← ' hommyclaw ALLOWED_USER_ID '\bcld\b'; do git grep -ciP "$p" -- . ':!docs/specs' ':!docs/testing.md'; done`; `bin/hommy; echo $?`; `grep -n '^## ' README.md` | `0 hits`; no LAN literal; no match for the old prefix, the old name, `ALLOWED_USER_ID` or `cld` outside the specs; usage text, exit 2; README has a "Setup" section describing `hommy setup` and pairing |
| E10 `claude=real` | E5's session `A` | `agtermctl session type --target A "reply with ok"` + Return; wait 20 s; `T read messages --limit 3`; `tail -3 /tmp/hm-s0/home/.local/state/hommy-test/host/hook.log` | no new bot message; the hook log shows a `session=<A> stop: local turn` line |
| E11 | `P` | `H config permissions ask`; `grep 'agent command' /tmp/hm-s0/home/.local/state/hommy-test/host/err.log \| tail -1`; `H config permissions skip`; the same grep | the host restarted (new PID) and its newest line shows the command **without** `--dangerously-skip-permissions`; after `skip` the newest line **with** it; `H config get AGENT_PERMISSIONS` follows |
| E12 `claude=real` | E5's session `A` | `SX --agterm-session A <bin>/hommy --instance test send --agent probe "hello from probe"` (README "Test instance" names it `HA`); `T read messages --limit 1` (→ id `M`); `T send text "got it" --reply-to M`; `HA inbox list --unacked`; then close `A` (`agtermctl session close --target A`) | the bot message starts `🤖 probe · <host>`; the reply is an unacked inbox item `tg-<id>` with text `got it` |
| E13 | `up` | `H config set FOO 1; H service restart hub`; `grep -c 'unknown setting FOO' /tmp/hm-s0/home/.local/state/hommy-test/hub/err.log`; `H config set HUB_LISTEN not-an-address; echo $?`; `H config unset FOO` | `≥ 1`; the set is refused, exit 2, naming `HUB_LISTEN` |
| E14 | `P` | `nc -z 127.0.0.1 4713; echo $?`; `H config set MINIAPP_LISTEN 127.0.0.1:4713; H config set MINIAPP_URL https://example.invalid`; `curl -s -o /dev/null -w '%{http_code}' -H "X-Telegram-Init-Data: $(T initdata --user owner)" http://127.0.0.1:4713/api/board`; the same with `--user other`; `H config unset MINIAPP_LISTEN; H config unset MINIAPP_URL` | first `1` (nothing listens); `200` for the owner; `401` for the other user |
| E15 | `VP` | `H hub host-key add h2` (→ key `K2`); `scripts/test-env.sh up --instance test-h2 --join` (a second scratch HOME, no hub, no fakes); `printf %s K2 \| scripts/hm.sh --h2 setup --join ws://127.0.0.1:4710/ws --name h2 --key-stdin --non-interactive --vault /tmp/hm-s0-h2/vault`; `curl -s -H "Authorization: Bearer $(scripts/test-env.sh host-key)" -H "X-Hommy-Host: $(H config get HOST_NAME)" http://127.0.0.1:4710/api/status`; `T send text "/target vault@h2" --wait reply`; `T send text "echo: from h2" --wait reply --timeout 60`; `scripts/observe.sh jobs \| tail -1`; teardown `scripts/hm.sh --h2 service uninstall --purge; scripts/test-env.sh down --instance test-h2` | both hosts `online`; reply `from h2`; the last job ran on `h2`; after teardown no `dev.hommy.test-h2.*` label is loaded |
| E16 | all rows done | `H service uninstall --purge`; `launchctl list \| grep -cF dev.hommy.test.`; `ls /tmp/hm-s0/home/.config/hommy-test 2>/dev/null \| wc -l`; `scripts/test-env.sh down --purge`; `scripts/live-guard.sh check` | `0`; `0`; guard `ok` (the maintainer's live labels, files and ports unchanged since the snapshot) |
| E17 `tg=real claude=real` | the real test bot (`hommy-test-bot-token` exists, testing.md §12); `scripts/test-env.sh up --tg real` (writes the real token into the instance item, `TELEGRAM_API_URL` default); `S`; `T` with `TG=real` | E2's owner pairing (`/start K` from the maintainer's account), E3 on a `vault@` target, and E5's voice note | the same Then as E2's owner part, E3 and E5, observed through `telegram-agent-cli e2e read`; cleanup as E16 |

**Unit:** config validation table; instance path table; pairing (TTL, single use, per-user 5 wrong codes, case/dash
folding, group chat, other user, unpaired hub sends nothing); secret round trip on a scratch keychain; plist rendering
and "only changed plists reload"; `--purge` removes only its own files; doctor check table with fakes; every default
URL is loopback; typed prefix and relay regex agree; `lang_code` mapping; the run environment keeps auth variables.

## Done
E1–E17 green; `docs/protocol.md`; README intro + Setup; CI green.

## Risks
- The copy is large: keep the source's behaviour and tests; change only what the coupling list and this spec name.
  At the 4 h timebox, leftovers go to `Quarantined / follow-ups` per the flow.
- agtermctl finds agterm through `HOME`: the test env's socket link (01) is required for every agterm row.
- Real-Claude rows cost usage: E5, E10, E12 (one session) and E17.

## Implementation notes
- Deviation (E4, how tested): the hub answers a voice note first with the source's `🎤 (<endpoint>) <transcript>`
  line (kept: the owner sees what was heard), so the row waits three times. Evidence: implement run, e4.txt.
- Deviation (E5, how tested): `agtermctl session list --json` does not exist in agterm 0.43; the README's one-liner
  (`window list` + `tree --json --window`) finds the workspace's sessions.
- Deviation (E10, how tested): hook log lines now carry `session=<agterm id>`, which is what makes "lines for `A`"
  checkable; the hook log had only a pid before.
- Deviation (E15, how tested): host-key auth on the hub API is the source's (bearer + `X-Hommy-Host`), so the curl
  names the host.
- E7: the source sends `🟢 <host> is back` only after a `🔴 silent` alert; hommy also sends it after an
  `offline; queued` notice (the row's expectation; the owner was told the host is offline).
- `wait --for reply` (fake) returns the next bot message the tester has not seen (README › Telegram); the fake keeps
  its ids across restarts. Both found in the implement run: a fast stub answer raced the wait, and a restarted fake
  reused message ids the hub had stored (the new message was dropped as a duplicate). `scripts/tg-real.sh` (E17) does
  the same: it keeps the newest id the tester saw in `/tmp/hm-s<N>/run/tgreal.seen` and passes it as `--since`.
- Deviation (E6, how tested): the hub runs the stub as `CLAUDE_BIN=/tmp/hm-s<N>/bin/claude-stub` (a link to
  `bin/hommy-e2e`), so `pgrep -f 'hommy-e2e fakeclaude'` matched nothing even with a stub alive; the row counts
  `pgrep -f /tmp/hm-s0/bin/claude-stub` (its own slot's stubs only).
- Deviation (E16, how tested): `grep -c dev.hommy.test` also counted other slots' `dev.hommy.test-s<N>.` agents; the
  row counts its own slot's prefix only (`dev.hommy.test.` for slot 0, `dev.hommy.test-s<N>.` for slot N).
