# Decisions

Synthesized outputs of sessions that would otherwise evaporate. When a conversation produces a real analysis, file it
here as a page so the reasoning compounds instead of dying in chat history.

## When to create a decision page

**Yes:**
- A choice was made between options and the reasoning matters.
- A non-trivial trade-off was analysed, even if the call is "wait".
- A direction-setting conversation produced a conclusion that should bind later sessions.
- A previous decision was revisited and reaffirmed or reversed.

**No:**
- A factual update: it goes in the matching `context/` file.
- A task or a date: it goes in `active/`.
- A capture or brain dump: it goes through `inbox/`.
- A small tactical pick that won't matter in a month.

## File naming

`YYYY-MM-DD-<short-slug>.md`. A revisited decision gets a NEW dated file linking back with `supersedes:`; the original
is never rewritten, it only gains a `superseded_by:` pointer.

## Page format

```markdown
---
date: YYYY-MM-DD
status: decided | open | revisited | superseded
topic: short tag
relates_to: ["[[Dashboard]]", "[[relevant-context-file]]"]
supersedes: <optional earlier decision file>
superseded_by: <optional later decision file>
---

# <Decision title>

## Question
What was being decided, in one or two sentences.

## Context
What state of the world matters. Link out; don't duplicate the Dashboard or context files.

## Options considered
- **A**: ...
- **B**: ...

## Decision
What was chosen, and the core reason in one paragraph.

## Reasoning
What tipped the balance, what was discounted, which constraints dominated.

## Revisit conditions
What would reopen this (concrete triggers, not "someday").
```

## Rule for the AI

When a session meets the "Yes" criteria:
1. **Offer** to file it; never file silently. The owner confirms.
2. Write the page in the format above.
3. Append to `log.md` (`## [YYYY-MM-DD] decision | <title>`).
4. Update `index.md` when its Decisions section lists entries.
5. Link it from the related context and active files with `[[wikilinks]]`.
