# hommy: install

How to install hommy, what the installer puts on your Mac, what `hommy setup` asks, pairing and uninstalling.

## Install: one command

```sh
curl -fsSL https://raw.githubusercontent.com/AlexDobrushskiy/hommy/main/install.sh | bash
```

Run it in Terminal as an admin user. It installs what is missing (below), then runs `hommy setup`, which asks for your
bot token and walks you through pairing. Re-running it is safe: nothing changes when nothing is new, and a newer hommy
is upgraded with every setting kept. Each step prints `[n/9] <stage>: present|installed|upgraded|skipped`, and
`~/.local/state/hommy/install.log` records every command with its source.

Flags go after `bash -s --`, e.g. `curl -fsSL …/install.sh | bash -s -- --with-whatsapp`:

| Flag | Meaning |
|---|---|
| `--with-whatsapp` | also install `wacli` (WhatsApp; read the [Risks](../README.md#risks) first) |
| `--with-telegram-user` | also install `telegram-agent-cli` (your own Telegram account; opt-in) |
| `--with-captcha` | the local captcha solver's models (several GB) |
| `--whisper-model large-v3-turbo\|medium\|small` | the local speech-to-text model (default `large-v3-turbo`) |
| `--instance NAME`, `--port-base N` | a second, separate install beside the default one |
| `--non-interactive` | never prompt; the bot token must already be in the Keychain ([Setup in detail](#setup-in-detail), Unattended) |
| any `hommy setup` flag | passed through ([Setup in detail](#setup-in-detail)) |

Homebrew's own installer asks for your password once when Homebrew is missing.

## What gets installed

Every tool comes from its own source; nothing is vendored, and Claude Code is never bundled.

| # | What | From |
|---|---|---|
| 1 | checks: macOS 14+, Apple Silicon, an admin user, 10 GB free | — |
| 2 | Homebrew, if missing | the official installer from [Homebrew/install](https://github.com/Homebrew/install) |
| 3 | `ffmpeg`, `whisper-cpp`, `llama-swap`, `uv`, `node`, `git`, [agterm](https://github.com/umputun/agterm), Google Chrome ([Brewfile](../Brewfile); tools you already have are not upgraded) | Homebrew (core, the `mostlygeek/llama-swap` tap, the `umputun/apps` tap, casks) |
| 4 | mlx-audio (Kokoro text to speech) with the English G2P model | PyPI via `uv tool install`, the spaCy model release |
| 5 | Claude Code, only when `claude` is missing; you sign in with your own subscription | Anthropic's installer, `https://claude.ai/install.sh` |
| 6 | hommy | the `hommy` cask of [AlexDobrushskiy/homebrew-tap](https://github.com/AlexDobrushskiy/homebrew-tap) (this repo's releases) |
| 7 | hommy-browsers into `~/.local/share/hommy-browsers` | a clone of [AlexDobrushskiy/hommy-browsers](https://github.com/AlexDobrushskiy/hommy-browsers) |
| 8 | optional extras (the `--with-*` flags) | their own taps |
| 9 | `hommy setup`: vault, speech (the whisper model from Hugging Face, checksum-pinned), launchd services, Claude Code plugins (hommy, hommy-browsers, [retainer](https://github.com/AlexDobrushskiy/retainer)), pairing, doctor | — |

## Setup in detail

```sh
hommy setup
```

`hommy setup` is interactive, safe to re-run (it keeps every existing value unless a flag changes it) and ends with a
`hommy doctor` run. Its steps:

1. **Preflight:** macOS 14+ on arm64; agterm running (it opens it if not); `claude` found, signed in and past its
   first-run screens (otherwise it opens `claude` in an agterm session for you to finish).
2. **Config:** the instance folders and `hommy.env` (section [Configuration](configuration.md#configuration)).
3. **Bot token:** create a bot with [@BotFather](https://t.me/BotFather) (`/newbot`) and paste the token. It is checked
   with Telegram and stored in the macOS Keychain, never in a file.
4. **Vault:** the folder the agent works in (default `~/hommy-vault`; an existing vault is kept). A folder under
   `~/Documents`, `~/Desktop`, `~/Downloads` or iCloud makes macOS ask for privacy access: give the signed copy
   `~/.local/share/hommy/bin/hommy` Full Disk Access (System Settings > Privacy & Security), once; the stable path keeps
   the grant across upgrades. The default location needs no grant.
5. **Permissions:** `skip` (default) starts agent sessions with `--dangerously-skip-permissions`, so an errand is not
   blocked on a dialog nobody is watching; `ask` keeps Claude Code's prompts. Change it later with
   `hommy config permissions ask|skip`.
6. **Speech:** `local` runs whisper (speech to text) and Kokoro (text to speech) on your Mac, `external` points at
   OpenAI-compatible servers you name, `off` answers voice notes with "voice is off".
7. **Keys and services:** a host key for the hub-to-host link, then the launchd services (section
   [Services](configuration.md#services)).
8. **Pairing** (below) and **doctor** ([configuration.md](configuration.md#doctor)).

| Flag | Meaning |
|---|---|
| `--instance NAME` | a separate install beside the default one (own paths, labels, Keychain items, ports) |
| `--non-interactive` | never prompt; a missing required value exits 2 |
| `--token-stdin` | read the bot token from stdin |
| `--vault PATH` | the vault folder |
| `--permissions skip\|ask` | agent permissions |
| `--speech local\|external\|off` | speech mode; `--stt-url`, `--tts-url` for `external`; `--whisper-model large-v3-turbo\|medium\|small` |
| `--claude-bin PATH` | the `claude` to run (default: the one on `PATH`) |
| `--port-base N`, `--host-name NAME` | first port (default 8470), this Mac's host name (default its `LocalHostName`) |
| `--telegram-api URL` | Bot API base URL (default `https://api.telegram.org`) |
| `--no-pair-wait` | do not wait for pairing (run `hommy pair` later) |
| `--join WS_URL --name NAME --key-stdin` | host-only install on another Mac ([More hosts](configuration.md#more-hosts-more-macs)) |

**Unattended** (`--non-interactive`, e.g. through the installer's `bash -s -- --non-interactive --no-pair-wait`): put the
bot token into the Keychain first with `security add-generic-password -s hommy -a telegram-bot-token -w` (it asks for
the token), then pair later with `hommy pair`.

## Pairing

`hommy pair` (run by setup) prints a code `XXXX-XXXX` and a `t.me` link. Open the link, or send your bot
`/start XXXX-XXXX` in a **private chat**. The first user to do that becomes the **owner**; the bot replies that it is
paired. Everyone else, and every group, is ignored without a reply, and before pairing the bot sends nothing at all. The
code is single use and expires after 30 minutes; five wrong codes from one user lock that user out until a new code is
made. To pair another account: `hommy pair --reset`. `hommy pair --status` prints `paired`, `pending` or `not paired`.

## Uninstall

```sh
hommy service uninstall --purge          # services, and every file, folder and Keychain item hommy created
~/.local/share/hommy-browsers/install.sh --uninstall && rm -rf ~/.local/share/hommy-browsers ~/.local/state/hommy-browsers
claude plugin uninstall hommy@hommy; claude plugin uninstall hommy-browsers@hommy-browsers
claude plugin marketplace remove hommy; claude plugin marketplace remove hommy-browsers
brew uninstall --cask hommy
```

Your vault (`~/hommy-vault`) is never removed. The third-party tools (Homebrew, agterm, Claude Code, ffmpeg, whisper,
llama-swap, uv, node, mlx-audio, Chrome) stay: you may use them for other things; remove them with their own
uninstallers if not (`uv tool uninstall mlx-audio`, `brew uninstall …`).
