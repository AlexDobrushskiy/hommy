# hommy: usage

How an errand runs, your first steps after setup, what you can send the bot, voices, and a demo errand to watch.

## How an errand runs

A typical errand: you are walking and say "book me a haircut at Barbearia Exemplo tomorrow at half ten". The hub on
your Mac transcribes the voice note locally, the host opens a new agterm session running `claude` in your vault, the
vault's routing table sends it to the `book-local-service` recipe, which opens the barber's booking page in a browser
of [hommy-browsers](https://github.com/AlexDobrushskiy/hommy-browsers), picks the slot and asks you on Telegram
("Book haircut, tomorrow 10:30? Yes / No") before the final click. You tap Yes; it books, writes the reference to the
vault's `log.md` and answers with a short text and a voice note. (Barbearia Exemplo is invented; see
[The barber demo](#the-barber-demo).)

## First steps

1. **Pair.** Setup prints a code `XXXX-XXXX` and a `t.me` link. Open the link on your phone (or send your bot
   `/start XXXX-XXXX` in a private chat). The bot answers that it is paired; from now on it listens to you only.
2. **First voice note.** Send the bot a voice note: "What's in my vault?". A session opens in agterm on the Mac, the
   bot answers `started <name> on <mac>` and then the reply. Watch the session in agterm if you like; type into it at
   the Mac and those turns stay local.
3. **Check it.** `hommy doctor` prints one line per check, `✓` or `✗` with the command that fixes it.
4. **Teach it about you.** Setup offers to open a session that runs the retainer interview (`--seed-context` says yes
   up front); it fills `context/` (home address, usual places, preferences), which the recipes read. Later, ask any
   session in the vault: "Run the retainer interview".

## Using it

Message the bot. By default every errand starts a **new agterm session** in your vault (the bot answers
`started <name> on <mac>`); more messages in that chat go into the same session until you start another.

| You send | What happens |
|---|---|
| text | typed into the current target's session (or run with `claude -p` for a `vault@` target) |
| a voice note | transcribed locally, then handled like text |
| a photo or file | saved on the Mac, its path typed into the session |
| a reply to a bot message | goes to the session that wrote that message |
| a reaction or a button | typed into that session with its meaning (`👍 yes / continue`) |
| `/new_session` | the next message starts a fresh session |
| `/target <spec>` | switch the target: `vault@<mac>`, `p@<mac>:<dir>`, `session@<mac>:<id>`, `new@<mac>:<dir>:interactive` |
| `/status` | hub, hosts, sessions and their state |
| `/voice` | list the English voices; `/voice <name>` switches |
| `/spawn`, `/peek`, `/files`, `/pause`, `/done` | session commands: open a session, look at its screen, read its brief folder, pause or finish it |

Answers come back as a reply to your message. A long answer also arrives as a voice note (the agent writes the spoken
version in a `<tts>` block). Turns you type at the Mac stay local and send nothing to Telegram. If an agent is stuck at a
permission prompt you get one `waiting at the Mac` message. The Stop button on the live draft cancels a `claude -p` run.

Inside a session the agent can write to you with `hommy send "text"` (add `--agent NAME --buttons "Yes|No"` to get
your tap back in the session) and read your lines with `hommy inbox`. Protocol details: [docs/protocol.md](protocol.md).

## Voices

Answers over `TTS_MIN_CHARS` get a voice note. The agent appends the spoken version as `<tts>...</tts>`, or
`<tts lang="es">...</tts>` for another language. Built in: English (`af_heart`), Spanish, French, Italian, Portuguese and
Hindi, all Kokoro voices from the local speech stack. Add or replace a language with `TTS_LANG_BACKENDS`
(comma-separated `lang|url|model|voice`); any server that speaks the OpenAI `/v1/audio/speech` protocol works, which is
how Japanese, Chinese (extra G2P packages) or Russian (another engine) are added. A language without a backend falls
back to the default voice. A voice failure never blocks the text.

## The barber demo

Barbearia Exemplo is an invented barber served locally by this repo's test fixture, so you can watch the reference
flow end to end without booking anything real. From a clone of this repo (needs Go: `brew install go`):

```sh
go run ./cmd/hommy-e2e booking-site --port 8480 --log /tmp/barber.log &      # http://127.0.0.1:8480/
scripts/vault.sh add-place ~/hommy-vault "Barbearia Exemplo" http://127.0.0.1:8480/
```

Then send the bot "Book a haircut at Barbearia Exemplo tomorrow at 10:30." The agent opens the site in a lane browser,
reaches the confirmation page and asks you before the final click; `/tmp/barber.log` shows each page it requested.
Answer Yes and the site shows `Booked ✓ ref BX-1`; the vault's `log.md` gets the line. Stop the site with `kill %1`.
