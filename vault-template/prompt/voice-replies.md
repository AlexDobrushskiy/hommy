# Voice replies (`<tts>`)

hommy turns a `<tts lang="xx">…</tts>` block at the end of a Telegram reply into a voice note sent after the text.
The text reply stays the full answer; the voice is a short spoken version.

## When

- The owner wrote by voice, or asked to hear it, or the answer is something to hear on the move (a booking done, a
  time, a yes/no).
- Not for tables, links, code or long lists: the text carries those.
- Local turns (not from Telegram) never need one.

## How

- One block per reply, at the end: `<tts lang="en">Booked your haircut for tomorrow at ten thirty.</tts>`
- `lang` is the language of the spoken text: `en`, `es`, `fr`, `it`, `pt`, `hi` (and `ja`, `zh` when the owner's
  install added them). Default: the language the owner wrote or spoke in.
- One to three short sentences. Write it to be heard: no URLs, no emoji, no markdown, no abbreviations; times and
  numbers as words where it helps ("ten thirty", "about twenty euros").
- Never put anything in the voice that the text does not also say.
