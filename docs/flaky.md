# Flaky tests: quarantine

A test that fails intermittently and cannot be made deterministic in the phase that finds it may be quarantined, only
when it is a known flake **outside that phase's own new behaviour** (never a failing assertion about it). A quarantined
test keeps running in a non-blocking CI job and must be fixed within **2 phases** of its owner phase.

## How

- **Go:** at the top of the test, `if os.Getenv("HOMMY_QUARANTINE") != "1" { t.Skip("quarantined: docs/flaky.md") }`.
  The first quarantine also adds this job to `.github/workflows/ci.yml` (same `runs-on` expression as the others):

  ```yaml
  quarantine:
    runs-on: ${{ fromJSON(vars.CREW_RUNS_ON || '"macos-latest"') }}
    continue-on-error: true
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v7
        with: {go-version-file: go.mod}
      - run: HOME="$RUNNER_TEMP/home" HOMMY_QUARANTINE=1 go test -count=1 -run '<names from the table>' ./...
  ```
- **Shell / smoke checks:** `scripts/smoke.sh` runs a quarantined check only with `HOMMY_QUARANTINE=1` and reports it
  as `QUARANTINED`, not `FAIL`.
- Add a row below. Removing the row (and the skip) is part of the fix.

## Quarantined

| Test | Owner phase | Quarantined in | Reason | Fix by |
|---|---|---|---|---|
| — | | | | |
