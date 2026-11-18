# 07 · Go public

**Goal:** everything that must be true before `hommy` and `hommy-browsers` become public is true and proven: the README a
stranger needs, clean scans of every ref GitHub will serve (S8), the release prepared, and **one ready-to-run step**
(`ops/go-public.sh --go`) that waits for Alex's OK. After his OK it switches visibility, releases `v0.1.0` and proves a
fresh VM installs from the public one-liner with no GitHub login.
**Depends on:** 05; runs in **workslot 3** in parallel with 06; the go step itself requires 06 shipped (master §14
D-15, D-22). **Master:** I1, I3, §8.3, §9, §10 (S7), S8, §14 D-14.

## Scope
- **In:**
  - `README.md` (hommy): what it is (one paragraph + the reference flow with Barbearia Exemplo), the one command, what
    it installs and from where (master §8.1 table), first steps (pairing, the first voice note, `hommy doctor`), the
    barber demo, risks one line each (design › Risks), signing note (self-signed; why grants persist), voices and
    extras (master §3.2, §14 D-7), Mini App opt-in, more hosts (README-only LAN steps, §14 D-17), uninstall, license.
    `hommy-browsers` README reviewed for the same reader.
  - `CHANGELOG.md` `v0.1.0`; `.goreleaser.yaml` upload switched on for tags (cask to the tap, noreply commit author).
  - Workspace `ops/go-public.sh [--dry-run|--go]`, in this order: preconditions (06 shipped, CI green on `main` of
    both repos, tag absent, VM image cached, ≥ 60 GB free, the README's install command and flags equal
    `scripts/clean-room.sh`'s, 07-E1 re-run); the S8 scans (below; abort on any finding);
    purge Actions run logs of both repos; delete `CREW_RUNS_ON` and unregister both self-hosted runners (S7: before the
    switch); `gh repo edit --visibility public --accept-visibility-change-consequences` for both repos; wait for a
    green GitHub-hosted CI run on `main`; tag `v0.1.0` and run GoReleaser on the build Mac with `GITHUB_TOKEN=$(gh auth
    token)` (release + the tap's `Casks/hommy.rb`; no tap-write secret in Actions); the post-go VM proof (P2); write
    `docs/release.md` (a follow-up PR). `--dry-run` checks every precondition and prints every step, changing nothing.
  - S8 scans (`ops/go-public.sh scan`, also run by the rows): fresh `git clone --mirror` of `hommy`, `hommy-browsers`
    and the tap (mirrors fetch `refs/pull/*/head`), `scripts/leak-scan.sh --history <mirror>` on each (blobs, path
    names, commit headers and messages; `Claude-Session:` trailers allowed); commit identities on every commit of both
    repos, and on the tap only after the tap HEAD recorded at this phase's start (`ops/go-public.sh record-tap`; the
    tap is shared and already public, older commits are pre-existing); PR titles, bodies and comments, release notes,
    tag and branch names of both repos via `gh api` through the same scanner; no stray branches (`ci-probe/*`,
    `leak-probe/*`, phase branches) left on the remotes. Before 06 ships, `scan` excludes 06's in-flight refs
    (`phase-06`, `ci-probe/06-*`) and reports them as pending; `--go` scans everything.
- **Out:** any announcement; the Developer ID (design: none); a mirror install (05-E7 already proves `--source` on a
  fresh VM; P2 proves the public one-liner).

## Tasks
1. README, CHANGELOG, release config; `ops/go-public.sh` with `record-tap`, `scan`, `runs`, `--dry-run`, `--go`.
2. Pre-go rows (the lane ships here).
3. The overseer asks Alex (`ops/tell-ceo.sh --ask` with the scan summary: repos and counts only) after 06 ships.
4. After his OK: `ops/go-public.sh --go`; P1–P2 recorded in `docs/release.md`.

## E2E cases
Pre-go (the lane's blind e2e): E1–E4. Post-go (run by `ops/go-public.sh --go`, recorded in `docs/release.md`): P1–P2.
`GP` = `../ops/go-public.sh` (workspace present; maintainer-only rows say so).

| # | Given | When | Then |
|---|---|---|---|
| E1 | the bound tree | read `README.md`; follow each relative link; `grep -c 'install.sh' README.md` | sections: what, one command, what gets installed (with sources), first steps, barber demo, risks (one line each), signing, voices, Mini App, more hosts, uninstall, license; every link resolves; `≥ 1` |
| E2 | maintainer-only | `GP scan` | for each of `hommy`, `hommy-browsers`, the tap: refs scanned (branches, tags, `refs/pull/*` count ≥ the merged PRs for both repos), `0` leak hits (blobs, paths, headers, messages), `0` identity findings (the tap: only commits after the recorded HEAD), `0` hits in PR/release/ref text; exit 0 |
| E3 | maintainer-only | `git ls-remote --heads origin` in both repos; `GP runs` | only `main`, this phase's PR branch and 06's in-flight refs; `runs` lists the Actions runs the go step will purge (counts per repo), deleting nothing |
| E4 | maintainer-only | `GP --dry-run`; `gh repo view AlexDobrushskiy/hommy --json visibility`; the same for `hommy-browsers` | every precondition printed with its state (`06 shipped` may be `waiting`), every step listed in the order of the scope, nothing changed: both still `PRIVATE`, runners and `CREW_RUNS_ON` still present |
| P1 | Alex's OK; 06 shipped | `GP --go` | the scans pass again; logs purged; runners and `CREW_RUNS_ON` gone before the switch; both repos `PUBLIC`; a GitHub-hosted CI run on `main` green; release `v0.1.0` with the darwin/arm64 archive; the tap has `Casks/hommy.rb` at `0.1.0` committed by the noreply identity |
| P2 | P1; fresh VM `hommy-07` from the cached image; no GitHub credentials in it | `curl -fsSL https://raw.githubusercontent.com/AlexDobrushskiy/hommy/main/install.sh \| bash -s -- --non-interactive …` (the real test bot); pairing with `T` (`TG=real`); `hommy doctor` | exit 0; paired; all `✓`; then the VM and the cached image deleted (`tart delete`, `tart prune`); `docs/release.md` records P1–P2 with times |

## Done
E1–E4 green and `GP --go` ready (the lane ships here); after Alex's OK, P1–P2 green and `docs/release.md` merged.

## Risks
- A finding in history at E2: history is never rewritten in place; the repo would be recreated from a clean tree (the
  overseer decides), which is why E2 runs before anything is public.
- PR heads cannot be deleted: they are scanned, not cleaned.
- The tap is already public and shared with another product: the go step only adds `Casks/hommy.rb`.
