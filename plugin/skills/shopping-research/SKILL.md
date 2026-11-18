---
name: shopping-research
description: Use when the user wants to buy something online or asks which model, which size, where to buy, the best price, stock or delivery time — any product (shoes, clothing, gear, electronics, tools, furniture), shipped to the user's address in context/places.md.
---

# Shopping research

**The answer is only as good as the data under it.** Every price, size and stock claim comes from a page you opened,
for that exact model + size + colour. Two failures this skill exists to stop: quoting the first shop's price (a plain
search often finds the same item noticeably cheaper elsewhere), and choosing a size from one chart plus your own rule of
thumb.

**Where it ships:** the user's address in the vault's `context/places.md` (the row of kind `home`; a trip address
when the item is for a trip: ask once). No address there? Ask once and offer to save it. Country, currency and shop
list follow that address.

## Steps

0. **Scale it.** If the user names the exact item and just wants a link to order it, do steps 3, 4 (their usual shop
   plus a price comparison only) and 6. The full run is for open choices.
1. **Pin the exact item:** model + version/year + gender + size + width. If the model is still open, shortlist first
   (independent lab reviews), then run steps 2–6 on your pick and the runner-up. Discontinued or leftover stock only →
   say so and add the current successor as an option.
2. **Size, only if the item has sizes.** Read the vault's sizes file if it has one (`context/` — the routing table
   says where): the user's measurements and their verdict on every brand and size they bought. **If it has nothing for
   this kind of item**, ask once before researching, then record the answer. Then gather, in this order of trust:
   1. **The brand's own fit tool for that model** ("What is my size?").
   2. **The brand's chart**, opened on the brand's own site.
   3. **The charts of 2–3 shops**, plus each product page's fit note ("users usually buy one size up").

   Say what each chart's "cm" row means (foot length, item length, or a label) before comparing numbers across charts.
   Give **one** size, plus the evidence for the size next to it. Never add your own margin on top, and don't default
   to "order two sizes, return one": returns cost the user an errand.
3. **Price sweep in a browser** (load the `agent-browsers` skill): a shopping search for the exact model + size in the
   user's country, plus the country's price-comparison sites. Search engines often bot-check lane browsers; retry in
   the identity browser when they do.
4. **Open the main shops directly:** the user's usual shops (from `context/preferences.md`, when listed), the big
   general retailers that deliver to the address, the category specialists (sports, electronics, DIY), shops in
   neighbouring countries that ship there, and the brand's own store.

   **On each page:**
   - **Check every colour.** The same item often differs a lot in price by colour.
   - **Open the size selector.** Note the size is in stock, sold out, or "last unit".
   - Note the delivery time to the address and the return terms: days, and who pays.
   - **A shop with a store near the user beats a remote one on returns**: rank a local offer above a distant
     marketplace unless the marketplace is clearly cheaper. Never skip a shop because a lane failed to load it: retry
     in the identity browser.
   - **Deadline given?** Drop offers that can't arrive in time before ranking by price.
   - Apply any membership discount the user has recorded in the vault.
   - Home delivery unless the user's preferences say otherwise; include the delivery fee in the price.
5. **Try-on and buy are separate questions.** Name where the user can try it on near the address and, separately, where
   it's cheapest.
6. **Report** as a table for one item.
   - **Columns:** shop · colour · size in stock · price incl. delivery · delivery time · returns · link.
   - **Then:** the size verdict with its evidence, and one pick.
   - **Confidence:** mark every figure **verified** (the product page was opened in a browser, by you or a subagent
     that returned the URL) or **unverified** (search snippet, listing card, memory).
7. **Links only.** Never add to cart, log in to pay, or order. The user orders.
8. **After they buy:** add or update the row in the vault's sizes file (model, size, date, "⏳ ordered"). When they
   report the fit, fill in the verdict (too big / right / too small).

## Red flags: stop and redo the step

| You're about to... | Instead |
|---|---|
| Give "the price" from one shop or a subagent's sample | Run steps 3 + 4 |
| Quote one colour's price | Open the other colours |
| Pick a size from a reseller's copy of the brand chart because the brand site blocked a fetch | Open the brand site in the browser |
| Add a margin of your own to the size | Take the brand tool's size; state the alternative with its evidence |
| Say "in stock" from a listing | Open the size selector |
| Answer a size question without reading the sizes file | Read it first |

## Parallel shop checks

Subagents get their own lanes (`pw-lane-a`…`-d`, per `agent-browsers`). Give each subagent the exact model, size and
colours to check, and the table columns to return. Retry a bot-checked shop in the identity browser yourself.
