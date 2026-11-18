# 04 · Plugin and vault template

**Goal:** the kit's Claude Code surface and memory, built while 02 runs: the `hommy` plugin and its marketplace
(skills, hooks, shims), the vault template with three recipes and its lint, and the booking fixture. In a scratch HOME
the plugins install and load, and an agent in the template vault follows the routing table to the booking recipe and
drives a lane browser to the confirm step without submitting. Wiring into `hommy setup`/`doctor` and the
answer-through-the-plugin-hook rows are 05's (master §14 D-15).
**Depends on:** 01 (harness), 03 (browsers engine and plugin). **Runs in workslot 2** (`scripts/slot.sh claim
plugin-vault --slot 2`) while 02 holds slot 0. **Master:** §5, §6, §14 D-3…D-6, D-15, D-22.

## Scope
- **In:**
  - `.claude-plugin/marketplace.json`, `plugin/` (master §5): `plugin.json`, `hooks/hooks.json`, `bin/hommy-hook`
    (master §4.5), `bin/agterm-status`, `bin/wacli-guard` (PreToolUse on Bash/Monitor, from the source's guard: read-only
    `wacli` commands pass, any other `wacli` invocation gets a forced `ask` so the user approves that exact command;
    commands parsed with `shfmt`, which `install.sh --with-whatsapp` installs; without `wacli` on PATH it passes
    everything), skills `notify`, `whatsapp`, `telegram`, `shopping-research` (from `SRC_SKILLS`: rules kept, every
    owner name, phone, account, group, city, shop and family reference removed; `notify` documents `hommy send` with
    `--agent`, `--buttons`, `--ask`; shopping-research ships to "the user's address in `context/places.md`").
  - `vault-template/` (master §6, incl. `embed.go`); `scripts/vault-lint.sh <vault>`: every routing-table path
    exists, `index.md` lists every non-README file, no folder is empty, every `context/` file starts with `## TL;DR`;
    `scripts/vault.sh init <dir>` (copies the template tree, minus `embed.go`, into an empty dir; 05 adds the real
    `hommy vault` command) and `scripts/vault.sh add-place <vault> <name> <url>` (a row in `context/places.md`).
  - Booking fixture (`hommy-e2e booking-site --port P --log FILE`): "Barbearia Exemplo" (invented). `/` business page
    with a "Book" link; `/book` lists the next 14 days (from the site's clock) × slots 09:00–18:00 every 30 min;
    `/confirm?service=haircut&date=YYYY-MM-DD&time=HH:MM` shows the summary and a "Confirm booking" button;
    `POST /bookings` records the booking and shows `Booked ✓ ref BX-<n>`. Every request is one JSON line in the log
    (`method, path, query, form`). `scripts/booking.sh up|down|log|reset`.
  - Harness: `scripts/plugins-env.sh up --slot N` (its own script, so 02's edits to `test-env.sh` never conflict)
    installs `hommy-browsers` (from its `origin/main`) into the scratch HOME with layout A ports and pool 0, and the
    plugins of this tree (`hommy`, `hommy-browsers`, `retainer`) through `SX claude plugin …`; README sections
    "Plugins", "Vault", "Booking site".
- **Out:** `hommy vault`, setup steps 9/11, the doctor checks `plugin`/`browsers`/`retainer`, answers through the
  plugin's hooks (05); the public marketplace URL (07).

## Interfaces and data
- `context/places.md` rows: `| Name | Kind | Booking | Notes |`, e.g. `| Barbearia Exemplo | barber | <url> | — |`.
- Recipe contract (`prompt/book-local-service.md`): find the business in `context/places.md` (else search, and ask
  before saving a new row); open the booking page in a lane (`pw-lane`); pick the requested service, date and time
  (resolve "tomorrow" with `date`); stop on the confirmation page; ask the owner with `hommy send --buttons
  "Book|Cancel" "<business> <date> <time>?"` (or, in a session not started from Telegram, as the final question);
  submit only after "Book"; then append one line to `log.md`, close the lane, answer with a `<tts>` block.

## Tasks
1. Marketplace, plugin, hooks, shims; skills de-personalised.
2. Vault template, lint, recipes.
3. Booking fixture; harness `--plugins`; README sections; prove every row.

## E2E cases
Definitions: slot 2, layout A (testing.md §2); `SX` = `scripts/sx.sh --slot 2`; `up` = `scripts/test-env.sh up --slot
2` then `scripts/plugins-env.sh up --slot 2`; `V` = `/tmp/hm-s2/vault` (from `scripts/vault.sh init`); `B` = `scripts/booking.sh --slot 2` (site on
port 4735); `CS` = `scripts/claude-session.sh --slot 2` (real Claude, testing.md §6). Every row ends with
`scripts/live-guard.sh check` → `ok`.

| # | Given | When | Then |
|---|---|---|---|
| E1 | clean tree | `SX claude plugin validate --strict .`; `SX claude plugin validate --strict plugin` | exit 0 both |
| E2 | `up` | `SX claude plugin list --json`; `SX claude plugin details hommy@hommy`; `grep -c 'hommy-hook\|hommy hook' /tmp/hm-s2/home/.claude/settings.json` | `hommy@hommy`, `hommy-browsers@hommy-browsers`, `retainer@retainer` enabled; details list skills `notify`, `whatsapp`, `telegram`, `shopping-research` and hooks for SessionStart, UserPromptSubmit, Stop, Notification (+ the agterm status and guard hooks); `0` (no `settings.json` wiring) |
| E3 | `up` | `echo '{}' \| env -i HOME=/tmp/none PATH=/usr/bin:/bin plugin/bin/hommy-hook stop; echo $?`; the same with a slow fake `hommy` on PATH that sleeps 30 s (README "Plugins" gives it), timed | no output, exit 0; then no output, exit 0 in under 5 s |
| E4 | `up` | `scripts/vault.sh init V`; `scripts/vault-lint.sh V`; `scripts/vault.sh init V; echo $?`; `grep -c 'book-local-service\|email-triage\|find-and-buy' V/AGENTS.md` | lint `ok`; second init exit 3 (`not empty`); `3` (each recipe routed) |
| E5 | `B up` | `curl -s 127.0.0.1:4735/ \| grep -c 'Barbearia Exemplo'`; `curl -s "127.0.0.1:4735/confirm?service=haircut&date=$(date -v+1d +%F)&time=10:30" \| grep -c 'Confirm booking'`; `B log` | `≥ 1`, `1`; the log has the two GETs and no POST |
| E6 `claude=real` | `up`; E4's vault; `scripts/vault.sh add-place V "Barbearia Exemplo" http://127.0.0.1:4735/`; `B reset` | `CS -p --cwd V --stream /tmp/hm-s2/e6.jsonl "Book a haircut at Barbearia Exemplo tomorrow at 10:30."`; `B log`; the transcript queries of README "Real Claude" on `/tmp/hm-s2/e6.jsonl` | the transcript reads `prompt/book-local-service.md` before its first browser tool call; its browser calls are the `pw-lane` tools of hommy-browsers; the site log has `GET /confirm` with `date=<tomorrow>` and `time=10:30` and **no** `POST /bookings`; the final answer asks to confirm that slot; `browser-reaper --json` lists no test Chrome afterwards |
| E7 | clean tree | `scripts/leak-scan.sh plugin vault-template .claude-plugin`; `scripts/test-env.sh down --slot 2 --purge`; `scripts/live-guard.sh check` | `0 hits`; `ok` |

**Unit:** shim exit 0 on every failure path (missing binary, bad instance, slow host); `vault.sh init` refuses a
non-empty dir; template lint on a broken copy; booking site date window and log format.

## Done
E1–E7 green; README sections; CI green.

## Risks
- E6 depends on the model following the routing table: the recipe and `AGENTS.md` wording are the product here; a
  flaky E6 is fixed in the wording, never by loosening the row.
- Plugin hooks and a user's own `settings.json` hooks both fire: hommy never writes hook entries into `settings.json`;
  the README warns users who wired the source bridge's hooks by hand to remove them.
