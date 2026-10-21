# WhatsApp MCP Server

[![License: MIT](https://img.shields.io/github/license/Sealjay/mcp-whatsapp)](LICENSE)
[![CI](https://github.com/Sealjay/mcp-whatsapp/actions/workflows/ci.yml/badge.svg)](https://github.com/Sealjay/mcp-whatsapp/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/Sealjay/mcp-whatsapp)](https://goreportcard.com/report/github.com/Sealjay/mcp-whatsapp)
[![Go 1.26+](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![MCP](https://img.shields.io/badge/MCP-protocol-6366f1)](https://modelcontextprotocol.io/)
[![44 tools](https://img.shields.io/badge/tools-44-blue)]()
[![whatsmeow](https://img.shields.io/badge/whatsmeow-multidevice-25D366?logo=whatsapp&logoColor=white)](https://github.com/tulir/whatsmeow)
[![Sealjay/mcp-whatsapp MCP server](https://glama.ai/mcp/servers/Sealjay/mcp-whatsapp/badges/score.svg?)](https://glama.ai/mcp/servers/Sealjay/mcp-whatsapp)
[![GitHub issues](https://img.shields.io/github/issues/Sealjay/mcp-whatsapp)](https://github.com/Sealjay/mcp-whatsapp/issues)

A single-binary Go [MCP](https://modelcontextprotocol.io/) server that wraps [whatsmeow](https://github.com/tulir/whatsmeow) to expose a personal WhatsApp account to LLMs. `whatsapp-mcp serve` runs as a lightweight HTTP daemon on `127.0.0.1:8765`; MCP clients (Claude Desktop, Cursor, Claude Code, etc.) connect to it via HTTP — no process spawning, no stdin/stdout juggling. Messages are cached in local SQLite and only travel to the model when the agent calls a tool.

> **Unaffiliated.** This is an independent open-source project. It is not affiliated with, endorsed by, or otherwise associated with Meta Platforms, Inc., WhatsApp, or [whatsmeow](https://github.com/tulir/whatsmeow). "WhatsApp" is a trademark of Meta Platforms, Inc., used here nominatively to describe interoperability.

This started as a fork of [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) and has since been rewritten as a single Go binary. What it adds over the original:

- **LID resolution** — normalises `@lid` JIDs to real phone numbers for accurate contact matching.
- **Sent-message storage** — outgoing messages are persisted locally so conversation history stays complete.
- **Disappearing-message timers** — outgoing messages inherit the group chat's ephemeral timer automatically.
- **Targeted history sync** — on-demand per-chat backfill via the `request_sync` tool.
- **Extended tool surface** — 44 tools (see below): reactions, replies, edits, revoke, mark-read, typing, is-on-whatsapp, full group admin, blocklist, polls (create + vote + tally), contact cards, view-once flag, presence, privacy settings, the profile "About" text, and local voice-note transcription.
- **Single-instance enforcement** — a `flock(2)` on `store/.lock` prevents two `serve` processes racing on the same SQLite files.

## Setup

### Prerequisites

- Go 1.26+ (build-time only; runtime needs just the compiled binary).
- An MCP client that speaks HTTP (Claude Desktop, Cursor, Claude Code, etc.).
- FFmpeg (_optional_) — required only for `send_audio_message` when the input is not already `.ogg` Opus. Without it, use `send_file` to send raw audio.
- **Windows:** CGO must be enabled — see [docs/windows.md](docs/windows.md).

### Install

```bash
git clone https://github.com/Sealjay/mcp-whatsapp.git
cd mcp-whatsapp
make build    # writes ./bin/whatsapp-mcp
```

### Pair your phone (first run only)

Start the daemon, then open the pairing page in a browser:

```bash
./bin/whatsapp-mcp serve          # starts on 127.0.0.1:8765
open http://127.0.0.1:8765/pair   # macOS; or visit the URL manually
```

Scan the QR with WhatsApp (*Settings → Linked Devices → Link a Device*). The session persists to `./store/whatsapp.db`; WhatsApp rotates it roughly every 20 days, when `/pair` shows a fresh QR.

- **No browser:** `./bin/whatsapp-mcp login` renders the QR in the terminal.
- **No QR scan:** use the phone-number form on `/pair` (or `login -phone +<international number>`), then type the code into *Link a Device → Link with phone number instead*.
- The `/pair/*` endpoints are CSRF-protected and rate-limited (15 GET/min, 1 reset/min, 3 code requests/min).

### Connect your MCP client

`serve` listens on `127.0.0.1:8765` (or `$WHATSAPP_MCP_ADDR`) and serves MCP at `/mcp`. It is stateless (no `Mcp-Session-Id`) and speaks MCP 2026-07-28 as well as older revisions.

```jsonc
// Claude Code (.claude/mcp.json or ~/.claude/mcp.json) and Cursor (~/.cursor/mcp.json)
{ "mcpServers": { "whatsapp": { "type": "http", "url": "http://127.0.0.1:8765/mcp" } } }

// Claude Desktop only launches stdio servers, so bridge with mcp-remote
// (~/Library/Application Support/Claude/claude_desktop_config.json)
{ "mcpServers": { "whatsapp": { "command": "npx", "args": ["-y", "mcp-remote", "http://127.0.0.1:8765/mcp"] } } }
```

Restart the client. The daemon outlives client restarts.

### Sending and receiving files

`WHATSAPP_MCP_MEDIA_ROOT` (default `<store>/uploads/`, created on first run) is the only directory file tools may touch:

- **Sending:** `send_file` and `send_audio_message` take a `media_path` under the root.
- **Receiving:** `download_media` caches to `<store>/<chat_jid>/<hex(message_id)>/<safe_filename>` (checked against the stored size and SHA-256). An optional `output_path` also places the file under the root; an existing file there makes the call a no-op.

Paths outside the root, including symlinks that resolve outside it, are rejected. Anything inside the root is readable and writable by the tools, so keep secrets out.

### Media retention

Downloaded files stay in the cache until something deletes them. Two ways to clean up, both deleting only cached media files and the folders left empty:

| How | What it does |
|---|---|
| `whatsapp-mcp [-store DIR] prune -media-older-than 30d` | One-off cleanup. `-dry-run` only reports what would go. Refuses to run while `serve` holds the store lock (a dry run still works then); stop `serve` first or use the flag below. |
| `whatsapp-mcp serve -media-retention 30d` | Same cleanup at startup and then every 24 hours, inside the daemon. Default `0` = off. |

Durations take whole days (`30d`) or Go durations (`720h`); the minimum is `1h`. A file's age counts from when `download_media` last fetched or handed it out. Message rows and cached transcripts stay, and `download_media` downloads a pruned file again when asked, as long as WhatsApp still has it on its servers; older media may be gone there for good. `messages.db`, `whatsapp.db`, `.lock`, the media root and anything not laid out as `<chat_jid>/<hex(message_id)>/<file>` are never touched, and symlinks or junctions are not followed. On Windows a store reached through a junction is left alone entirely, and a media root that contains the store makes pruning refuse to run.

**Sandboxed clients (Claude Cowork and similar)** cannot read the daemon's cache. Point `WHATSAPP_MCP_MEDIA_ROOT` at a directory the sandbox can read and give the client this instruction:

> When calling the WhatsApp MCP's `download_media`, always pass `output_path` set to a path under your shared workspace. Without it the decrypted file lands only in the daemon's local cache, which is outside your sandbox and unreadable. `output_path` must live under `WHATSAPP_MCP_MEDIA_ROOT` on the daemon side; the basename is yours to pick.

### Serve flags and environment

- `-addr host:port` (env `WHATSAPP_MCP_ADDR`, default `127.0.0.1:8765`).
- `-allow-remote`: bind a non-loopback address. Requires `WHATSAPP_MCP_TOKEN`, a bearer token for `/mcp` and `/pair/*`; `serve` exits without it.
- `WHATSAPP_MCP_MEDIA_ROOT`: the file allowlist root above.
- `WHATSAPP_MCP_DEBUG=1` or `-debug`: verbose logs (see [Debug logging](#debug-logging)).
- `-read-only` (global, before the command) or `WHATSAPP_MCP_READ_ONLY=1`: register only the 17 tools without outbound effect (`search_contacts`, `list_messages`, `list_unanswered`, `list_chats`, `get_chat`, `get_message_context`, `download_media`, `transcribe_media`, `request_sync`, `get_status`, `pairing_status`, `list_groups`, `get_group_info`, `get_blocklist`, `get_privacy_settings`, `get_poll_results`, `is_on_whatsapp`). Nothing that sends, edits, deletes, sends read receipts or changes groups and privacy is reachable, whatever the client calls. `get_group_invite_link` is left out because it can reset the link. An explicit flag wins over the env var, e.g. `-read-only=false`.
- `-media-retention 30d`: delete cached media not used for that long, at startup and every 24 hours (see [Media retention](#media-retention)). Default `0` = off.
- `-keep-revoked` (global, before the command) or `WHATSAPP_MCP_KEEP_REVOKED=1`: keep the text of messages their sender deleted for everyone. By default the daemon blanks the stored text and media keys of such a message, deletes a file `download_media` already cached for it and shows it as `[deleted by sender]`; with this switch the line keeps the text after the marker. Either way `revoked_at` is recorded. An explicit flag wins over the env var.
- `WHATSAPP_MCP_WHISPER_BIN` and `WHATSAPP_MCP_WHISPER_MODEL`: turn on `transcribe_media`. The first is the [whisper.cpp](https://github.com/ggml-org/whisper.cpp) CLI (`whisper-cli`, on Windows `whisper-cli.exe`), the second a ggml model file such as `ggml-base.bin`. Transcription runs on this machine; ffmpeg must be on `PATH`. Without both variables the tool answers with an error that names them.
- `WHATSAPP_MCP_WHISPER_LANG`: spoken language passed to whisper.cpp, e.g. `de`; default `auto` (detect).
- `WHATSAPP_MCP_WHISPER_TIMEOUT`: time limit for converting and transcribing one message, as a duration (`90s`, `5m`) or seconds; default `5m`.
- `WHATSAPP_MCP_WHISPER_MAX_DURATION`: longer audio is not transcribed; same format, default `15m`.

## Running the daemon

Events reach SQLite **only while `serve` runs**. After downtime, whatsmeow backfills via `HistorySync` within WhatsApp's own retention window; anything older is lost, and `request_sync` backfills one chat on demand. A `flock(2)` on `store/.lock` allows one `serve` (or `login`) per store.

| Option | How |
|---|---|
| macOS launchd | Copy `docs/launchd/com.sealjay.whatsapp-mcp.plist` to `~/Library/LaunchAgents/`, fill `{{PATH_TO_REPO}}` / `{{STORE_DIR}}`, `launchctl load` |
| Linux systemd | Copy `docs/systemd/whatsapp-mcp.service` to `~/.config/systemd/user/`, fill placeholders, `systemctl --user enable --now whatsapp-mcp` |
| Claude Code hook | Put `docs/hooks/setup.sh` in `.claude/hooks/` and call it from `settings.json` (idempotent) |
| Manual | `./bin/whatsapp-mcp serve` |

**Docker.** The image (`debian:bookworm-slim` plus ffmpeg, a few hundred MB) runs `-store /home/app/store`; persist that volume. Pair with a one-off `login` container first, because `login` and a running `serve` cannot share the store lock:

```bash
docker build -t whatsapp-mcp --build-arg VERSION=$(git describe --tags --always) .
docker run --rm -it -v whatsapp-mcp-store:/home/app/store whatsapp-mcp login
TOKEN=$(openssl rand -hex 32); echo "$TOKEN"
docker run -d --name whatsapp-mcp -v whatsapp-mcp-store:/home/app/store \
  -p 127.0.0.1:8765:8765 -e WHATSAPP_MCP_TOKEN="$TOKEN" \
  whatsapp-mcp serve -addr 0.0.0.0:8765 -allow-remote
```

Clients then send `Authorization: Bearer $TOKEN`. The container binds loopback by default, so `-allow-remote` plus a token is what makes `-p` work. To re-pair after WhatsApp rotates the session (about every 20 days), stop the container, rerun the `login` container and start it again.

## Architecture

```
cmd/whatsapp-mcp/       login / serve / smoke / prune subcommands
internal/client/        whatsmeow client wrapper (send, download, events, history, features)
internal/daemon/        HTTP server, pairing state machine, /pair endpoints
internal/mcp/           mark3labs/mcp-go v1 server + tool registrations
internal/media/         ogg parsing, waveform synthesis, ffmpeg and whisper.cpp shell-out
internal/mediacache/    layout of the downloaded-media cache, pruning and per-message removal
internal/security/      path allowlisting, filename sanitisation, log redaction
internal/store/         SQLite cache, LID resolution, query layer
```

Data lives under `./store/` (`-store DIR`): `messages.db` (message cache), `whatsapp.db` (whatsmeow session) and `.lock`. Query tools read SQLite; send and action tools call whatsmeow; incoming events are written to SQLite by a goroutine in the same process.

All times are UTC. `messages.db` stores them as `2026-10-20 09:27:49+00:00`, `list_messages` prints them as `2026-10-20T09:27:49Z`, and JSON results use RFC 3339 with `Z`. Older builds stored the local offset of the machine; the first start after an upgrade rewrites those rows to UTC once and records that in `PRAGMA user_version`.

Edits and deletes for everyone update the cached message instead of being stored as messages of their own. An edit replaces the text and sets `edited_at`; an edit older than the one already stored is ignored, so a history sync cannot roll the text back. A delete sets `revoked_at` and, unless `-keep-revoked` is set, blanks the text and media keys, and also drops the copy of the text that replies to it carry. `list_messages` marks such lines `[edited]` or `[deleted by sender]`, and `get_message_context` returns both fields. A reply to a deleted message shows `[deleted by sender]` as its quoted text (followed by the media marker, or by the kept text with `-keep-revoked`). A change is applied only when it comes from the message's author (or one of your own devices for your own messages); a group admin deleting someone else's message is ignored.

Message types without text of their own are stored with a readable line instead of being dropped: `[location] name, address (lat,lon) https://maps.google.com/?q=lat,lon`, `[live location] …`, `[contact] Name, TEL …` (read from the vCard) and `[contacts: n] names`. Stickers are stored as media type `sticker` and video notes as `video`; `download_media` fetches both. A message this device could not decrypt is stored as `[message could not be decrypted - check the phone]`; when WhatsApp's automatic retry succeeds, the real message replaces it. Reactions are kept in a table of their own, so they never show up as a chat's last message or as a message in `list_unanswered`. `list_messages` ends a reacted-to line with `[reactions: 👍 Name, ❤️ Me]`, and `get_message_context` returns them as `reactions`. Removing a reaction removes it from the cache as well.

## Tools

44 tools, grouped by purpose.

### Read / query

| Tool | Purpose |
|---|---|
| `search_contacts` | Substring search across cached contact names and phone numbers |
| `list_messages` | Query + filter messages; returns formatted text with context windows. `after` (inclusive) and `before` (exclusive) take RFC 3339 with `Z` or an offset, a time without offset (read as UTC) or a bare `YYYY-MM-DD` (00:00 UTC) |
| `list_unanswered` | Chats where someone wrote and is still waiting for your reply: incoming messages after your own last message and after `since` (default 7 days ago, same time formats as `list_messages`). Longest wait first, with count, first/last waiting time and the newest waiting message. Every own message counts as a reply, stickers, locations and contact cards included, and so does your own emoji reaction to an incoming message: it answers that message and everything before it. Groups only with `include_groups: true`; status updates, broadcast lists, channels and deleted messages are left out |
| `list_chats` | List chats with last-message preview; sort by activity or name |
| `get_chat` | Chat metadata by JID |
| `get_message_context` | Before/after window around a specific message, with each message's emoji reactions |
| `download_media` | Download persisted media to a local path |
| `transcribe_media` | Transcribe a voice note or other audio message locally with whisper.cpp (see `WHATSAPP_MCP_WHISPER_*` above). The audio is fetched like `download_media`, nothing is sent to the chat. The transcript is cached: repeat calls return it straight away, `list_messages` shows it on the line (`[voice note, 45 s] transcript: "…"`) and its `query` searches it. Deleting a message for everyone also deletes its transcript unless `-keep-revoked` is set |
| `request_sync` | Ask WhatsApp to backfill history for a chat |

### Send

| Tool | Purpose |
|---|---|
| `send_message` | Send a text message to a phone number or JID |
| `send_file` | Send image/video/document/raw audio with optional caption; `view_once: bool` marks image/video/audio submessages as view-once (ignored for documents) |
| `send_audio_message` | Send a voice note (auto-converts via ffmpeg if not `.ogg` Opus); supports `view_once: bool` |
| `send_poll` | Send a poll with a question and 2+ options; `selectable_count` controls how many options a voter may pick. Generates the 32-byte `MessageSecret` required for votes to decrypt |
| `send_poll_vote` | Cast a vote on a previously-seen poll; `options` must match option names exactly |
| `get_poll_results` | Return the tally for a poll we have cached (includes 0-vote options) |
| `send_contact_card` | Send a contact card; synthesises a vCard 3.0 from `name` + `phone`, or pass a raw `vcard` to skip synthesis |

### Message actions

| Tool | Purpose |
|---|---|
| `mark_read` | Mark specific message IDs as read |
| `mark_chat_read` | Ack the most recent incoming messages in a chat to clear the unread badge |
| `send_reaction` | React to a message (empty emoji clears an existing reaction) |
| `send_reply` | Text reply that quotes a prior message |
| `edit_message` | Edit a previously-sent message |
| `delete_message` | Revoke (delete for everyone) a message |
| `send_typing` | Set per-chat composing / recording presence |

### Groups

| Tool | Purpose |
|---|---|
| `create_group` | Create a group with a name and initial participants |
| `leave_group` | Leave a group |
| `list_groups` | List all groups the user is a member of |
| `get_group_info` | Full group metadata (participants, settings, invite config) |
| `update_group_participants` | Add / remove / promote / demote participants (`action: add\|remove\|promote\|demote`). Checks first that you are an admin; people whose privacy settings block a direct add get a group-invite card in their 1:1 chat (`send_invite`, default true; optional `invite_message` caption) |
| `set_group_name` | Change the group subject |
| `set_group_topic` | Change the group description; empty string clears it |
| `set_group_announce` | Toggle announce-only mode (only admins can send) |
| `set_group_locked` | Toggle locked mode (only admins can edit group metadata) |
| `get_group_invite_link` | Get the invite link; `reset: true` revokes the previous link first |
| `join_group_with_link` | Join a group via a `chat.whatsapp.com` URL or bare invite code |

### Blocklist

| Tool | Purpose |
|---|---|
| `get_blocklist` | Return the current blocklist (JIDs, usually `@lid`, plus phone numbers where known) |
| `block_contact` | Block a contact by phone number or JID |
| `unblock_contact` | Unblock a contact |

### Privacy / presence / status

| Tool | Purpose |
|---|---|
| `send_presence` | Set own availability (`available` or `unavailable`) — distinct from per-chat `send_typing` |
| `get_privacy_settings` | Current privacy settings as JSON |
| `set_privacy_setting` | Change one privacy setting by `name` + `value` (strict enum validation; invalid combinations are rejected) |
| `set_status_message` | Update the profile "About" text; empty string clears it |

### Admin

| Tool | Purpose |
|---|---|
| `is_on_whatsapp` | Batch-check which phone numbers are registered on WhatsApp |
| `get_status` | Report whether the bridge is connected and which account it's paired as |
| `pairing_status` | Report the device-pairing state as a structured `setup_state` envelope (`ready` / `awaiting_qr` + `qr_payload` / `error`) for programmatic supervisors that surface the linking QR |

### Deferred

Intentionally not exposed yet:

- **`subscribe_presence`** — no persistence layer for presence events, skipped to avoid a dangling tool.
- **Profile photo setter** — upstream whatsmeow doesn't expose a user-level setter.
- **Approval-mode participants, communities, newsletters** — low-use surface, deferred.

## Limitations

- **Prompt-injection risk:** as with many MCP servers, this one is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). Prompt injection in incoming messages could lead to private data exfiltration — treat the tool surface accordingly.
- **Message gaps:** see [Running the daemon](#running-the-daemon).
- **Single instance per store:** only one `whatsapp-mcp serve` can hold the store lock. Parallel MCP clients must point at different `-store` directories (and therefore different paired sessions).
- **Windows:** requires CGO and a C compiler — see [docs/windows.md](docs/windows.md).
- **Upstream bounds:** message fetch/send is bounded by what [whatsmeow](https://github.com/tulir/whatsmeow) supports against the WhatsApp web multidevice API.

## Development

```bash
make test          # unit tests
make test-race     # with -race
make vet           # go vet
make e2e           # build + JSON-RPC smoke over HTTP (requires -tags=e2e)
make smoke         # boot-test the server without connecting to WhatsApp
```

### Upgrading whatsmeow

Weekly CI runs an upstream upgrade probe. To do it manually:

```bash
make upgrade-check
```

This bumps `go.mau.fi/whatsmeow@main`, re-tidies, builds, and tests. If green, commit the `go.mod` / `go.sum` changes.

`scripts/mdtest-parity.sh` in CI fails the build early if upstream removes or renames any whatsmeow method we call — it's the canary for API drift.

## Troubleshooting

- **`connect failed …` on `serve`** — the daemon is not paired. Open `http://127.0.0.1:8765/pair` in a browser and scan the QR. Alternatively, run `./bin/whatsapp-mcp login` in a terminal.
- **`another whatsapp-mcp instance is already running`** — only one `serve` can hold the store lock. Check for a stray process (`ps aux | grep whatsapp-mcp`) or another MCP client pointed at the same `-store` directory.
- **QR doesn't display** — the terminal doesn't render half-block Unicode. Try iTerm2, Windows Terminal, or similar.
- **Device limit reached** — WhatsApp caps linked devices. Remove one from *Settings → Linked Devices* on your phone.
- **No messages loading** — after initial auth, it can take several minutes for history to backfill. Use `request_sync` to target a specific chat.
- **WhatsApp out of sync** — delete both database files (`store/messages.db` and `store/whatsapp.db`) and re-run `login`.
- **`ffmpeg not found`** — `send_audio_message` needs ffmpeg on `PATH` to convert non-Opus audio. Use `send_file` for raw audio instead.

### Debug logging

Log lines start with a UTC timestamp ending in `Z` (`2026-10-21T09:12:33.123Z`), and message times inside them are UTC too, so they match the times in tool output.

By default, stderr logs redact JIDs to `…<last-4>`, shorten message IDs to `…<last-6>`, summarise bodies as `[<length>B: text|url|command]`, show contact, group and file names only as `[name: <n> chars]` and collapse media URLs to `<scheme>://<host>/…`. As a safety net, every line is scrubbed once more before it is written: JIDs and runs of 10 or more digits left in error values or in whatsmeow's own lines become `…<last-4>`. whatsmeow's internal logger only passes warnings and errors, because at INFO it names your own number and, during session migrations, your contacts' numbers. Transcripts are never logged.

To see content while debugging, run `./bin/whatsapp-mcp -debug serve` or set `WHATSAPP_MCP_DEBUG=1`. That shows full message IDs, bodies and names, turns off the scrubbing and lets whatsmeow log at INFO. Even then, phone-number-shaped digits in JIDs, bodies and names show only their last 5 (`+15551234567` → `****34567`). That is obfuscation, not anonymisation: someone who knows your contacts can still correlate them.

For Claude Desktop integration issues, see the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues).

## Contributing

Contributions welcome via pull request. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Licence

MIT Licence — see [LICENSE](LICENSE).
