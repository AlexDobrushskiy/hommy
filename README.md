# hommy

Send a Telegram text or voice note, and an interactive Claude Code session on your Mac does the errand. It runs in
[agterm](https://github.com/umputun/agterm), inside a markdown vault that is its memory and recipe book, and the answer
comes back to the same chat as text and, for longer replies, a voice note.

- **One Mac by default:** a hub (talks to Telegram), a host (opens the sessions) and a local speech stack, all under
  launchd. More Macs can join as extra hosts.
- **Your subscription, your Claude:** hommy starts the unmodified `claude` you signed in to. It is never bundled.
- **macOS 14+ on Apple Silicon only.**

> Not released yet: nothing here is ready to install from a public source. Development notes are at the end.

## Setup

```sh
hommy setup
```

`hommy setup` is interactive, safe to re-run (it keeps every existing value unless a flag changes it) and ends with a
`hommy doctor` run. Its steps:

1. **Preflight:** macOS 14+ on arm64; agterm running (it opens it if not); `claude` found, signed in and past its
   first-run screens (otherwise it opens `claude` in an agterm session for you to finish).
2. **Config:** the instance folders and `hommy.env` (section Configuration).
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
7. **Keys and services:** a host key for the hub-to-host link, then the launchd services (section Services).
8. **Pairing** and **doctor** (below).

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
| `--join WS_URL --name NAME --key-stdin` | host-only install on another Mac (More Macs) |

### Pairing

`hommy pair` (run by setup) prints a code `XXXX-XXXX` and a `t.me` link. Open the link, or send your bot
`/start XXXX-XXXX` in a **private chat**. The first user to do that becomes the **owner**; the bot replies that it is
paired. Everyone else, and every group, is ignored without a reply, and before pairing the bot sends nothing at all. The
code is single use and expires after 30 minutes; five wrong codes from one user lock that user out until a new code is
made. To pair another account: `hommy pair --reset`. `hommy pair --status` prints `paired`, `pending` or `not paired`.

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
your tap back in the session) and read your lines with `hommy inbox`. Protocol details: [docs/protocol.md](docs/protocol.md).

## Configuration

`hommy config list|get|set|unset [KEY] [VALUE]` edits `~/.config/hommy/hommy.env` (`KEY=VALUE`, mode 0600), validates
the value and restarts the affected service. `hommy config path` prints the file. Precedence: process environment, file,
default. Secrets are never in it: they live in the Keychain (`telegram-bot-token`, `host-key`, `hub-host-keys`,
optional `gemini-api-key`).

| Key | Default | Meaning |
|---|---|---|
| `PORT_BASE` | `8470` | the ports below derive from it |
| `HUB_LISTEN`, `HUB_URL` | `127.0.0.1:8470`, `ws://127.0.0.1:8470/ws` | where the hub listens, where hosts connect |
| `VAULT_DIR` | `~/hommy-vault` | the agent's working folder |
| `DEFAULT_TARGET` | `new@<this mac>:<vault>:interactive` | where a message goes when no target is set |
| `HOST_NAME` | the Mac's `LocalHostName` | this host's name |
| `AGENT_PERMISSIONS` | `skip` | `skip` or `ask` |
| `AGENT_COMMAND` | the absolute path of `claude` | what agterm runs for a new session |
| `AGTERM_WORKSPACE` | `hommy` | the agterm workspace that holds the sessions |
| `AGENT_BRIEFS_DIR` | `~/.local/share/hommy/briefs` | one folder per errand |
| `NEW_SESSION_ROOTS`, `NEW_SESSION_PINNED` | `~/Projects,~/src`, the vault | folders a new session may start in |
| `CLAUDE_TIMEOUT` | `30m` | limit for a `claude -p` run |
| `PAIRING_TTL` | `30m` | pairing code lifetime |
| `SPEECH` | `local` | `local`, `external` or `off` |
| `WHISPER_ENDPOINTS` | the local whisper | speech-to-text servers, `name\|url\|model` |
| `WHISPER_MODEL` | `large-v3-turbo` | local whisper size |
| `TTS_URL`, `TTS_MODEL`, `TTS_DEFAULT_VOICE` | local Kokoro, `af_heart` | text to speech; `TTS_URL=off` disables voice |
| `TTS_LANG_BACKENDS` | es, fr, it, pt, hi | extra TTS languages (Voices) |
| `TTS_MIN_CHARS` | `400` | shorter answers get no voice note |
| `GEMINI_MODEL` | `gemini-2.5-flash` | used only when a Gemini key is in the Keychain (STT fallback) |
| `MINIAPP_LISTEN`, `MINIAPP_URL` | empty (off) | optional Telegram Mini App (board, console); needs a public `https://` URL you provide |
| `TELEGRAM_API_URL` | `https://api.telegram.org` | Bot API base |
| `LOG_UPDATES` | `0` | log every Telegram update as JSON |

More keys (draft and notice timings, state folders, ports) are listed by `hommy config list`. An unknown key is logged
and ignored; an invalid value stops the service from starting and names the key.

## Services

