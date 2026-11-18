# 03 · hommy-browsers

**Repo:** `hommy-browsers/` in the build workspace = new private `AlexDobrushskiy/hommy-browsers` (own git repo, PRs to
`main` only, `pre-push` refusing `main`, the leak gate copied from 01). **Runs in parallel with 02** in its own workslot.
**Goal:** a clean first commit of the generic browser engine (design D8) from the private source (`SRC_BROWSERS`,
private companion), configurable for any user and for side-by-side test installs, with an idempotent installer and its
own Claude Code plugin; the private source repo, its remote and its clones stay byte-for-byte untouched.
**Depends on:** 01 (scanner, hooks, CI pattern). **Master:** I1–I3, §5, §7, §14 D-6, D-8.

## Scope
- **In:**
  - Create the repo (`gh repo create AlexDobrushskiy/hommy-browsers --private`); local clone at `$W/hommy-browsers`
    with `user.email` = the noreply address, `core.hooksPath .githooks`, `hommy.leak.required true`; the scanner,
    hooks, `.github/commit-identities`, gitleaks config and CI (`shellcheck`, `leak`, `identities`, `test`: the
    offline tests) copied from `hommy`; the Actions secret `LEAK_IDENTIFIERS` set.
  - From the source, **copied file by file, never `git clone`/`cp -r` of the repo** (no `.git`, no `sites/`, no
    venvs, no `node_modules`, no `plain-chrome.bak-*`): `serve`, `agent-chrome`, `real-chrome-ua.cjs` (its header's
    reference to another private repo removed), `pw-identity`, `plain-chrome`, `captcha-solve`, `captcha/` (code,
    `requirements.txt`, offline tests, the labelled public-demo grids; **not** `data/bench` screenshots of real pages),
    `browser-reaper`, `launchd/` templates, `config/` snippets, `package.json`/`package-lock.json`, `skill/` → the
    plugin. A **new** `creds` (`creds get|new|check <domain>`): Keychain service `<HB_LABEL_PREFIX>.creds.<domain>`,
    account = `HB_LOGIN_EMAIL` (no default; `new` refuses without it), password generated and stored only in the
    Keychain, never printed except `get --print` to a pipe.
  - Config (master §7): every fixed path, port, label, bundle id and node path in the inventory replaced by
    `config.env` keys; all runtime state under `HB_STATE_DIR` (profiles, pool, caches, logs); the reaper plist
    templated; `agent-chrome`'s refresh check matches only this install's Chrome (by bundle id), never another's;
    `pw-identity`'s port map from config; `plain-chrome` and `serve` honour the identity profile path; `agent-chrome
    --print-app` prints this install's app bundle path.
  - `install.sh [--with-captcha] [--no-launchd] [--uninstall]` (Bash): checks prerequisites (Chrome.app, node ≥ 20,
    npm, uv, ffmpeg; Ollama only with `--with-captcha`); `npm ci` only when the lockfile changed; builds Agent Chrome;
    captcha venv + models only with `--with-captcha` (never pulls otherwise); renders plists and reloads only changed
    ones; `pw-identity seed --all` only when a master profile exists; smoke test that **fails** the install when an
    enabled server does not answer an MCP `initialize`; prints a summary (`changed: <n>`).
  - Plugin (master §5): `.claude-plugin/marketplace.json` (marketplace `hommy-browsers`), `plugin/.claude-plugin/
    plugin.json` (`userConfig`: `lanes_port`, `identity_port`, `pool` numbers with the engine defaults),
    `plugin/skills/agent-browsers/SKILL.md` + `references/` (generic rules: lane vs identity vs plain-chrome, the
    escalation ladder, close what you open, identity hold/release, subagents, confirm before committing; **no site
    table rows, no accounts, no owner rules**; a pointer to `HB_SITES_DIR/<site>/README.md` for a user's own site
    notes), `plugin/hooks/hooks.json` (identity lease `pw-identity hook pre|post`), `plugin/.mcp.json`.
  - `README.md` (what, install, config keys, the plugin, runtime state, models), MIT `LICENSE`.
  - The test harness is `hommy`'s (built in 01): used from a **clean worktree of `hommy` at `origin/main`**
    (`git -C $W/app worktree add --detach $W/hommy-harness origin/main`, outside `app-slots/`, built there), never from slot 0 where
    02 is working: `scripts/test-env.sh up --slot 1 --layout browsers` (writes the test `config.env`), `scripts/sx.sh`,
    `scripts/mcp-call.sh`, `hommy-e2e static-site`. This phase changes nothing in the `hommy` repo.
  - `e2e/README.md` **in this repo** (the blind tester reads `{repo}/e2e/README.md`): the harness worktree, the
    commands of every row, the scratch install, and a "Maintainer-only rows" section for E1/E10, which call the
    workspace's `ops/src-record.sh`.
  - Workspace: `ops/src-record.sh snapshot|check` (records the private source's HEAD, remote URL, `status --porcelain
    --ignored`, a SHA-256 over the working-tree files (tracked and ignored, excluding nothing) and a marker file;
    `check` reports `unchanged` or the differing fields plus `find <src> -newer <marker>` counts, never paths of the
    source's ignored data); a second repo-scoped self-hosted runner for `hommy-browsers` and its `CREW_RUNS_ON`
    variable (S7; `ops/ci-runner.sh`; 07 removes both).
- **Out:** `sites/` (stays private, never copied); per-site helpers (`sites/_lib`); any paid proxy service; wiring into
  `hommy setup` and `hommy doctor` (04).

## Interfaces and data
- `config.env` keys and defaults: master §7. Labels `<HB_LABEL_PREFIX>.{lanes,identity,identity-<x>,chrome-refresh,
  identity-seed,reaper}`; test installs `dev.hommy.test-s<N>.browsers.*` and bundle id `dev.hommy.test-s<N>.agent-chrome`.
- `creds` output: `get` → exit 0 + `login` on stdout (password only with `--print`), exit 1 missing; `check` → `ok`.

## Tasks
1. Before anything: `scripts/live-guard.sh snapshot` and `ops/src-record.sh snapshot`.
2. Create the repo; copy the engine; parameterise; remove every personal and machine-specific item of the inventory.
3. `creds`, installer, plugin, README; leak scan clean; the first commit is the whole clean tree.

## E2E cases
Definitions: harness = the `hommy` worktree (README); slot 1, layout B (testing.md §2: lanes `4720`, identity `4721`,
identity-a `4722`, debug `4723`–`4725`, static site `4726`); scratch HOME `/tmp/hm-s1/home` from `scripts/test-env.sh
up --slot 1 --layout browsers`; `SX` = `scripts/sx.sh --slot 1`; `HB` = `SX /tmp/hm-s1/home/.local/share/hommy-browsers`
(a checkout of the bound tree), so `HB/install.sh` runs with the scratch HOME; `M` = `scripts/mcp-call.sh`.

| # | Given | When | Then |
|---|---|---|---|
| E1 | maintainer-only; the snapshot from task 1 | `gh repo view AlexDobrushskiy/hommy-browsers --json isPrivate`; `git log --max-parents=0 --format=%H main` and `git show --stat` of it; `../ops/src-record.sh check` | `isPrivate: true`; one root commit holding the whole engine; `unchanged` |
| E2 | the PR branch | `scripts/leak-scan.sh --history`; `scripts/leak-scan.sh .`; `git log --format='%ae %ce' \| sort -u` | `0 hits` both; only the noreply address and `noreply@github.com`; no `sites/`, no `.png` under `captcha/data/bench` |
| E3 | scratch HOME, no install | `HB/install.sh`; `HB/install.sh` | first exit 0, summary lists the created services; second exit 0, `changed: 0`, and `launchctl print` PIDs of every test service equal those after the first run |
| E4 | E3 | `launchctl print gui/$UID/dev.hommy.test-s1.browsers.lanes \| grep 'state ='`; same for `identity`, `identity-a`; `M 4720 initialize {}`; `defaults read "$(HB/agent-chrome --print-app)/Contents/Info" CFBundleIdentifier` | `running` ×3; a JSON-RPC result with `serverInfo`; `dev.hommy.test-s1.agent-chrome` |
| E5 | E3; `hommy-e2e static-site --port 4726` running | `M 4720 browser_navigate '{"url":"http://127.0.0.1:4726/"}'`; `curl -s 127.0.0.1:4723/json/version`; `M 4720 browser_snapshot {}`; `M 4720 browser_close {}`; after 30 s `HB/browser-reaper --json` | a `Browser` field (the lane's Chrome started on the first tool call); the snapshot shows the heading `hommy-browsers test page`; close ok; the reaper lists no Chrome of this install |
| E6 | E3 | feed `HB/pw-identity hook pre` the PreToolUse payload of `mcp__identity__browser_navigate` for session `s-A` (README gives the payload files), then for session `s-B`; `HB/pw-identity pool`; feed `hook post` for `s-A`'s `browser_close`; `hook pre` for `s-B` again | `s-A` allowed; `s-B` denied naming the holder `s-A` and listing `identity-a` free; `pool` shows `identity` held by `s-A`; after `s-A`'s close `s-B` is allowed |
| E7 | E3 | `SX claude plugin marketplace add /tmp/hm-s1/home/.local/share/hommy-browsers`; `SX claude plugin install hommy-browsers@hommy-browsers --config lanes_port=4720 --config identity_port=4721 --config pool=1`; `SX claude plugin list --json`; `SX claude plugin details hommy-browsers@hommy-browsers` | the plugin `enabled`; details list the `agent-browsers` skill, the lease hooks and MCP servers whose URLs use ports 4720/4721/4722 |
| E8 | config `HB_LOGIN_EMAIL=owner@example.test` | `HB/creds new shop.example.test`; `HB/creds get shop.example.test`; `HB/creds check shop.example.test`; `HB/creds get shop.example.test --print \| scripts/grep-tree.sh /tmp/hm-s1 --exclude Keychains` | `get` prints `owner@example.test`; `check` → `ok`; `0` files hold the password |
| E9 | E3–E8 done | `HB/install.sh --uninstall`; `launchctl list \| grep -c dev.hommy.test-s1.browsers`; `scripts/test-env.sh down --slot 1 --purge` | `0`; nothing listens on 4720–4729 |
| E10 | maintainer-only; all rows done | `scripts/live-guard.sh check`; `../ops/src-record.sh check` | guard `ok` (the maintainer's browser services keep their PIDs and ports); `unchanged` |

**Unit (offline, CI `test` job):** config rendering of plists for two prefixes; port layout for pool 0–4; the reaper's
own-install filter; `creds` against a scratch keychain; the captcha solver's offline tests that exist in the source.

## Done
E1–E10 green; CI green; README; the Actions secret set.

## Risks
- Personal data in screenshots and test data: only the public-demo labelled grids are copied; every image is checked by
  eye once by the implementer and listed in `implement.md`.
- The engine's scripts kill or kickstart by label: every such call must use this install's prefix (E10 proves the live
  services survive).
