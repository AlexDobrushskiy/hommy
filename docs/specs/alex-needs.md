# Alex-needs (hommy, 2026-11-17)

Only what the maintainer alone can give. Every earlier item was ruled on 2026-11-17 (build plan › rulings), so **one
item is open: the public go.** Nothing here blocks a phase before 07's go step.

| # | Item | Status | Phase | How the phase runs |
|---|---|---|---|---|
| 1 | Test Telegram bot token | **ruled: agents create it** via BotFather from his account with `telegram-agent-cli`; token → Keychain `hommy-test-bot-token` | 02 (`tg=real` row E17), 04–07 | testing.md §12; until it exists, every row runs on the fake Bot API and E17 waits for it inside 02 |
| 2 | OK to send test messages and voice notes from his Telegram account to the test bot | **ruled: yes**, to the test bot only (plus BotFather for #1) | 02-E17, 06-E3/E4, 07-P2 | `telegram-agent-cli e2e` with the test bot allow-listed |
| 3 | Claude sign-in for tests (VM and scratch HOME) | **ruled: reuse the existing long-lived token** (private companion), env-only, never in a repo, log, transcript or surviving image | 02, 04, 05, 06, 07 | `ops/claude-token.sh import` (01); testing.md §6 |
| 4 | Apple Developer ID | **ruled: no** — self-signed at install | 02, 05 | master §4.8, §8.3 |
| 5 | Live cutover of his own setup | **ruled: none** — he is not switching; his setup is never touched | — | master I2 |
| 6 | **The public go** | **open**: after 07's pre-go rows (full-history scans clean), the overseer sends one `ops/tell-ceo.sh --ask` with the scan summary (counts, repos) | 07 | the lane ships with `ops/go-public.sh --go` ready; P1–P2 run after his OK |

**Not Alex items (decided by the build):** creating `hommy-browsers` (private), Actions secrets on both repos, the
self-hosted runner and its removal (S7), test bot allow-listing, VM images and their deletion.
