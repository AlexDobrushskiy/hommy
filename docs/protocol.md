# hommy: hub, host and agent protocol

Reference for the pieces that talk to each other: the **hub** (Telegram side, one per install), the **host** (one per
Mac, runs the agent sessions), and the **agent** inside an agterm session (`hommy send`, `hommy inbox`, the hooks).
Code: `internal/proto` (wire types), `internal/hub`, `internal/host`, `internal/agterm`, `internal/hook`,
`internal/agentcli`, `internal/sanitize`. Setup and everyday use are in the [README](../README.md).

## 1. Transport

One WebSocket `GET /ws` on `HUB_LISTEN` (default `127.0.0.1:8470`) per host.

| Header | Value |
|---|---|
| `Authorization` | `Bearer <host key>` (Keychain item `host-key` on the host, a `name key` line in `hub-host-keys` on the hub) |
| `X-Hommy-Host` | the host's name (`HOST_NAME`, default the Mac's `LocalHostName`) |

Frames (JSON, one per message), `type` selects the shape:

| Frame | Direction | Carries |
|---|---|---|
| `hello` | host to hub | `{host, version}`; sent first after connect |
| `job` | hub to host | `{id, event_id, type, payload}` |
| `result` | host to hub | `{job_id, status, data, error}` |
| `event` | host to hub | `{job_id, kind, data}` (section 5) |
| `heartbeat` | host to hub | host state (section 4), every 10 s |
| `ack` | both | confirms receipt of a `result` (hub to host) or an `event` (hub to host); carries the id |

The hub reads a heartbeat or frame within **35 s** or drops the connection; the host reconnects with backoff and the
hub re-pushes everything unacked.

## 2. Jobs and results

`Result.status`: `ok` | `inbox` (stored, not typed yet; retried by the host) | `no_session` | `error`. An unknown job
type answers `error`.

| Job | Payload | Does |
|---|---|---|
| `run_p` | `target, prompt, session?, new_dir?, ...` | a streamed `claude -p` run in the vault or a folder (section 7) |
| `save_file` | `name, data, ...` | stores a photo or file the owner sent; the path is typed into the session |
| `cancel` | `job_id` | stops a `run_p` (section 7) |
| `type` | `session, msg_id, text, quote?, reply_to?, kind?` | text to the session's inbox, then typed into its composer (section 9) |
| `inbox_append` | `session or brief_dir, msg_id, text, kind?, quote?, reply_to?` | inbox only, nothing typed |
| `new_session` | `dir, slug, msg_id, text, brief?, kind?, file?{name,ext,data}` | opens an agterm session (section 8) |
| `peek` | `session, lines?` (default 40, max 500) | last screen lines of the primary pane and the composer state |
| `files` | `session or brief_dir, path?, tail?` | lists the brief folder or reads one file (64 KB cap, or last N lines); the path must stay inside the folder, symlinks resolved |
| `pause` / `done` | `session or brief_dir` | writes a `PAUSE` file / sets `state.json` `status: done`, other keys kept in order |

`hommy agterm-job TYPE [--wait] [--payload JSON]` runs one job on the local machine as the host would and prints the
result (a development path; `snapshot` prints the heartbeat data).

## 3. Delivery guarantees

- A job is **stored before it is pushed**. The host answers with its `result` only after an **fsynced** write to its
  ledger; the hub acks that result. Unacked jobs are **re-pushed** on reconnect, and a re-pushed finished job is answered
  from the ledger: processing is **idempotent per `event_id`**.
- **`run_p` is at-most-once.** Before starting, the host writes a fsynced "begun" marker with the process group to its
  ledger. A run that was in flight when the host stopped (clean or crash) is answered
  `interrupted: the host restarted during the run`, and its leftover process group (and its tool-command groups) is
  killed on the next start, unless that group id now belongs to a claude started later. If the answer cannot be
  written, the host refuses to start rather than risk running the job twice. Other job types that must not repeat
  register the same way (`host.RegisterAtMostOnce`).
- Telegram updates are stored before they are processed and deduplicated by message id.
- **Lanes:** one job in flight per target, so queued messages run in order and each `-p` run resumes the session the
  previous one ended on. Lines and files for agent sessions share one lane per host (`type:<host>`), so the host gets
  them in the owner's order even when they name different sessions of one agent. Stop updates and `cancel` have their
  own lanes, never behind a voice note that is still transcribing.
