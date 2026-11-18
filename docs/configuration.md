# hommy: configuration

Config keys, the launchd services and their logs, `hommy doctor`, code signing, more Macs, and the optional Mini App.

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
| `TTS_LANG_BACKENDS` | es, fr, it, pt, hi | extra TTS languages ([Voices](usage.md#voices)) |
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

## Signing

hommy has no Apple Developer ID. The release binary is not notarized (the cask clears the download quarantine flag),
and services run a copy of it at `~/.local/share/hommy/bin/hommy`, signed with a self-signed certificate created at
install in its own keychain (no dialog). macOS ties privacy grants (Full Disk Access, Local Network) to a binary's code
requirement. An unsigned build is identified by its hash, which changes with every build and silently drops the grant,
so an unattended run would hang on a prompt nobody sees. The stable path and certificate keep the grant across
upgrades. The certificate only does that: it proves nothing about who built the binary.

## More hosts (more Macs)

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

## Mini App (opt-in)

Off by default. With it on, the bot's 🎯 picker and its "Open" buttons open a Telegram Mini App with the session
board and a console for one session (its screen, files, talk, pause, done). Telegram only opens Mini Apps over a public
`https://` URL, which you provide (a reverse proxy or tunnel you run in front of the listener; hommy sets none up):

```sh
hommy config set MINIAPP_LISTEN 127.0.0.1:8473        # the hub's Mini App listener
hommy config set MINIAPP_URL https://hommy.example.org # the public URL that reaches it
```

Only the paired owner gets in (Telegram's signed `initData`, checked on every request); everything else gets 401.
`hommy config unset MINIAPP_LISTEN` turns it off again.
