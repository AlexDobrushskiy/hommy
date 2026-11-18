# Release v0.1.0

Go step: `ops/go-public.sh --go` on the build Mac, 2026-11-18T12:56:11Z.

| Step | Finished (UTC) | Result |
|---|---|---|
| preconditions | 2026-11-18T12:56:23Z | result: all ok |
| scan | 2026-11-18T12:56:47Z | result: clean, everything scanned |
| purge | 2026-11-18T12:57:08Z | hommy: 0 runs, 0 caches; hommy-browsers: 0 runs, 0 caches |
| runners | 2026-11-18T12:57:24Z | hommy: 0 runners, no CREW_RUNS_ON; hommy-browsers: 0 runners, no CREW_RUNS_ON |
| public | 2026-11-18T12:57:28Z | hommy-browsers: PUBLIC at 2026-11-18T12:57:26Z; hommy: PUBLIC at 2026-11-18T12:57:28Z |
| ci | 2026-11-18T14:39:31Z | hommy: run 44323675995 success on 2bada50, 4 jobs GitHub-hosted; hommy-browsers: run 44325580780 success on 18a1e18, 4 jobs GitHub-hosted |
| release | 2026-11-18T14:39:47Z | release: v0.1.0 with hommy_0.1.0_darwin_arm64.tar.gz; Casks/hommy.rb 0.1.0 by the noreply identity |
| vm | 2026-11-18T15:30:07Z | doctor: exit 0, 19 ✓; vm deleted: 2026-11-18T15:30:05Z; image deleted: 2026-11-18T15:30:07Z; paired: yes |

## P1

Scans clean; Actions logs and caches purged; both runners and `CREW_RUNS_ON` removed before the switch; both repos public; GitHub-hosted CI green on main; release with the archive; the tap's cask committed by the noreply identity (table above).

## P2

Fresh VM `hommy-07` from the cached image, no GitHub credentials; the public one-liner with `--non-interactive --no-pair-wait`; paired with the real test bot; `hommy doctor` exit 0, 19 ✓. Timings (s): {"create": 29, "install": 469}. VM deleted 2026-11-18T15:30:05Z, cached image deleted 2026-11-18T15:30:07Z.