- A hub restart or a dropped WebSocket leaves a `run_p` going; the host keeps the newest progress snapshot and re-sends it
  when the hub re-pushes the job.
- Acks on the owner's message (reactions): 👀 queued for an offline host (plus one `<host> is offline; queued` after
  `QUEUED_NOTICE_AFTER`), ✍ run started or line typed, 👌 stored in the session's inbox because it was busy (typing is
  retried, section 9), 🤷 no live session (kept in the inbox), 🫡 the agent acked it (`hommy inbox ack`).
  A host silent for `HOST_SILENT_AFTER` gives one `host <name> silent` notice, and `<name> is back` on reconnect.

## 4. Heartbeat

Every 10 s, host to hub. Fields: `host`, `version`, `started_at`, `running_jobs`, `claude_sessions`, `agent_folders`
(recent brief folders, newest first), `agterm` (the agterm sessions with agent and composer state
`ready | draft | menu | working | unknown`, the pinned folders and project roots), `hooks` (agterm id to the bridge id of
that terminal's newest Claude Code session, for session links), unacked inbox counts and the spooled agent pings
(section 10); and

```json
"checks": {"vault": "ok|missing|denied", "agterm": "ok|down"}
```

`vault: ok` means the folder exists, the host process can read it and it holds a `CLAUDE.md`; `denied` means macOS
privacy (TCC) blocked the read. `hommy doctor` reports these.

## 5. Events (host to hub)

| Event `kind` | Meaning |
|---|---|
| `started` | a `run_p` began |
| `progress` | snapshot of a run: status line plus the answer text so far (never deltas), at most every `HOST_PROGRESS_EVERY` (500 ms) |
| `type_outcome` | a typed line's final outcome: `delivered | unconfirmed | expired | failed` (+ `no_session`) |
| `hook_answer` | a Telegram turn's final answer from a hook (section 11) |
| `hook_waiting` | a Telegram turn is stuck at a permission or input prompt |
| `hook_resumed` | that prompt is over |
| `spool` | agent pings sent directly to the Bot API, forwarded so the hub can record them (section 10) |

Hook events are durable: they are kept in the host's outbox and re-sent with each heartbeat until the hub acks. The hub
stores a hook answer as a job that already has its result, then sends it (retries, parts sent once, resumed after a
restart, deduplicated per turn id).

## 6. Targets, routing, drafts

**Targets** (one current target per chat, sticky):

| Spec | Meaning |
|---|---|
| `vault@<host>` | `claude -p` in the vault folder of that host |
| `p@<host>:<dir>` | `claude -p` in `<dir>` (must resolve under the host user's home) |
| `session@<host>:<agterm id>` | an interactive session: text, voice transcripts and file paths are typed into it |
| `new@<host>:<dir>:<interactive\|-p>` | the next message creates a session (section 8) |

The default is `DEFAULT_TARGET` (`new@<this host>:<vault>:interactive`). `/target <spec>` switches; `/new_session` starts
a fresh `-p` session for a vault or `p@` target, and for a session target switches back to `DEFAULT_TARGET`.

**Routing rules** for an incoming owner message, first match wins:

1. an inline-button callback `ag:<msgid>:<i>`: the session of that message;
2. a reaction: the session of the reacted message (a `-p` answer receives it as a prompt; an unknown message is ignored);
3. a reply: the target that wrote the replied-to message (from the DB, else the `🤖 name · host` header), the current
   target unchanged. A reply to an agent message whose session is unknown (a bot message the hub never recorded, an
   unresolvable header, a session-less ping or plain send) is **not delivered**: after waiting up to 5 min for a spool
   entry, the owner gets `Not delivered`. A reply to the owner's own line or a hub notice falls to rule 4;
4. otherwise the current target. (An unused `Answer…` press makes the next message go to that agent, once, within 15 min.)

Only the **owner's private chat** is ever routed (README › pairing). The owner is read live from the hub DB.

**Indicators:** every bot message carries a persistent reply keyboard with the current target and the two most recent
others; a pinned line `Talking to: <label>` is edited in place; a switch posts `Now talking to <label>`.

**Drafts:** the hub shows a streaming run as one `sendMessageDraft` per run (stop button, at least `DRAFT_EVERY` apart,
re-sent every `DRAFT_KEEPALIVE`, newest text only, rate limits waited out). A draft is a view: if it fails the answer still
arrives, and none is sent after the answer. Text never waits for voice: the answer text goes out first, the voice note
follows as its own message.

## 7. Runs, stop, cancel

`run_p` runs `claude -p --output-format stream-json --verbose --include-partial-messages` in its own process group.
The environment is clean: session-identity variables (`CLAUDECODE`, `CLAUDE_CODE_ENTRYPOINT`, `CLAUDE_CODE_SESSION_*`,
`CLAUDE_CODE_BRIDGE_SESSION_ID`) are dropped, auth variables (`CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`,
`CLAUDE_CONFIG_DIR`) are kept; the host log lists the `CLAUDE*` names it passed.

- **Stop:** Telegram's draft stop button (`stopped_message_generation`, in the default `ALLOWED_UPDATES`) drops the
  draft and queues a durable `cancel` job (own lane, one per run): SIGTERM to the run's process group and to the groups
  of its tool commands, SIGKILL after `CLAUDE_KILL_GRACE` (5 s), then one `stopped` line with the partial text. The
  run's session id is kept, so the next message resumes it. A stop for an unknown or finished draft is ignored.
- **Timeout:** `CLAUDE_TIMEOUT` (default 30 m) cancels the same way and answers `timed out after ...`.

## 8. New sessions

`new_session` is the default way an errand starts. The host:

1. **Folder rule:** `dir` must be inside the briefs folder (`AGENT_BRIEFS_DIR`) or inside a project root
   (`NEW_SESSION_ROOTS`, default `~/Projects,~/src`) or a pinned folder (`NEW_SESSION_PINNED`, default the vault), symlinks
   resolved through the deepest existing ancestor. A missing folder is created. Roots that do not exist do not count.
2. Makes a **slug** from the message (stop words dropped, at most 4 words and 32 characters, kebab-case; `tg-<msgid>` when
   nothing is left; `-2`, `-3` appended while the brief folder or a live session has the name).
3. Creates the **brief folder** `<AGENT_BRIEFS_DIR>/<date>-<slug>/` (section 12): `brief.md` = the message or transcript
   (a photo or file is saved there and its path typed), `state.json.active_agent` = the new agterm session id.
4. Opens an agterm session in workspace `AGTERM_WORKSPACE` (default `hommy`) running `AGENT_COMMAND` (the command comes
   from config, never from a job), named by the slug, in `dir`; accepts the folder-trust dialog for the folder chosen;
   waits for a ready composer (never ready within 2 minutes: the session is closed and the brief gets
   `status: failed`); then types the first message with the `tg-<msgid>` prefix, so its answer returns as a reply to it.

The bot posts `started <slug> on <host>` (brief folder, short session id), after waiting up to `NEW_LINK_WAIT` for the
session's link from the hooks. The new session becomes the target unless the owner switched elsewhere meanwhile. Answers
from that session wait for the started line, so it always comes first (at most 3 min).

- Messages that arrive while the session is created queue behind the job and are typed into it, in order. Never a second
  session for one target or one event (the host's `new-sessions.json` resumes a re-pushed job).
- **Failure** (refused folder, agterm error, composer never ready): one `new session on <host> failed: ...` line, the
  target stays `new@...`, lines queued behind it get `wasn't sent`.
- **`-p` mode:** the next message switches the target to `p@<host>:<dir>` with a fresh session and runs as its first
  streamed `-p` (`run_p` with `new_dir`, same folder rule); a first run that fails before the session started puts the
  `new@...` target back.

## 9. Typing and the inbox

**Typed line** (`internal/sanitize`): `[Telegram · tg-<msgid>[ · re "<quoted head>"]] <text>`. The text is NFC
normalised; C0/C1 controls, ESC, DEL and format characters are removed; tab and space runs become one space; newlines
become ` ⏎ `; the line is capped at 1500 characters (the full text is in the inbox). The hook relay matches only this
prefix form. Reactions are typed with their meaning (`👍 yes / continue`, `❤ 👍 + good work`, `👎 no / drop`,
`🤔 explain / options`, `👀 seen, nothing to do`, `🔥 urgent`, a removed reaction `retracted 👍`); a pressed button is typed
as its label. Item ids are `tg-<msg>`, `tg-<msg>-r<n>` (reaction), `tg-<msg>-b<n>` (button).

**Composer states** (`hommy compose-state --target ID [--json]`; exit 0 ready, 1 not ready, 2 error):

| State | Meaning |
|---|---|
| `ready` | empty composer at an idle prompt, cursor at column 2, no running turn |
| `draft` | someone's unsent text in the composer |
| `menu` | a permission, selection or folder-trust dialog |
| `working` | a turn is running (spinner row, the title Claude sets, or agterm status `active`) |
| `unknown` | no Claude Code composer on screen (a shell, another program): never typed into |

**Typing a line** (`type` job): the full text goes to the **inbox first** (fsync). The line is typed into the session's
primary pane only when the composer is `ready`: bounded chunks, each verified on screen through a probe marker, the
body verified before Return, Return repeated (up to 3 times) while the composer still holds exactly our line, then the
composer must clear. A failed check backspaces only the text we typed. Not ready: result `inbox` and a host-side retry
**every 60 s for 30 min**, in order per agent (brief folder; a session without one: per session), persisted in
`<state>/host/agterm/typing-queue.json` so it survives restarts (✍ when it finally lands, `⌛`/`⚠️` notices while it
waits).

**Handover:** a line for a session that has ended is typed into the successor named by its brief folder's `state.json`
(`active_agent`; `handover_from` is accepted), within the same 30 min window; with no successor the result is
`no_session` and the line stays in the inbox.

**Inbox:** `<brief dir>/inbox.jsonl`, where the brief dir is the folder whose `state.json` names the session
(`active_agent`, `active_watcher` or `overseer`) or the folder the session's cwd is in; otherwise
`<state>/inbox/<session id>.jsonl` (default `~/.local/state/hommy/inbox/`). One JSON object per line:
`id, event_id, ts, from, via, kind, host, session, text, quote, reply_to`.

`hommy inbox` (run inside an agent session; it finds the inbox from `$AGTERM_SESSION_ID`):

| Command | Does |
|---|---|
| `list [--unacked]` | the items as JSON |
| `ack ID` | appends to `inbox.acked.jsonl`, `POST /api/ack` (the owner sees 🫡; spooled if the hub is down) |
| `pending` | exit 0 when unacked items exist, 1 when none |
| `wait [--pending] --timeout S` | prints `{"event":"owner","item":...}` and exits 0 on the next new item (with `--pending`: also on an unacked one already there), or `{"event":"timeout"}` and exits 1 |

## 10. Agent pings: `hommy send`

`hommy send [--agent SLUG [--dir D] [--buttons "A|B|Answer..."] [--ask]] [--silent] [--json] [--env-file F] TEXT|-`
sends to the owner's chat. `--` ends the options; an unknown flag exits 2 and sends nothing.

- **Through the hub** (host env file with `HUB_URL`, `HOST_NAME` and the host key): `POST /api/send` with the host key,
  idempotent per request id. A plain send (no `--agent`) from an agterm session carries the session and is recorded with
  it, so a reply reaches the session; outside a session it is recorded `plain` and a reply is not delivered.
- With `--agent` the hub sends `🤖 <slug> · <host>` + text (+ the session link) and records message, session
  (`$AGTERM_SESSION_ID`), brief folder and `pings` row before answering `{"message_ids":[...]}`. Buttons are sent without the
  reply keyboard, then `ag:<msgid>:<i>` callbacks are attached; a press answers at once, edits the message to
  `→ <label> ✓` and types the label into the session. A label ending in `...` sets a one-shot answer context; `--ask` adds
  `Answer...` (no `force_reply`: it would clear the target keyboard). `--buttons` and `--ask` need `--agent`.
- **Never duplicates:** the hub checkpoints each part under the request id (a retry resumes; buttons that did not attach
  are reported and retried). The CLI goes around the hub only when no attempt reached it (connection refused every time);
  a lost answer or a 5xx is an error, never a second send.
- **Direct fallback** (token and chat id in the config, no hub): the Bot API plus a spool file
  `<state>/spool/<id>.json` naming its `HUB_URL` and bot. The host forwards the entries for its `HUB_URL` with each
  heartbeat (`spool` event); the hub records only its own bot's entries (never re-sends) and acks once stored, then the
  host deletes them. A press, reaction or reply on an agent message the hub does not know yet waits (event `parked`, at
  most 5 min) for that spool entry, then routes. `--agent` against a config without a hub warns that nothing can route back.

## 11. Hooks: answers back from interactive sessions

Flow: Claude Code hook, `hommy hook <event>`, the **local** host's relay (unix socket `HOOK_SOCKET`, default
`<state>/host/hook.sock`, 0600), a durable outbox event, the hub, the chat.

| Subcommand | Claude Code event | Does |
|---|---|---|
| `session-start` | SessionStart | registers the agterm id (`AGTERM_SESSION_ID`), session id, cwd, transcript |
| `user-prompt-submit` | UserPromptSubmit | records the bridge id and whether the prompt starts with `[Telegram · tg-<id>` (that id is the turn's reply target) |
| `stop` | Stop | Telegram turns only: `last_assistant_message` (fallback: the transcript's last assistant text) becomes the answer; the first durably accepted Stop ends the turn |
| `notify` | Notification (not `idle_prompt` / `auth_success`) | Telegram turns only, once per prompt: a `waiting at the Mac` ping |

- **A hook run never prints and always exits 0:** stdin and the post give up after 2.5 s, a watchdog exits at 4 s. The
  plugin's shim runs it with output to `/dev/null`. One line per run goes to `<state>/host/hook.log` with its duration.
- **Local turns stay local:** a Telegram prompt leaves a marker in `<state>/host/hookturns/`; `stop` and `notify` without
  one return at once (`local turn` in the log). A local prompt sends only the session ids, never its text.
- **Host down:** a Telegram turn's requests are spooled to `<state>/host/hookspool/` (local ones are dropped) and handled
  in order when the host is back, one at a time.
- Per-session state lives in `<state>/host/hookrelay.json` (a turn survives a restart; one without its Stop for 24 h is
  dropped). A request is acked only once its state is on disk; answer ids depend only on the request, so a replay gives
  the same id.
- **Which session answered:** for a Telegram turn the relay uses the agterm session the host typed `tg-<id>` into, else
  the hook's `AGTERM_SESSION_ID`.
- **The answer:** sent as a reply to the owner's message, every chunk tagged with the session label, the last ending in
  the session link when Remote Control gave one (`CLAUDE_CODE_BRIDGE_SESSION_ID` exists only then; without it the answer
  has no link). A `<tts lang="xx">...</tts>` block in the answer is stripped and sent as a voice note after the text.
- **Waiting at the Mac:** a turn stuck at a prompt sends one ping per prompt with an inline Open button (when the Mini App
  is configured). When the prompt ends (answered, declined, interrupted, typed at the Mac, session exited) the host sees it
  in the next composer reading (a live composer taken at least 3 s after the hook) or hook and sends a durable
  `hook_resumed`; the hub edits the ping to `no longer waiting`.
- A background task's completion absorbed before the Stop continues the turn; one starting a new turn afterward stays local.

## 12. Brief folders

`<AGENT_BRIEFS_DIR>/<date>-<slug>/` (default `~/.local/share/hommy/briefs`) is the durable home of one errand.

| File | Content |
|---|---|
| `brief.md` | the first message or transcript, plus whatever the agent adds |
| `state.json` | `created_by` (`"hommy"`), `active_agent` (agterm session id of the live agent), optional `active_watcher`, `overseer`, `handover_from`, `status` (`failed`, `done`), other keys preserved in order |
| `inbox.jsonl`, `inbox.acked.jsonl` | owner lines and the acked ids (section 9) |
| `PAUSE` | written by the `pause` job |
| `log.md` | the agent's own log; the console shows its last 200 lines |

An agent that is not started by hommy writes `{"active_agent": "<$AGTERM_SESSION_ID>"}` to `state.json` at start so
replies to a successor still find the folder.

## 13. Agent-brief conventions

Paste this section into a brief for a long-running agent (a watcher, an overseer, an errand with a wait loop) that the
owner may talk to from Telegram. Replace `<slug>` with the agent's session name.

```markdown
## Telegram (hommy)
- The owner can write to you from Telegram. Their lines are typed into this session as
  `[Telegram · tg-<id> · re "..."] <text>` (a reaction arrives as its meaning, e.g. `👍 yes / continue`; a button as its
  label): act on it like a line typed here. Your final answer to that turn goes back to their chat by itself.
- On start, handle `hommy inbox list --unacked` (lines that came while you were busy, or for your predecessor). The wait
  loop also returns `{"event":"owner"}` when `hommy inbox pending` exits 0. Ack each item after handling it
  (`hommy inbox ack <id>`); do not ack what you hand over to a successor.
- Ping the owner only with `hommy send --agent <slug> "<text>"` (`--buttons "A|B|Answer..."` for a choice, `--ask` for a
  free answer, `--silent` at night), so a reply, reaction or button press comes back to you. A raw Bot API send cannot.
- Push only what needs the owner now: a decision or approval that blocks progress today, with `--buttons` or `--ask`.
  No FYI, progress or confirmation pings ("sent", "scheduled", "noted"): those go to your overseer or a digest, or wait
  until the owner asks. A Telegram line gets one reply in that turn and nothing after it; a result asked for later is
  one ping.
```

**Wait loop.** A script that blocks until something happens checks the inbox on every poll and returns at once when the
owner has written:

```python
import json, subprocess, sys
def owner_pending():
    try:
        return subprocess.run(["hommy", "inbox", "pending"], capture_output=True, timeout=10).returncode == 0
    except Exception:
        return False
# in the loop, before the other checks:
if owner_pending():
    print(json.dumps({"event": "owner"})); sys.exit(0)
```

In a shell, `hommy inbox wait --pending --timeout 1500` does it in one call (ends at once on an unacked item already
there or on a new one). Not `pending || wait`: an item landing between the two calls would wait out the timeout.

## 14. Hub HTTP API (on `HUB_LISTEN`)

| Endpoint | Auth | Answer |
|---|---|---|
| `GET /healthz` | none | `200 {"ok":true,"version":...}` when the DB answers and the Bot API was reached in the last 3 min; else `503` |
| `GET /api/status` | host key | `{"version","telegram":"ok\|stale\|unauthorized","paired":bool,"owner":{"first_name"},"hosts":[{"name","online","last_seen","version","checks"}]}` |
| `POST /api/pairing` | host key, loopback only | `{}` makes a new code `{"code":"ABCD-EFGH","expires_at","bot":"@name","link":"https://t.me/<bot>?start=<code>"}` (replaces a pending one); `{"reset":true}` also clears the owner; `{"owner_id":N}` binds directly (operator path, no code) |
| `GET /api/pairing` | host key | `{"paired":bool,"pending":bool,"expires_at"}` |
| `POST /api/send` | host key | an agent ping (section 10), answers `{"ok","chat_id","message_ids","warning"}` |
| `POST /api/ack` | host key | marks an inbox item acked (the owner sees 🫡) |
| `GET /ws` | host key | the host connection (section 1) |

The optional Mini App (`MINIAPP_LISTEN`, `MINIAPP_URL`; off by default) is a separate listener with Telegram `initData`
authentication (HMAC, `auth_date` under 1 h, the owner only; 401 otherwise, 30 requests per minute per user).

## 15. Pairing

- **Code:** 8 Crockford-base32 characters shown `XXXX-XXXX` (40 bits), accepted case-insensitively with or without the
  dash; stored as a SHA-256 hash with an expiry (`PAIRING_TTL`, default 30 m); single use.
- **Unpaired hub:** a private-chat `/start <code>` from any user with the pending code makes that user the **owner**
  (`owner(user_id, chat_id, first_name, paired_at)`, at most one row), consumes the code, answers
  `Paired with <bot>. Send a text or a voice note.` with the help text and the reply keyboard, and sets the bot's
  commands. A wrong or expired code, a group chat or any other update is dropped without a reply, with one log line
  `dropped update <id>: <reason>` (user id only). **Per user**, 5 wrong codes make the hub ignore that user's `/start`
  until a new code is made, so nobody can burn the owner's code. An unpaired hub sends nothing to anyone.
- **Paired hub:** only the owner's private chat is routed; a `/start <code>` from anyone else is dropped. Re-pairing:
  `hommy pair --reset` at the Mac.
- After pairing, `hommy pair` and `hommy setup` write `OWNER_CHAT_ID` into the config for the `send` direct fallback;
  the hub itself always reads the owner from its DB.
