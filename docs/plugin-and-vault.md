# hommy: the Claude Code plugin and the vault template

What hommy adds to Claude Code (hooks, skills) and the vault that `hommy vault init` writes.

## The Claude Code plugin

`plugin/` is hommy's side of Claude Code, from the marketplace at this repo's root (the installer adds it; by hand:
`claude plugin marketplace add <this repo>` then `claude plugin install hommy@hommy`):

- **Hooks** (`plugin/hooks/hooks.json`): SessionStart, UserPromptSubmit, Stop and Notification run `hommy hook …`
  through `plugin/bin/hommy-hook`, which never prints, always exits 0 and gives up within about 4 s, so a session
  works the same with hommy stopped or not installed. Only a prompt hommy typed (`[Telegram · tg-…`) becomes a Telegram
  turn. Also: agterm session status, and guards that ask before WhatsApp (`wacli`) and Telegram (`telegram-agent-cli`)
  writes, each a no-op without its tool.
- **Skills:** `notify` (`hommy send`: messages and button questions to your phone), `whatsapp`, `telegram`,
  `shopping-research`.
- `userConfig.instance`: which hommy instance receives the hooks (empty = the default). No secrets: hommy keeps its
  tokens in the Keychain.

hommy never writes hook entries into `~/.claude/settings.json`. **If you wired another Telegram bridge's hooks into
`settings.json` by hand, remove them**: both sets would fire on every turn.

## The vault template

`vault-template/` is the vault `hommy vault init` writes (embedded in the binary): an `AGENTS.md` kernel with a routing
table, `context/`, `active/`, `decisions/`, `inbox/`, `archive/`, and three recipes in `prompt/` (book a local
service, email triage, find and buy). The [retainer](https://github.com/AlexDobrushskiy/retainer) interview fills it
with your context. `scripts/vault-lint.sh <vault>` checks its structure.
