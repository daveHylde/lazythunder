# lazythunder

A lazygit-style terminal UI for Thunderbird, built on
[thunderbird-cli](https://github.com/vitalio-sh/thunderbird-cli) (`tb`), with a
local [llama.cpp](https://github.com/ggml-org/llama.cpp) mail assistant.

```
[1] Folders │ [2] Messages │ [3] Preview / borealis2 / draft
```

## Requirements

- Thunderbird 128+ with the thunderbird-cli add-on installed
- `tb` and `tb-bridge` on `PATH` (`npm install -g thunderbird-cli thunderbird-cli-bridge`)
- a llama.cpp server with tool calling (the default model alias is `borealis2`)
- Go 1.26 to build: `go build -o lazythunder .`

On start, lazythunder checks `tb health`. If needed, it starts `tb-bridge` and
`thunderbird --headless`, then waits up to 60s for the add-on to connect. Logs
go to `~/.local/state/lazythunder/`.

## Configuration (environment)

| Variable | Default | |
|---|---|---|
| `LAZYTHUNDER_BRIDGE_PORT` | `7710` | tb-bridge HTTP port. Not 7700, which Meilisearch also uses. The add-on always connects to WebSocket port 7701. |
| `LAZYTHUNDER_LLM_URL` | `http://127.0.0.1:8085/v1` | OpenAI-compatible llama.cpp endpoint |
| `LAZYTHUNDER_MODEL` | `borealis2` | model alias |
| `VISUAL` / `EDITOR` | `vi` | editor for drafts |

## Keys

Press `?` in the app for the full list.

- **Navigation:** `tab`/`1 2 3`/`h l` switch panels, `j k g G` move, `enter` opens a folder or reads a message (marks it read), `/` searches all accounts, `u` toggles unread-only.
- **Mail:** `m` toggles read, `F` toggles flag, `e` archives, `d` deletes (to trash), `M` moves, `c` composes, `r`/`R`/`f` reply / reply all / forward.
- **Agent:** `a` asks borealis2, `s` summarizes the selected message or thread, `w` drafts a reply with AI, `ctrl+l` starts a new conversation, `esc` cancels a request.
- **Drafts:** `e` edits in `$EDITOR`, `enter` saves to Drafts, `S` sends (asks y/n), `x` discards.

The agent can search, read and list mail on its own. It asks y/n before
mark, move, archive or delete. It never sends: drafts it writes open in the
draft screen for you to review.

## Notes

- Sending needs **Allow agents to send email directly** ticked in the add-on's
  options. Without it, `S` fails with `SEND_DISABLED` and the draft stays open.
- For replies, Thunderbird picks the identity and recipients and appends the quoted original.
- Folder lists page by date window (`tb search --since/--until`), because a
  sorted `tb list` of a large folder takes about 20s in the add-on. Search
  results show one page of 100 hits.
