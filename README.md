# lazythunder

A lazygit-style terminal UI for Thunderbird, built on
[thunderbird-cli](https://github.com/vitalio-sh/thunderbird-cli) (`tb`), with a
local [llama.cpp](https://github.com/ggml-org/llama.cpp) mail assistant.

```
 1 All inboxes (577) │ 2 me@gmail.com (457) │ 3 work@company.no …   ← alt+1..9
[1] Messages                                   (● unread ★ flagged ↩ replied → forwarded)
[2] Email (75 cols): your reply above it, then rendered HTML with images │ sidebar: ✦ AI summary,
    (or borealis2 / draft / link, folder, attachment pickers)             │ action items, links,
                                                                          │ attachments + safe preview
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

- **Tabs:** `alt+1` opens All inboxes, a merged newest-first list of every inbox. `alt+2..9` open accounts; `[`/`]` cycle. Tabs show inbox unread counts.
- **Navigation:** `tab`/`1 2` switch between list and message, `j k g G` move, `l` opens the folder picker, `enter` reads a message. Resting on a message for 1s marks it read. `/` searches all accounts, `u` toggles unread-only.
- **Mail:** `o` opens the link picker (`enter` opens in the browser, `y` copies). `A` opens attachments: `enter` previews (images in the viewer, PDF via pdftotext, text/CSV/JSON/HTML in the pane), `d` saves to ~/Downloads, `o` opens in the default app. `i` views images full screen, `m` toggles read, `F` toggles flag, `e` archives, `d` deletes (to trash), `M` moves, `c` composes, `r`/`R`/`f` reply / reply all / forward.
- **Agent:** `a` asks borealis2, `s` summarizes the selected message or thread, `w` drafts a reply with AI, `ctrl+l` starts a new conversation, `esc` cancels a request.
- **Drafts:** `e` edits in `$EDITOR`, `enter` saves to Drafts, `S` sends (asks y/n), `x` discards.

The agent can search, read and list mail on its own. It asks y/n before
mark, move, archive or delete. It never sends: drafts it writes open in the
draft screen for you to review.

## Notes

- Sidebar (`b` toggles; it needs 30 free columns beside the text):
  - **Summary:** borealis2 writes a one-line summary and up to 3 action items for each new inbox message, in a background queue (one at a time, paused while you chat with the agent). Summaries are cached in `~/.cache/lazythunder/summaries.json`. Resting 1s on any message summarizes it next.
  - **Attachments:** the first image, PDF or text attachment is previewed automatically and safely. Images are decoded in Go. PDFs are read by `pdftotext` inside `bwrap`, with no network, read-only `/usr`, no home directory and only that one file visible. Nothing is ever run or opened by itself.
  - **Risky attachments:** executables, scripts, shortcuts, archives, macro Office files and HTML/SVG are flagged ⚠ (by any extension, so `invoice.pdf.exe` counts) and never previewed. Opening one asks for confirmation. A file whose content is an executable or script while its name says otherwise is never opened.
- Replies written by borealis2 (`w` asks what to say in a few words) follow `~/.config/lazythunder/reply-guide.md`, created on first use: Norwegian or English to match the email, polite, to the point, no fluff, with a greeting/sign-off template. Edit it to taste.

- Replies: a message shows ↩ if you sent a later message with the same subject (Re: or none) to its sender, and → if you forwarded it. The match uses Sent folders and subjects, so generic subjects can misfire. When you open a replied message, your reply is shown above it, dimmed, with the quoted history removed.
- Images are shown complete at natural size, fit to the pane. In sixel terminals that report their cell size (foot, wezterm), fully visible images are drawn sharp in the preview; partly scrolled ones fall back to half-blocks, as do kitty/ghostty/alacritty. Embedded (`cid:`) images and remote `http(s)` images are both loaded automatically, which tells senders when you open their mail. Tracking pixels (≤2px) are skipped. Press `i` for the full-resolution viewer: kitty graphics in kitty/ghostty, sixel elsewhere (foot, wezterm). Set `LAZYTHUNDER_GRAPHICS=kitty|sixel` to override.

- HTML mail is rendered in the terminal: bold, italic, headings, lists, quotes. Layout tables are flattened, hidden preheaders are dropped, and links are numbered `[n]` with their URLs listed at the end.

- Sending needs **Allow agents to send email directly** ticked in the add-on's
  options. Without it, `S` fails with `SEND_DISABLED` and the draft stays open.
- For replies, Thunderbird picks the identity and recipients and appends the quoted original.
- Folder lists page by date window (`tb search --since/--until`), because a
  sorted `tb list` of a large folder takes about 20s in the add-on. Search
  results show one page of 100 hits.
