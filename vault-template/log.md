# Vault log

Append-only chronological record of what sessions did in and through this vault. Newest entries at the bottom. Never
edit past entries.

## Format

Each entry starts with a parseable header line, optionally followed by one or two lines of detail:

```
## [YYYY-MM-DD] <type> | <short title>
```

Types:
- `errand` — an errand finished (what was done, the reference or result: "booked Barbearia Exemplo haircut 2026-03-14 10:30, ref BX-1")
- `decision` — a decision was filed in `decisions/` (link the page)
- `ingest` — a non-trivial inbox capture was processed (link source and destinations)
- `lint` — a vault audit pass (summarize the findings)
- `structure` — a structural change (new folder, new recipe, new convention)

Skip routine tweaks (single-fact context updates, plan edits). The bar: **"would I want to find this in 6 months?"**

Quick query: `grep "^## \[" log.md | tail -20`

---

## [YYYY-MM-DD] structure | vault created from the hommy template

Template with three recipes (booking, email triage, shopping). The owner's context is still `⚠️ to capture`: the
retainer interview, or passive learning, fills it.
