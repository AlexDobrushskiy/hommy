# 06 · Clean room

**Goal:** prove the product as a stranger meets it (slot 0; the VM re-run, extras and upgrade of 05 run here, master
§14 D-21): in a fresh macOS VM, one command → `hommy setup` → pairing with the
real test bot → a real voice note → a real errand (lane browser booking at the fixture site with the owner's "Book"
press, a vault write, a text and voice answer). Every failure is fixed in the repo and the run is repeated from a fresh
clone; nothing is fixed by hand in the VM. Then one over-engineering review: what can be removed. The run is recorded
in `docs/clean-room.md` with timings.
**Depends on:** 05. **Master:** §8, §10, I5, I6; testing.md §6–§8. The cached VM image comes from 05.

## Scope
- **In:** `scripts/clean-room.sh run [--until setup] | place NAME | rerun [extra install flags] | upgrade` (one
  driver: create VM `hommy-06` from the cached image, copy artifacts, run the
  install exactly as README's unattended form with the real test bot token fed on stdin, record per-stage timings, run
  the errand rows, collect logs to `.dev/clean-room/<run>/`, delete the VM); `docs/clean-room.md` (date, image, host
  disk before/after, per-stage timings, each row's result, every failure with its root cause and the commit that fixed
  it, the removal list); fixes found by the run (any phase's code; the small-fix path of the flow applies).
  **Over-engineering review:** one read of the whole tree (Go packages, scripts, plugin, template, installer) listing
  what can be removed without changing a behaviour some spec row or design decision needs; apply the removals that
  qualify, re-run the affected rows, list the rest with the reason they stay.
- **Out:** the public one-liner from GitHub (07); anything that needs the repos public.

## Interfaces and data
The booking fixture runs **inside** the VM (`hommy-e2e booking-site` from the artifacts, port 4715) and the vault gets
the place row from `scripts/vault.sh add-place` run over ssh — the only non-user step, recorded as such in
`clean-room.md`. Telegram is the real test bot (`tg=real`) driven from the maintainer's account with
`telegram-agent-cli e2e` on the build Mac (authorized: build plan › rulings); the VM reaches `api.telegram.org`
directly. Claude Code in the VM is signed in by the test token in the VM environment only, and its first-run
onboarding is marked done (testing.md §6): this stands in for the user's own sign-in and is the second recorded
non-user step.

## Tasks
1. Driver, recording, errand rows; run; fix in the repo; re-run from a fresh clone until green.
2. Over-engineering review and removals; re-run the touched rows.
3. `docs/clean-room.md`.

## E2E cases
Definitions: `CR` = `scripts/clean-room.sh`; `T` = `scripts/tg.sh` with `TG=real`; `VM` = `scripts/vm.sh ssh hommy-06 --`;
"fresh" = a new clone of the cached `golden-gate-vanilla` image. Rows E1–E4 run in one `CR run`; E6–E8 continue in that VM; E5 is a second full run.

| # | Given | When | Then |
|---|---|---|---|
| E1 | fresh VM; the real test bot token in `hommy-test-bot-token`; ≥ 60 GB free | `CR run --until setup` | the install's one command exits 0; `.dev/clean-room/<run>/timings.tsv` has one line per stage with seconds; total printed |
| E2 | E1 | `VM hommy doctor` | all `✓` (incl. `speech-stack`, `plugin`, `browsers`, `retainer`, `claude-auth`) |
| E3 `tg=real` | E1, setup waiting at pairing | `T send text "/start <code from the setup output>" --wait reply --timeout 60` | `✅ Paired…`; `VM hommy pair --status` → `paired` |
| E4 `tg=real claude=real speech=real` | E3; `CR place "Barbearia Exemplo"` (fixture site up, place row added) | `T send voice e2e/fixtures/audio/book-haircut.ogg` ("book a haircut at Barbearia Exemplo tomorrow at ten thirty"); wait for the `🆕` line, then for a message with buttons `Book` / `Cancel` naming tomorrow 10:30 (`T wait --for reply --timeout 600`); `T press --msg <it> --button Book --wait edit`; `T wait --for reply --timeout 600` (text), then the voice; `VM cat <fixture log>`; `VM tail -3 ~/hommy-vault/log.md` | the transcript in the `🆕` brief matches the voice note; the button message names the slot; after the press the fixture log has exactly one `POST /bookings` for tomorrow 10:30; `log.md` has a new line about the booking; the answer arrives as text then a voice note; no Chrome left in the VM after the session (`hommy-browsers/browser-reaper --json`) |
| E5 | E1–E4 green; at least one fix was committed during this phase | a second `CR run` from a fresh VM (whole flow, no manual step besides the recorded non-user steps) | E1–E4 green again; `clean-room.md` cites both runs. If the first run was green with no fix, this row is `PASS (not needed)` and `clean-room.md` says so |
| E6 | the first `CR run`'s VM after E4 | `CR rerun` (the same install command again) | `install: 0 changes`; the services' PIDs unchanged |
| E7 | E6 | `CR rerun --with-whatsapp --with-telegram-user`; `VM hommy doctor` | `wacli` (`openclaw/tap`), `shfmt` and `telegram-agent-cli` (`AlexDobrushskiy/tap`) installed; doctor shows them `✓` (E2 had shown them as `!`) |
| E8 `claude=real` | E7; an artifacts set with a bumped version | `CR upgrade`; `VM hommy version`; `VM hommy doctor`; `T send text "/target vault@<vm host>"`; `T send text "Reply with ok" --wait reply --timeout 300` | stage 6 `upgraded`; the new version; hub and host restarted once; config, vault and Keychain items unchanged; doctor `✓`; the `run_p` reply `ok` (real Claude with the token: the run keeps auth variables, master §3.2) |
| E9 | E8 | read `docs/clean-room.md` | timings per stage for every run; every failure with root cause, class (`product|spec|harness|env`) and fixing commit; the over-engineering list (removed: what and why; kept: why) |
| E10 | all rows done | `.dev/tools/tart list`; `scripts/live-guard.sh check` | no `hommy-*` VM; guard `ok` |

## Done
E1–E10 green; `docs/clean-room.md` committed; removals merged.

## Risks
- The real errand depends on the model and on Telegram: a red E4 is diagnosed from the logs (host, hub, hook log,
  fixture log, transcript) before any change; a wording fix in the recipe counts as a product fix.
- One VM at a time (Apple allows two; tests use one); the cached image is deleted by 07 after its last VM row.
- 07's prep runs in parallel with this phase (master §14 D-15): no change here may wait on 07.
