# Clean room (phase 06)

A stranger's install on a fresh Mac: a new clone of the cached `golden-gate-vanilla` image in Tart, one command, the
real test bot, a real voice note and a real errand. Driver: `scripts/clean-room.sh` (commands: `e2e/README.md` ›
Clean room). Every failure was fixed in the repo and the run repeated from a fresh VM; nothing was fixed by hand in a
VM. All fixes land in this phase's commit (`phase 06: clean room`).

- **Date:** 2026-11-18. **Image:** `ghcr.io/cirruslabs/macos-golden-gate-vanilla:latest@sha256:b35695ce…a873`
  (macOS 27.2 arm64; VM disk grown to 80 GB). **hommy:** `0.0.1-dev` (snapshot), upgrade to `0.1.1-dev`.
- **Install command** (the README's unattended form): `./install.sh --source artifacts --non-interactive
  --token-stdin`, the real test bot's token on stdin.
- **Non-user steps** (done by the harness, each logged with a timestamp): the VM's ssh key and keychain unlock;
  `~/.claude.json` onboarding done + the Claude test token in the VM's environment (stands in for the user's own
  sign-in); agterm's two first-run answers (Welcome alert, Sparkle update prompt; a real Mac's first `hommy setup`
  waits for the user on both); `CR place` (the booking fixture inside the VM on `127.0.0.1:4715` and its row in
  `context/places.md`).

## Runs

Seconds per stage (`timings.tsv`); `pair-wait` is the time setup waited at pairing for the tester's `/start`.

| Stage | run 1 (08:29) | run 2 (08:37) | run 3 (09:12) | run 4 (10:07) | run 5 (10:22, E5) |
|---|---|---|---|---|---|
| artifacts | 5 | 5 | 9 | 5 | 4 |
| vm-create | 33 | 38 | 38 | 43 | 38 |
| copy | 11 | 2 | 1 | 2 | 2 |
| 1 preflight | 2 | 2 | 2 | 2 | 2 |
| 2 Homebrew (incl. Command Line Tools) | 136 | 133 | 139 | 130 | 133 |
| 3 Brewfile | 126 | 131 | 135 | 131 | 124 |
| 4 mlx-audio | 17 | 11 | 20 | 18 | 17 |
| 5 Claude Code | 19 | 21 | 20 | 19 | 18 |
| 6 hommy | 5 | 5 | 6 | 5 | 5 |
| 7 hommy-browsers | 18 | 21 | 19 | 19 | 18 |
| 8 optional | 0 | 0 | 0 | 0 | 0 |
| 9 hommy setup (whisper model + warm-up) | failed (F1) | 147 | 146 | 142 | 146 |
| pair-wait | — | 37 | 583 ¹ | 2 ² | 4 ² |
| **total** | — | **553** | **1117** | **517** | **510** |

¹ the tester's `/start` waited on the shared Telegram account lock (F4), not on hommy. ² `CR run` pairs by itself.

Runs 1–3 found F1–F7 (below); run 4 is the first with every fix, and its VM carried E6–E8. Run 5 is E5: the whole
flow again from a fresh VM, no step by hand. Host disk: 139 GB free before run 1, 134 GB free after the last VM was deleted (the slot-0 scratch root, 5.1 GB, and `.dev/`, 0.9 GB, account for the difference).

## Rows

| Row | Result | Evidence (`.dev/clean-room/<run>/`) |
|---|---|---|
| E1 | PASS (runs 2–5): the one command exits 0; `timings.tsv` one line per stage; total printed | `timings.tsv`, `install-first.txt` |
| E2 | PASS (runs 2–5): doctor exit 0, all ✓ incl. speech-stack, plugin, browsers, retainer, claude-auth; whatsapp, telegram-user `!` | `doctor-e2.txt`, `doctor.txt` |
| E3 | PASS (runs 2–5): `✅ Paired with @<test bot>…`; `pair --status` → `paired` | `tg-pair.json` |
| E4 | PASS (runs 3, 4, 5): 🎯 → 🎤 transcript → 🆕 (its `brief.md` = the transcript) → `Book`/`Cancel` naming 2026-11-19 10:30 → press → exactly one `POST /bookings` 2026-11-19 10:30 → text `Booked … BX-1` → voice note; `log.md` `## [2026-11-18] errand \| booked Barbearia Exemplo haircut 2026-11-19 10:30 (ref BX-1)`; `browser-reaper --json` `[]`. Run 2 failed here (F2, F3) | `tg.jsonl`, `fixture-log.jsonl`, `brief.md`, `vault-log-tail.txt`, `reaper.json` |
| E5 | PASS (run 5, after F1–F7 were fixed): `CR run` from a fresh VM, no step by hand besides the recorded non-user steps; E1–E4 as above (install 510 s, doctor exit 0, paired, one `POST /bookings`, text + voice, reaper `[]`) | run 5 |
| E6 | PASS (run 4): `install: 0 changes`; 12 launchd jobs, same PIDs and run counts. Run 3 failed (F5) | `install-rerun1.txt`, `rerun1-services-*.txt` |
| E7 | PASS (run 4): `[8/9] optional: wacli installed, shfmt installed, telegram-agent-cli installed` (`openclaw/tap`, `homebrew/core`, `alexdobrushskiy/tap`); doctor ✓ both | `install-rerun2.txt`, `doctor-e7.txt` |
| E8 | PASS (run 4): stage 6 `upgraded (0.0.1-dev → 0.1.1-dev)`; `hommy version` 0.1.1-dev; hub and host runs 1 → 2, speech and browsers untouched; config, vault, Keychain items unchanged; doctor ✓; `/target vault@…` then `Reply with ok` → `ok`. Run 3 failed (F6, F7) | `upgrade-*.txt`, `doctor-e8.txt`, `e8-ok.json` |
| E9 | this file | |
| E10 | PASS: `.dev/tools/tart list` shows only the cached image (OCI rows); `live-guard: ok` | |

## Failures

| # | Run | Class | What happened | Root cause | Fix |
|---|---|---|---|---|---|
| F1 | 1 | product | `setup: token: usage: --token-stdin got no token`: setup exited 2 | an install stage before setup (a child of stages 2–8) read the caller's stdin and swallowed the token | `install.sh`: with `--token-stdin` stages 2–8 run with stdin from `/dev/null`, setup reads fd 3; test `TestInstallTokenStdinReachesSetup` |
| F2 | 2 | harness | the tester's next `wait --for reply` never saw the Book/Cancel question | a real reply wait returns `.message` plus every reply that landed meanwhile in `.messages`; `tg-real.sh` advanced its seen id to the max of `.messages`, so the next wait skipped them | advance to the returned `.message` only; `TestTgRealReplyWaitStartsAfterTheLastSeen` |
| F3 | 2 | product | after the Book press the fixture got exactly one `POST /bookings`, but the answer never reached Telegram (`hook.log`: `stop: local turn`) | the hub types a pressed button as `[Telegram · tg-<msg>-b<seq> …]` (a reaction `-r<seq>`); the hook's `telegramRe` took only `tg-<digits>` followed by `]`, space or `·`, so the turn counted as local | `internal/hook/relay.go` accepts `-b<n>`/`-r<n>`; `TestTelegramMsgID` |
| F4 | 3 | harness | `/start` refused `lock_busy`; another project on the maintainer's account got `lock_busy` from our waits | `telegram-agent-cli` has one lock per account; a long `wait` held it for its whole timeout | `tg-real.sh` reply waits in ≤ 30 s slices after one anchor id, lock released between them, a `lock_busy` slice retried; `send … --wait reply` = send + sliced wait; other kinds one wait (review r1); `TestTgRealWaitSlicesTheLock` |

| F5 | 3 | product | E6 re-run: `setup: 3 changes`, hub and host restarted | with `--token-stdin` setup stored the same bot token again and flagged the secrets changed | an unchanged token is kept (`✓ bot @… (token kept)`); `TestTokenStdinSameTokenIsKept` |
| F6 | 3 | product | E8 upgrade: stage 6 stayed `present (0.0.1-dev)` with 0.1.1-dev artifacts | `brew outdated` exits 1 when a cask *is* outdated; under `pipefail` the `brew outdated \| grep` test was false exactly then | read its output, not its status; `TestInstallLocalTapUpgrade` |
| F7 | 3 | product | the local tap's cask pointed at the older archive | `local_tap` took the first `hommy_*` archive in `dist/` | the archive of the cask's own version; `version_of` tolerates a missing binary; `TestInstallLocalTapUpgrade` |

Also fixed on the way: stage 8 listed the extras `,`-joined (`wacli installed,shfmt installed`); now `, `
(`TestInstallOptionalLine`).

## Over-engineering review

One read of the tree (Go packages, scripts, plugin, template, installer); each removal proven unused by `deadcode`
(`x/tools`, with and without tests) and `git grep` over code, docs, scripts, CI, plugin and template.

**Removed** (−492 lines; re-proved by `go test ./...`, smoke 13/13, the hook probe and this run's E2–E8):

- `internal/hook/jsonobj.go`: the Codex hook installer's settings editor (D-9: not carried over).
- `hook.ConfigPath` / `LoadConfig` / `Config.Mode`: read the source's `hook.env`; the hook's config comes from the
  instance config.
- The Codex hook path: `hommy hook --agent`, `hook.AgentCodex`, `codexFinalText` (nothing installs Codex hooks; D-9,
  design D7).
- `internal/claude` `Run`/`runOnce`/`Options`: the non-streaming `claude -p --output-format json` path; `run_p` uses
  `claude.Stream`.
- The hub's `done_ledger` table: written, never read (`jobs.event_id UNIQUE` gives per-event idempotency; the I9
  ledger is the host's). An existing database keeps the unused table.
- `agentcli` `TELEGRAM_ENV_FROM` / `TELEGRAM_TOKEN_FILE`: `send.py` leftovers (D-9), the second a plaintext-token
  path (against I4).
- Unused symbols: `envfile.Load`, `agterm.RowMatches`, `agterm.ErrBlocked`, `Store.Hosts`, `fakespeech.ReplyOgg`,
  `browsers.DefaultDebugBase`, `inflightJob.jobID`.

**Kept** (looked at):

- Codex screen parsing in `internal/agterm` (+ its test screens): `AGENT_COMMAND` lets an owner run any agent in a
  session (master §3; 02-E5).
- Accessors only tests read (`Queue.Items`, `Running`, `SetHookSessions`, `Config.WithEnv`, …): small test seams.
- `hommy send --env-file` / `$HOMMY_SEND_ENV`: documented (protocol.md §10), the agentcli tests' seam.
- Every `scripts/*` file, config key, CLI command and flag: each is named by a spec row, the README, `e2e/README.md`
  or CI.
- Plugin skills, guards and hooks; every vault template file (master §5–§6, 04 rows).
- `internal/doctor/kit.go` vs `internal/setup/kit.go`: checks vs actions, not duplicates.