`hommy setup` installs three launchd agents (`dev.hommy.hub`, `dev.hommy.host`, and `dev.hommy.speech` with
`SPEECH=local`; an instance `X` uses `dev.hommy.X.*`):

```sh
hommy service status [hub|host|speech|all]
hommy service restart host
hommy service stop all
hommy service install            # re-run after an upgrade
hommy service uninstall [--purge]
```

Logs are in `~/.local/state/hommy/<hub|host|speech>/{out,err}.log`. `uninstall` removes the launchd agents;
`--purge` also removes exactly the files, folders and Keychain items hommy created, and never anything else in those
folders (your vault is never touched).

**Signed stable copy.** Services run a copy of the binary at `~/.local/share/hommy/bin/hommy`, signed with a
self-signed certificate created at install in its own keychain (no Apple Developer ID, no dialog). macOS ties privacy
grants (Full Disk Access, Local Network) to a binary's code requirement. An unsigned build is identified by its hash,
which changes with every build and silently drops the grant, so an unattended run would hang on a prompt nobody sees.
The stable path and certificate keep the grant across upgrades.

## Doctor

`hommy doctor` prints one line per check, `✓ <id> <detail>` or `✗ <id> <detail>` followed by `fix: <what to run>`;
exit 0 when nothing is red. `hommy doctor --deep` adds live speech round trips.

| Check | What it verifies |
|---|---|
| `platform` | macOS 14+ on arm64 |
| `agterm` | `agtermctl` answers |
| `claude`, `claude-auth` | `claude` runs; signed in and onboarding complete |
| `config`, `secrets` | the file is valid and 0600; the Keychain items exist |
| `telegram` | the stored token is accepted by Telegram |
| `binary` | the signed copy exists, matches this version and is not hash-pinned |
| `services`, `hub`, `host` | launchd agents run; the hub answers `/healthz`; the host is online |
| `paired` | an owner is paired |
| `vault` | the host can read the vault (`missing` or `denied` come with the fix) |
| `hooks` | the hook relay socket accepts connections |
| `speech` | the speech servers answer (or `SPEECH=off`) |

The installer adds checks for the speech stack, the Claude plugins, the browsers and optional tools.

## More Macs

Run extra hosts on other Macs against the same hub. On the hub Mac:

```sh
hommy config set HUB_LISTEN 192.0.2.10:8470      # a LAN address of this Mac (the hub stays on 127.0.0.1 too)
hommy service restart hub
hommy hub host-key add studio                    # prints a key once
```

On the other Mac (it needs agterm, `claude` and its own vault folder):

```sh
printf %s 'THE-KEY' | hommy setup --join ws://192.0.2.10:8470/ws --name studio --key-stdin
```

The link is a plain WebSocket with a bearer key, so use it on a network you trust. macOS asks the hub Mac to allow
incoming connections for the signed `hommy` (firewall prompt) and the joined Mac to allow **Local Network** access
for it (System Settings > Privacy & Security > Local Network); a missing grant shows as `no route to host`. Then
`/target vault@studio` or `/target new@studio:<dir>:interactive` sends work there. This path is documented but not
covered by the automated tests (the protocol is the same one the tests run over loopback).

## Voices

Answers over `TTS_MIN_CHARS` get a voice note. The agent appends the spoken version as `<tts>...</tts>`, or
`<tts lang="es">...</tts>` for another language. Built in: English (`af_heart`), Spanish, French, Italian, Portuguese and
Hindi, all Kokoro voices from the local speech stack. Add or replace a language with `TTS_LANG_BACKENDS`
(comma-separated `lang|url|model|voice`); any server that speaks the OpenAI `/v1/audio/speech` protocol works, which is
how Japanese, Chinese (extra G2P packages) or Russian (another engine) are added. A language without a backend falls
back to the default voice. A voice failure never blocks the text.

## Risks

- **Anthropic:** running the unmodified `claude` on your own subscription is allowed; hommy never wraps it in an Agent
  SDK harness on subscription OAuth.
- **Skipped permissions:** with `skip` (default) the agent can run commands without asking. Telegram is the only
  door and only the paired owner passes it; use `hommy config permissions ask` if that is too much trust.
- **WhatsApp (optional `wacli`):** it breaches WhatsApp's terms; the risk is low at personal volume, and it is off
  unless you install it.
- **Telegram user-account automation (optional):** the bot side carries no risk; reading your own account does, and is
  opt-in.
- **Signing:** there is no Developer ID. The self-signed certificate only keeps macOS privacy grants stable.

## Development

- [docs/testing.md](docs/testing.md): how end-to-end tests run (fake Telegram, fake speech, a `claude` stub, a scratch
  `HOME`) without touching a real setup. [e2e/README.md](e2e/README.md): the commands.
- [docs/specs/](docs/specs/): the master spec and the phase specs. [docs/protocol.md](docs/protocol.md): the hub, host
  and agent protocol.
- Commits are scanned for personal data before they are made and pushed (`scripts/leak-scan.sh`, see the leak-gate spec);
  forks run the self-test and the secret scan without the identifier list.
