# Changelog

## v0.1.0

The first public release: a Telegram voice note becomes an errand that an interactive Claude Code session on your Mac
carries out, with a markdown vault as its memory and recipe book.

- **One command** installs everything on an Apple-Silicon Mac (macOS 14+): Homebrew if missing, the third-party
  tools from their own sources, Claude Code only when missing, hommy (Homebrew cask), hommy-browsers, the local speech
  stack, then `hommy setup`. Re-running it changes nothing; a newer hommy is upgraded with every setting kept.
- **Hub and host** under launchd: the hub talks to Telegram, the host opens agterm sessions running the unmodified
  `claude`; more Macs join as extra hosts over a plain WebSocket with a host key.
- **Pairing:** a single-use code makes the first private-chat sender the owner; nobody else reaches an agent, and an
  unpaired bot sends nothing.
- **Delivery:** jobs stored before they are pushed, acked after an fsynced write, re-pushed until acked, idempotent.
- **Speech, local by default:** whisper for voice notes, Kokoro voice replies in English, Spanish, French, Italian,
  Portuguese and Hindi; other languages through any OpenAI-compatible speech server.
- **Claude Code plugins:** hommy's hooks and skills, hommy-browsers' browser tools, and the retainer interview.
- **Vault template** with a routing table and three recipes (book a local service, email triage, find and buy).
- `hommy doctor`, `hommy config`, `hommy service`, an opt-in Telegram Mini App.
- Secrets only in the macOS Keychain; agent permissions default to skip (`hommy config permissions ask` to change).
