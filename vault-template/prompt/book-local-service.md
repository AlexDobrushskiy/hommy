# Recipe: book a local service

A barber, a dentist, a table, a class: anything booked on a business's own booking page. The booking commits the
owner, so **nothing is submitted before the owner says yes to that exact slot.**

## Steps

1. **Find the business** in `context/places.md` (the row's Booking column is the booking page). Not there: search the
   web for its official booking page, and ask the owner before adding a row (`| Name | Kind | Booking | Notes |`).
2. **Resolve the date and time** with `date`, never from memory: "tomorrow" = `date -v+1d +%F`, "on Friday" =
   `date -v+fri +%F` (macOS `date`; the Mac's time zone is the owner's). Keep the time exactly as asked (`10:30`).
3. **Open the booking page in a lane browser**: the `pw-lane` tools of hommy-browsers (the `agent-browsers` skill;
   tool names `mcp__plugin_hommy-browsers_pw-lane__…`). A lane is a clean, logged-out browser: use the identity
   browser only when the site needs the owner's login.
4. **Pick the service, date and time** the owner asked for. Fill only what the page needs; contact details come from
   `context/bio.md` (ask when missing). The slot is not offered? Don't pick a neighbour silently: name the closest
   free slots in the question (step 6).
5. **Stop on the confirmation page**: the summary with the final "Confirm" / "Book" / "Pay" button. Do not press it.
6. **Ask the owner, one question:**
   - **Telegram turn** (the prompt starts `[Telegram · tg-`): `hommy send --agent booking --buttons "Book|Cancel"
     "<business> <service> <date> <time>?"`, keep the lane open on the confirmation page and wait for the press (it
     arrives typed into this session).
   - **Any other session** (not started from Telegram): close the lane, then end the turn with the question as your
     final message: "<business>: <service> on <date> at <time>. Shall I book it?" On a yes in the next turn, open the
     confirmation page again and go on with step 7.
7. **Submit only after "Book"** (or an explicit yes for that slot). "Cancel", silence or anything else: submit nothing.
   Read the confirmation (reference number) from the page.
8. **Finish:**
   - append one entry to `log.md`: `## [<today>] errand | booked <business> <service> <date> <time> (ref <ref>)`;
   - close the lane (`browser_close`);
   - answer with the result and a `<tts>` block (prompt/voice-replies.md), e.g. "Booked: haircut at Barbearia
     Exemplo tomorrow at 10:30, reference BX-1." `<tts lang="en">Booked your haircut for tomorrow at ten thirty.</tts>`

## Never

- Press the final submit before the owner's yes; create an account; pay or give card details (ask the owner to do it).
- Save screenshots or page dumps in the vault.
