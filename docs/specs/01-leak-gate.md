# 01 · Leak gate, skeleton, test harness

**Goal:** from here on nothing personal can enter `hommy` (blobs, path names, commit headers or messages) without
being stopped at commit, push, lane commit, ship and CI; the repo has its skeleton and green CI; the test harness every
later phase drives exists. The specs (`docs/specs/`, `docs/testing.md`) are committed with this phase.
**Depends on:** — (repo, `main` and the `pre-push` hook exist from hm-setup). **Master:** I1–I4, §9, §10, §14 D-1,
D-18, D-19.

## Scope
- **In (app):**
  - Skeleton: MIT `LICENSE` (copyright line with the GitHub owner's name, S1), `README.md` stub (one paragraph + "not
    released yet"), `go.mod` `github.com/AlexDobrushskiy/hommy` (Go pinned), `cmd/hommy` printing `version` only (02
    brings the bridge), `docs/flaky.md`.
  - Leak gate (master §9): `scripts/leak/leak_scan.py`, `scripts/leak-scan.sh`, `scripts/leak/images.txt`,
    `scripts/leak/canary.sh` (`print-word`, `print-phone`, `print-token`: build the canary strings and a random
    bot-token-shaped string at run time from parts; never written to a file by themselves), `.githooks/pre-commit`,
    extended `.githooks/pre-push`, `.github/commit-identities`, `.gitleaks.toml`; in this clone `core.hooksPath
    .githooks`, `hommy.leak.required true`, `user.email` = the noreply address.
  - `scripts/leak-probe.sh commit|push` (for testers): clones `HEAD` into `/tmp/hm-leakprobe-<pid>/` with a local bare
    remote (`file://`), copies the hooks and config, plants the canary through `canary.sh` and tries the commit or the
    push there; prints the hook's output and the result; deletes the probe dir. It never writes to the repo under
    test or to GitHub.
  - CI (`.github/workflows/ci.yml`, `runs-on` from `vars.CREW_RUNS_ON`, default `macos-latest`; S7): jobs `go`,
    `shellcheck`, `leak` (scanner, gitleaks config and identities list taken from `origin/main` when present; the
    identifiers from secret `LEAK_IDENTIFIERS` into a 0600 temp file, never echoed), `identities` (also on every push
    to `main`).
  - Pinned tools: `scripts/tools.sh ensure <tool>` downloads gitleaks, shellcheck, Tart and GoReleaser release archives
    (versions + SHA-256 in `scripts/tools.env`; Tart pinned to 2.42.0, the version the overseer's image pull uses) into
    `.dev/tools/`; CI uses the same versions. Nothing is installed
    with the build Mac's Homebrew.
  - Harness (`cmd/hommy-e2e`, `e2e/`, `scripts/`; testing.md):
    - `hommy-e2e faketg` (§4), `fakespeech` (§5), `fakeclaude` (§6; also answers `--version` and `auth status`),
      `static-site` (serves `e2e/fixtures/static/`).
    - `scripts/sx.sh [--slot N] [--agterm-session ID] <cmd…>` (master §10: `env -i` + allowlist, scratch `HOME`,
      fixed `PATH` with links to the real `claude`, `uv`, `node`, `npm`, `git`, `HOMEBREW_NO_AUTO_UPDATE=1`; refuses to
      run when it cannot resolve the real `claude` by absolute path).
    - `scripts/test-env.sh up|down|status|home [--slot N] [--layout browsers] [--no-start] [--purge]`: scratch root
      `/tmp/hm-s<N>/`, scratch keychain, the agterm socket link, onboarding seed, test secrets copied in (testing.md
      §3), ports check, fakes started detached; the test instance's config is written by 02.
    - `scripts/tg.sh` (testing.md §4, incl. `TG=real` passthrough), `scripts/speech.sh`, `scripts/claude-session.sh`
      (testing.md §6), `scripts/mcp-call.sh <port> <tool> <json>`, `scripts/live-guard.sh snapshot|check`,
      `scripts/grep-tree.sh <dir|files…> [--exclude NAME]` (reads needles from stdin, prints only `<n> files`),
      `scripts/dev-build.sh` (`bin/hommy`, `bin/hommy-e2e`), the kit's workslots (master §14 D-22): `scripts/slot.sh
      status|list|claim SLUG [--slot N]|release N` (a git worktree of `origin/main` on its own branch under
      `$SLOTS_DIR/slot<N>`, slot ports and `/tmp/hm-s<N>/`; ported from the Books app's `scripts/slot.sh`, trimmed to
      what hommy needs), `docs/workslots.md`, `scripts/dev-up.sh [--restart]` / `scripts/dev-down.sh` (wrappers of
      `test-env.sh` for the flow's cold run), `scripts/smoke.sh`, `scripts/ci-preflight.sh` (gofmt, vet,
      `go test` through `sx`, shellcheck, `leak-scan.sh --self-test`, gitleaks), `scripts/ci-probe.sh`, `scripts/lib/detach.py`.
    - Fixtures: `e2e/fixtures/audio/{hello,book-haircut}.ogg` (synthetic speech from macOS `say` + ffmpeg, Ogg Opus)
      with `.txt` transcripts ("hello", "book a haircut at Barbearia Exemplo tomorrow at ten thirty").
    - `e2e/README.md` › "Start here", "Telegram", "Speech", "Real Claude", "Leak gate".
- **In (workspace, private):** `ops/leak-scan.sh` (wrapper: runs the scanner from `origin/main`, fallback the working
  tree; prints `scanner: origin/main <sha7>` or `scanner: worktree`; `--root` for another repo), `ops/leak_patterns.py`
  (kit CLI: `scan`, `check`, `allow N`), `ops/leak-gate-hook.py` (lane commit gate, adapted from Books),
  `ops/test_leak.py`; the S3 switch `WS_LEAK_GATE=0` in `ops/pipeline.env`, read at the points `ops/phase-notes.md` ›
  hm-01 lists, and `tools/ws-sync.py` added to `ops/kit.keep` so a kit update cannot undo it; the canary lines
  appended to the identifiers list (silently, `grep -q` first) and `gh secret set LEAK_IDENTIFIERS` for this repo;
  `ops/claude-token.sh import|check` (testing.md §6: the test token into the build Mac's Keychain item
  `hommy-test-claude-token`, never printed; an ops step, not a test).
- **Out:** the bridge code (02); `hommy-browsers`' gate (03 copies the scanner); the tap and `refs/pull` scans (07).

## Interfaces and data
- `scripts/leak-scan.sh`: master §9 modes, output and exit codes; last line `leak scan: <files> files, <commits>
  commits, <hits> hits (identifiers: <n> | skipped)`.
- `--self-test` (temp list + runtime probes in a temp dir, deleted after): canary word in lower, upper, mixed case and
  NFD form (hit); glued to letters (no hit); between punctuation (hit); in a path name (hit, path not printed); the
  canary phone with spaces, dashes, parentheses and `+` (hit) and one digit changed (no hit); inside `.zip`, `.docx`
  (split across runs), `.gz` and a SQLite file (hit); an unlisted `.png` (hit); a commit message (hit); a foreign author
  email (identity hit); a bot-token-shaped string (gitleaks hit). Prints `self-test: PASS (<k> cases)` or the failing
  case names; never a probe's content.

## Tasks
1. Scanner + self-test + unit tests; hooks; identities; clone config; `leak-probe.sh`.
2. Workspace wrapper, lane gate, `allow`, S3 switch + `kit.keep`, canary lines + Actions secret.
3. CI workflow (judge from `origin/main`); tools; ci-preflight; ci-probe.
4. Harness binaries, scripts, fixtures, README; test token import.

## E2E cases
Definitions: run from `app/` in a fresh `zsh -l`; `C` = `scripts/leak/canary.sh print-word` (only ever inside
`$(…)`, never echoed); `L` = `scripts/leak-scan.sh`; `LP` = `scripts/leak-probe.sh`; slot 0; "clean tree" = the
bound tree with no local changes. The tester makes no git write in the repo and no push (flow › e2e).

| # | Given | When | Then |
|---|---|---|---|
| E1 | clean tree | `L`; `L --staged`; `.dev/tools/gitleaks detect --no-git -s .` (`scripts/tools.sh ensure gitleaks` first) | `0 hits`, exit 0 twice; gitleaks exit 0 |
| E2 | clean tree | `LP commit 2>&1 \| tee .dev/lp1.txt`; `grep -c "$(C)" .dev/lp1.txt` | the probe commit was refused by the pre-commit hook naming `docs/canary-probe.md:1: leak #<n> (canary)` and `result: refused`; `0` |
| E3 | clean tree | `LP push 2>&1 \| tee .dev/lp2.txt`; `grep -c "$(C)" .dev/lp2.txt` | the commit (made `--no-verify` inside the probe clone) was refused at push by the pre-push hook naming the file and `#<n> (canary)`, and also `commit <sha12>: message: leak #<n>` (the probe puts the canary in the message too); `result: refused`, the bare remote has no probe branch; `0` |
| E4 | the implementer's CI probe run on branch `ci-probe/leak-01` (pushed `--no-verify` with the probe file, a canary message and a commit by `Probe <probe@example.com>`; branch deleted after, the run stays) | `gh run list -R AlexDobrushskiy/hommy --branch ci-probe/leak-01 --limit 1 --json databaseId,conclusion`; `gh run view <id> --log-failed`; `gh run view <id> --log \| grep -c "$(C)"` | conclusion `failure`; job `leak` failed with `docs/canary-probe.md:1: leak #<n> (canary)` and `commit <sha12>: message: leak #<n>`; job `identities` failed with `commit <sha12>: author email not allowed`; `0` |
| E5 | clean tree | `L --self-test` | `self-test: PASS`, exit 0; no probe files left in the temp dir it names |
| E6 | clean tree | `LP commit --token 2>&1 \| tee .dev/lp3.txt` (plants `canary.sh print-token` instead of the word); `grep -cE '[0-9]{8,10}:[A-Za-z0-9_-]{35}' .dev/lp3.txt` | refused by gitleaks (rule id printed, secret redacted); the probe's own line `planted token in the hook output: 0`; the grep → `0` |
| E7 | workspace present | `../ops/leak-scan.sh .`; feed `../ops/leak-gate-hook.py` the PreToolUse JSON for `git commit -m x` in an `LP`-style probe clone with the canary staged (`LP pretooluse python3 "$PWD/../ops/leak-gate-hook.py"`; README "Leak gate"); write `$(C)` into `../lanes/hm-01-leak-gate/probe.md` (a synced workspace path) and run `python3 ../tools/ws-sync.py status`; `grep -c tools/ws-sync.py ../ops/kit.keep` | first prints `scanner: origin/main <sha7>` (or `scanner: worktree` before the first merge) and `0 hits`; the hook prints a `deny` decision naming `#<n> (canary)`; ws-sync status shows no `leak` hold (S3); `1`. Cleanup: delete the probe file |
| E8 | the bound tree's ci-probe run (`scripts/ci-probe.sh 01-r<round>` by the implementer) | `gh run list -R AlexDobrushskiy/hommy --branch ci-probe/01-r<round> --limit 1 --json databaseId,conclusion`; `gh run view <id> -R AlexDobrushskiy/hommy --json jobs -q '.jobs[] \| .name + "=" + .conclusion'`; `scripts/ci-preflight.sh` | `success`, with jobs `go`, `shellcheck`, `leak`, `identities`; preflight exit 0 |
| E9 | nothing listening on 4710–4719 | `scripts/live-guard.sh snapshot`; `scripts/test-env.sh up`; `scripts/sx.sh security list-keychains -d user`; `scripts/sx.sh env \| grep -c '^CLAUDE_CONFIG_DIR\|^CLAUDECODE'`; `scripts/test-env.sh down`; `nc -l 127.0.0.1 4711 & NC=$!`; `scripts/test-env.sh up; echo $?`; `kill $NC`; `scripts/test-env.sh status`; `scripts/live-guard.sh check` | `up` prints the slot's ports and `ready`, scratch root under `/tmp/hm-s0/`; the keychain list holds only the scratch keychain; `0`; the second `up` exits 3 naming port 4711; after the kill `status` says `down` and nothing listens on 4710–4719; `launchctl list \| grep -c dev.hommy.test` → `0`; guard `ok` |
| E10 | `scripts/test-env.sh up` | `scripts/tg.sh send text "hello" --from owner`; `curl -s "$(scripts/tg.sh bot-url)/getUpdates"`; `curl -s "$(scripts/tg.sh bot-url)/sendMessage" -d chat_id=1001 -d text=pong`; `scripts/tg.sh read messages --limit 2`; `scripts/tg.sh send voice e2e/fixtures/audio/hello.ogg`; then `getUpdates`, `getFile` and the file download with curl | the update carries `from.id 1001`, `chat.type private`, text `hello`; `read messages` shows `pong` from `bot`; the downloaded voice has the fixture's SHA-256 |
| E11 | `scripts/test-env.sh up` | `curl -s -F file=@e2e/fixtures/audio/hello.ogg -F model=whisper "$(scripts/speech.sh url)/v1/audio/transcriptions"`; `scripts/speech.sh set-transcript "Reply with pong"` then the same request; `curl -s "$(scripts/speech.sh url)/v1/audio/speech" -H 'content-type: application/json' -d '{"model":"m","input":"hi","voice":"af_heart","response_format":"opus","lang_code":"en"}' -o .dev/o.ogg; file .dev/o.ogg`; `scripts/speech.sh log` | `{"text":"hello"}`; then `{"text":"Reply with pong"}`; `Ogg data, Opus audio`; the log lists the calls with `voice` and `lang_code` |
| E12 | `scripts/test-env.sh down --purge`; clean tree | from a new `zsh -l`, run `e2e/README.md` › "Start here" verbatim (tools, build, `up`, `scripts/smoke.sh`, `down`) | every command runs as written; smoke passes on this first (cold) run; `head -1 LICENSE` is `MIT License` |
| E13 | workspace present; `scripts/test-env.sh up` | `../ops/claude-token.sh check`; `scripts/claude-session.sh -p --cwd /tmp/hm-s0 --stream .dev/cs.jsonl "Reply with the word ok."`; `jq -r 'select(.type=="result").result' .dev/cs.jsonl`; `scripts/claude-session.sh --audit .dev/cs.jsonl` | `ok` (the item exists; nothing printed); the run answers `ok`; the audit (which reads the token from the keychain itself and prints counts only) reports `token matches: 0` over the stream file, `.dev/` and the scratch root |
| E14 | clean tree | `scripts/slot.sh claim probe --slot 2`; `scripts/slot.sh status 2`; `git -C "$(scripts/slot.sh path 2)" rev-parse --abbrev-ref HEAD`; `scripts/slot.sh release 2`; `scripts/slot.sh status 2` | a worktree at `app-slots/slot2` on branch `slot2/probe` from `origin/main`, ports `4730`–`4739`, scratch root `/tmp/hm-s2/`; after release the worktree and branch are gone and the slot is `free` |

**Unit:** `sx` passes only the allowlist (a planted `CLAUDE_CONFIG_DIR`, `CLAUDE_CODE_X`, `ANTHROPIC_X` are absent);
boundary rule (Latin, Cyrillic, NFC/NFD, digits, underscore, punctuation), phone reduction, archive/XML/gz/
printable-run extraction, images list, path-name hits, commit header/message parsing, `--push` range for a new and an
existing branch, `origin/main` fallback; faketg method contracts (each Bot API method the source uses answers the
shape the Go client parses); fakespeech; stub modes incl. `--version` / `auth status`; `sx.sh` env scrub.

## Done
E1–E14 green; CI green on the PR; specs and `docs/testing.md` committed; README sections; the Actions secret set; the
overseer told to add `--author-email <noreply>` to the flow's ship merge (master §9) if it is not there yet.

## Risks
- A weakened scanner, identities list or gitleaks config in a phase diff: the workspace wrapper and CI judge with
  `origin/main`'s copies; review sees the diff.
- GitHub masks secrets per value, not per line: the scanner never prints matched text, so masking is not relied on.
- Images and PDFs are unreadable: each one needs a reviewed entry in `scripts/leak/images.txt`.

## Implementation notes

- Deviation (E6): the planted token is random and never stored, so the tester cannot `grep -c` it. `leak-probe.sh`
  counts it in the hook output itself (`planted token in the hook output: N`) and the row greps the token's shape.
- Deviation (E7): `scratch-shots/` is gitignored in the workspace, so a probe file there proves nothing about ws-sync
  (it never looks at it). The row uses a synced path, `lanes/hm-01-leak-gate/probe.md`; with the gate forced on the
  same file is held as `leak: 1 pattern(s): canary` (control run, implement r1). The lane-gate feed is
  `leak-probe.sh pretooluse <hook command>` (a mode added for this row: stages the canary in the probe clone and feeds
  the hook Claude Code's PreToolUse JSON for `git commit -m x`).
- Deviation (output, master §9): besides §9's formats the scanner prints `<path>: image not reviewed
  (scripts/leak/images.txt)` for an unlisted image/PDF and `commit <sha12>: header: leak #N` for a hit in an author or
  committer name/email; hits in binary content (printable runs, archive XML) carry line `0`. None prints a matched text.
- Review r1 (output, master §9): `--push`/`--history` also print `commit <sha12>: path name: leak #N` for every path a
  commit adds (renames of blobs the remote has included), `tag <sha12>: name|header|message: leak #N` and `tag <sha12>:
  tagger email not allowed` for annotated tags, and `<path>: too large to scan (> <bytes> bytes)` for a blob over 64 MB
  (a hit). A phone-shaped entry outside `phone` gets both the word and the phone rule; phone runs count from 6 chars.
- Deviation (matching, master §9): an identifier shaped like a phone number (`+`, digits and separators, ≥ 7) gets the
  phone rule in any category, so the `canary` category's phone line matches with any separators like a `phone` line.
- CI's leak and identities logic lives in `scripts/ci-leak.sh` (shellchecked, runnable locally) rather than inline
  YAML; it extracts origin/main's scanner, identities and gitleaks config into a temp dir and prints which judged.
- Deviation (E9): `security list-keychains` without `-d` also prints macOS's system-domain keychain
  (`/Library/Keychains/System.keychain`, always listed, not removable); the row reads the user domain (`-d user`),
  which holds only the scratch keychain.

- Deviation (tg=real driver): `scripts/tg.sh` is fake-only and never names the real CLI (a guard hook reading it made
  every fake call prompt); the passthrough is `scripts/tg-real.sh`, which takes the test bot only from `HOMMY_TEST_BOT`
  or `~/.config/hommy-test/test-bot`, always passes it as `--chat`, and refuses before any call when it is unset or not
  `@hommy_test_<6>_bot`, or on a fake-only verb/flag (overseer ruling 2026-11-18: the CLI's default chat is the
  maintainer's own bot). Its refusal paths are tested with a recording stub (`internal/e2e/harness/tgreal_test.go`).
- Deviation (E8): `gh run list` has no `jobs` JSON field (it errors); the row reads the jobs with `gh run view <id> --json
  jobs`.
- E4 note: `actions/checkout` prints the head commit's subject ("HEAD is now at … <subject>"), which carried the canary
  into the run log until every job first registered each identifier with `::add-mask::` (ci.yml); since then the log's
  count is 0 (run 44213795318).
