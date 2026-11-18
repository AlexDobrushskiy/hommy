---
name: notify
description: Use when the user asks to be sent something on Telegram ("send me the link", "ping my phone", "text me when it's done"), or when a session must ask the user something from their phone (a yes/no before a booking or a purchase, a choice between options). Sends through hommy's bot with `hommy send`; replies, reactions and button presses come back into the session.
---

# Send the user a Telegram message (`hommy send`)

```sh
hommy send "<text>"
hommy send - <<'MSG'
multi-line text
MSG
```

- Text goes verbatim: newlines kept, plain text (no Markdown), split past 4096 characters. URLs get a link preview.
- `--silent`: no notification sound (night). It does not make an unrequested ping fine (see "When to push").
- `-h` / `--help` prints the usage and sends nothing; an unknown `-x` / `--xyz` exits 2 and sends nothing. Text that
  starts with a dash: `hommy send -- "--text"`.
- Exit 0 and `{"ok":true,"via":"hub|direct","message_ids":[…]}` (with `--json` or `--agent`) means it was sent. A
  non-zero exit with `error: …` means it was not: say so, never claim it was sent.
- hommy keeps its bot token in the macOS Keychain. Never look for it, print it or put it in a file.
- Another instance (a test install): `hommy --instance <name> send …`.

## Asking the user (agents and errands)

```sh
hommy send --agent <slug> "the dentist has 10:30 or 15:00, which one?"
hommy send --agent <slug> --buttons "Book|Cancel" "Barbearia Exemplo 2026-03-14 10:30?"
hommy send --agent <slug> --buttons "10:30|15:00|Answer…" "which slot?"   # "…" = the user answers in text
hommy send --agent <slug> --ask "what's the budget?"                       # adds an "Answer…" button
```

- `--agent <slug>` (letters, digits, `.`, `_`, `-`) heads the message `🤖 <slug> · <host>` and registers it with the
  hub, so the user's reply, reaction or button press comes back typed into this session as
  `[Telegram ← … · tg-<id> · re "…"] …` and lands in the session's inbox. `--buttons` and `--ask` need `--agent`.
- Inbox: `hommy inbox list --unacked` · `hommy inbox ack <id>` (→ 🫡 on the user's message) · `hommy inbox pending`
  (exit 0 = unacked items) · `hommy inbox wait --timeout 300` (`--pending`: also returns at once on an unacked item).
- Wait for the answer before doing the thing you asked about. Silence is not a yes.

## When to push

- Telegram is for what the user asked for right now ("send me the link"), a question that blocks the errand (with
  `--buttons` or `--ask`), and the result of an errand they started from Telegram.
- No FYI, progress or confirmation pings ("started", "noted", "ignore my last"). A message from the user gets one
  reply in that turn, not a stream of them.
- A session started from Telegram answers through its final message anyway (hommy's Stop hook sends it): do not also
  `hommy send` the same answer.

## What to send

Lead with what they asked for (the link on the first line or right after a short title), then at most 2–3 lines they
can act on from a phone. No transcript dumps.
