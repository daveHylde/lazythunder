# lazythunder add-on protocol

Thunderbird runs the add-on in `addon/`. At startup it calls
`browser.runtime.connectNative("lazythunder")`, which makes Thunderbird spawn
`lazythunder hub` with stdio as a native-messaging pipe (4-byte little-endian
length + UTF-8 JSON, both ways). On disconnect the add-on reconnects every 5 s.

The hub serves `$XDG_RUNTIME_DIR/lazythunder/hub.sock` (fallback
`/tmp/lazythunder-$UID/hub.sock`) to lazythunder clients (TUI, daemon). On the
socket: newline-delimited JSON, same message shapes. The hub rewrites request
ids so many clients can share the one pipe, and copies every event to every
client.

## Messages

Request (client → add-on):  `{"id": 7, "op": "search", "params": {...}}`
Response (add-on → client): `{"id": 7, "ok": true, "data": ...}`
                            `{"id": 7, "ok": false, "error": "text", "code": "NOT_FOUND"}`
Event (add-on → clients):   `{"event": "newMail", "data": {...}}` (no id)

The hub answers `{"ok":false,"code":"EXTENSION_DISCONNECTED"}` when no add-on is
connected. Host → add-on messages are capped by
`webextensions.native-messaging.max-input-message-bytes` (install raises it to 64 MiB).

## Shapes

`Message`, `Folder`, `Account`, `Attachment`, `FullMessage`, `Page` are the
JSON shapes of the same-named types in `internal/tb/tb.go`. Dates are ISO 8601
strings. Folder ids are `accountId + ":/" + path` (e.g. `account1://INBOX`).
Message ids are Thunderbird MailExtension message ids (ints).

## Ops

| op | params | data |
|---|---|---|
| `health` | – | `{"version": "..."}` |
| `accounts` | – | `[Account]` with identities |
| `folders` | – | `[Folder]`, every folder of every account, with unread/total counts and depth |
| `search` | `from`, `subject`, `folder` (id), `since`, `until` (RFC 3339), `unread`, `includeJunk`, `limit` | `Page`. Header filters only (no full text). Junk excluded unless `includeJunk`. `hasMore` when more than `limit` matched |
| `byMessageId` | `lookups: [{id: headerMessageId, folders?: [folder id]}]` | `[Message]`, every copy found; `folders` limits the search to those folders |
| `read` | `id` | `FullMessage` (`parts.text`, `parts.html`, `parts.attachments`) |
| `headers` | `id` | `{"raw": "<header block, CRLF or LF>"}` |
| `thread` | `id` | `{"thread": [Message]}` oldest first |
| `attachments` | `id` | `[Attachment]` incl. inline parts with `contentId` |
| `download` | `id`, `part` | `{"data": "<base64>"}` |
| `mark` | `ids`, `flag`: read/unread/flagged/unflagged/junk/not-junk | – |
| `markFolderRead` | `folder` | `{"marked": n}`, all unread in the folder |
| `move` | `ids`, `folder` | – |
| `archive` | `ids` | – |
| `delete` | `ids` (to trash) | – |
| `submit` | `kind`: new/reply/replyAll/forward, `replyTo` (id), `from` (identity email), `to`, `cc`, `subject`, `body` (plain text), `send` (bool; false saves a draft) | – |

## Events

| event | data |
|---|---|
| `newMail` | `{"folder": "<folder id>", "messages": [Message]}` |
| `changed` | `{"folder": "<folder id or empty>"}`: flags, moves, deletes, folder counts |
