# lazythunder ⚡

**Your whole inbox, at the speed of your keyboard.**

lazythunder is a [lazygit](https://github.com/jesseduffield/lazygit)-style terminal UI for
Thunderbird. Every account in one window, HTML mail rendered properly in the terminal, images
drawn sharp, and an optional AI assistant that summarizes, writes replies, finds things for you,
and unsubscribes you from newsletters without opening a browser.

Thunderbird keeps doing the hard parts (IMAP, SMTP, OAuth, filters, offline sync). lazythunder
gives you a fast, keyboard-only way to get through it all.

```
 1 All inboxes (577) │ 2 me@gmail.com (457) │ 3 work@company.no (12) │ 4 side@project.io
╭[1] All inboxes · l folders───────────────────────────────────────────────────────────────────╮
│●  09:14  Fiken           me@gmail.com   Faktura 2026-118 forfaller fredag                    │
│ ↩ 08:52  Kari Nordmann   work@company   Re: Workshop next week                               │
│●  Nov 24 GitHub          work@company   [GitHub] A new SSH key was added                     │
│★  Nov 24 Posten          me@gmail.com   Pakken din er klar til henting                       │
╰──────────────────────────────────────────────────────────────────────────────────────────────╯
╭[2] Faktura 2026-118 forfaller fredag─────────────────────────────────────────────────────────╮
│From: Fiken <noreply@fiken.no>                       │ Action items · o+n opens ──────────────│
│Date: Wed 2026-11-25 09:14                           │ ☐ Pay the invoice by Friday [1]        │
│──────────────────────────────────────────────────── │   fiken.no/betal/2026-118              │
│✦ Invoice of 4 200 kr is due on Friday.              │ [2] Open the invoice on Fiken          │
│──────────────────────────────────────────────────── │   fiken.no/faktura/2026-118            │
│Hei!                                                 │                                        │
│                                                     │ Attachments (1) · A ───────────────────│
│Fakturaen under forfaller fredag.                    │ 📎 faktura-118.pdf 84 KB               │
│                                                     │ FAKTURA 2026-118  Beløp: 4 200,00      │
│Betal nå[1] · Se faktura[2]                          │                                        │
│                                                     │                                        │
│                                                     │                                        │
╰──────────────────────────────────────────────────────────────────────────────────────────────╯
 12 messages   l folders · r reply · space select · a archive · d delete · : agent · ? help
```

## Why you'll like it

- **One inbox for everything.** `alt+1` merges every account's inbox, newest first. `alt+2…9`
  jump straight to an account, and each tab shows its unread count.
- **Fast search.** `/` searches sender, recipients, subject, attachment names and message text
  across every account and all time, ignoring case. The first hits show within a second. Newest first.
- **Real HTML in your terminal.** Bold, italics, headings, lists and quotes. Layout tables keep their
  columns side by side, small icons sit inline with the text, hidden preview text is dropped, and remote and embedded images show up inline: sharp
  with sixel or kitty graphics, and as half-blocks in any other terminal. `i` opens them full
  screen.
- **A summary and a sidebar that tell you what matters.** A one-line AI summary under the
  headers, and a sidebar **Action items** list: action
  items tied to the link that does them, followed by every other safe link with a short title of
  what it opens. Type `o` and the number to open one. Codes you need to type somewhere (login
  codes, booking references, vouchers, tracking numbers) are action items too: `y` copies one.
- **The whole conversation at a glance.** Above the action items, a **Thread** box lists the
  conversation's inbox messages, newest first, the open one highlighted; your own replies show at
  the top of the message they answer. Type `t` and the number to jump to one.
- **Triage at a glance.** Every summarized message gets a short dim tag before its subject:
  `reply` (needs your answer), `act` (something to do), `news`, `rcpt` (receipts), `note`
  (notifications) or `spam`, and `!` when it is urgent. Plain FYI mail gets no tag.
- **Unsubscribe with one key, no browser.** `U` uses one-click unsubscribe when the sender supports
  it, sends the unsubscribe email when that's the method, or lets the assistant read the web page
  and fill in the form for you. Then the message goes to the trash.
- **An assistant that works your mail.** `:` and ask: "what did Kari say about the workshop?",
  "find the train tickets from October", "archive all GitHub notifications from last week". It
  searches, reads and acts across every account. It asks y/n before changing anything and never
  sends on its own.
- **Replies in your own style.** `W`, type "yes, Friday works", and get a full reply in the email's
  language, written to your own reply guide. You review it, edit it in `$EDITOR`, and send.
- **Safe by default.**
  - Risky attachments (`invoice.pdf.exe`, macro Office files, scripts, archives) are flagged ⚠
    and never previewed. A file whose content is an executable while its name says otherwise is
    never opened.
  - PDFs are previewed with `pdftotext` inside a `bwrap` sandbox: no network, no home directory,
    only that one file visible.
  - Only safe links get a number: phishing tricks are hidden, such as link text that names a
    different site, IP addresses, lookalike domains, URL shorteners and executable downloads.
  - Web requests never reach private or local addresses.
- **Know what you've answered.** ↩ marks messages you replied to and → the ones you forwarded.
  Open one and your reply is shown above it.
- **Bring your own AI, or none.** Any OpenAI-compatible endpoint works: a local llama.cpp server,
  Ollama, vLLM, OpenAI or OpenRouter. Without AI, everything else still works.

## Quick start

lazythunder talks to Thunderbird through its own small add-on, which Thunderbird connects to
lazythunder over native messaging. Thunderbird 128 or newer is required.

1. **Install lazythunder** (Go 1.26+):
   ```sh
   go install github.com/daveHylde/lazythunder@latest
   ```
2. **Install the add-on.** Close Thunderbird, then run:
   ```sh
   lazythunder install
   ```
   It installs the add-on into your default profile, registers lazythunder as its
   native-messaging host, removes thunderbird-cli if it was installed, and starts Thunderbird
   headless.
3. **Run it:**
   ```sh
   lazythunder
   ```
   On start, lazythunder starts `thunderbird --headless` if it isn't running, and waits for the
   add-on to connect. It still works if Thunderbird is already open.

### Optional extras

| For | Install |
|---|---|
| PDF previews | `pdftotext` (poppler) and `bwrap` (bubblewrap) |
| Copying links with `y` | `wl-copy` (wl-clipboard, Wayland) |
| Sharp images | a sixel or kitty-graphics terminal: foot, wezterm, kitty, ghostty |
| AI features | any OpenAI-compatible endpoint, see below |

lazythunder runs on Linux. Links and files open through `xdg-open`.

## AI setup

AI is optional. It powers summaries, action items, link titles, the assistant (`:`, `z`), AI
replies (`W`), suspicious-link detection, and unsubscribing through web pages. The endpoint must
support OpenAI-style chat completions with tool calling and JSON-schema output.

```sh
# Local llama.cpp (the default URL). --jinja enables tool calling.
llama-server -m your-model.gguf --port 8085 --jinja
lazythunder

# Ollama
LAZYTHUNDER_LLM_URL=http://localhost:11434/v1 LAZYTHUNDER_MODEL=qwen3 lazythunder

# OpenAI
LAZYTHUNDER_LLM_URL=https://api.openai.com/v1 LAZYTHUNDER_MODEL=gpt-4o-mini \
LAZYTHUNDER_API_KEY=sk-… lazythunder

# OpenRouter
LAZYTHUNDER_LLM_URL=https://openrouter.ai/api/v1 LAZYTHUNDER_MODEL=openai/gpt-4o-mini \
LAZYTHUNDER_API_KEY=sk-or-… lazythunder

# No AI at all
LAZYTHUNDER_LLM_URL=off lazythunder
```

A hosted provider receives the text of the emails it summarizes or reads. Use a local model if
your mail should stay on your machine.

## Keys

Press `?` in the app for the full list.

| | |
|---|---|
| **Tabs** | `alt+1` all inboxes · `alt+2…9` accounts · `[` `]` previous/next |
| **Move** | `j` `k` `g` `G` · `tab` or `1` `2` switch pane · `enter` read · `n` `p` next/previous unread · `l` folders · `v` sidebar |
| **Scroll** | `pgup` `pgdown` always scroll the message · `space` pages · `ctrl+d` `ctrl+u` |
| **Find** | `/` search · `u` unread only · `esc` back from search · `F5` refresh · `T` open the Thunderbird window |
| **Mail** | `r` reply · `R` reply all · `f` forward · `c` compose · `a` archive · `d`/`Del` trash · `M` move · `m` read/unread · `S` star · `C` mark folder read |
| **Select** | `space` select in the list · `m` `S` `a` `d` `M` act on all selected · `esc` clears |
| **Links** | `o` link list · `o3` opens link [3] · `y` copy |
| **Codes** | `y` copies the message's code (login code, booking reference, voucher, tracking number) · `y2` the 2nd when several |
| **Thread** | `t` thread list · `t2` opens message 2 of the thread (in the list, or the whole thread as a list; `esc` back) |
| **Attachments** | `A` list · `enter` preview · `d` save to ~/Downloads · `o` open · `i` images full screen |
| **Clean up** | `U` unsubscribe and trash |
| **AI** | `:` ask · `z` summarize message or thread · `W` write a reply · `X` new chat · `esc` cancel / back to the email |
| **Drafts** | `e` edit in `$EDITOR` · `enter` save to Drafts · `S` send · `x` discard |
| **Accounts** | `ctrl+a` accounts screen · `a` add · `e` edit servers or identity · `D` remove (local copy only, server untouched) · `i` add identity · `x` delete identity · `*` default identity |

Resting on a message for a second marks it read and moves its summary to the front of the queue.

## Configuration

Everything is set with environment variables.

| Variable | Default | |
|---|---|---|
| `LAZYTHUNDER_LLM_URL` | `http://127.0.0.1:8085/v1` | OpenAI-compatible base URL. `off` disables AI. |
| `LAZYTHUNDER_MODEL` | (the server's default) | model name. Hosted providers need it. |
| `LAZYTHUNDER_API_KEY` | (none) | sent as `Authorization: Bearer …` |
| `LAZYTHUNDER_GRAPHICS` | auto | `kitty` or `sixel` to force an image protocol |
| `VISUAL` / `EDITOR` | `vi` | editor for drafts |

| File | |
|---|---|
| `~/.config/lazythunder/reply-guide.md` | how AI replies are written: language, tone, length, greeting and sign-off. Created on first use; edit it to taste. |
| `~/.local/share/lazythunder/lazythunder.db` | summaries, category tags, reply status and unsubscribe methods (SQLite, shared with the daemon) |
| `~/.config/lazythunder/env` | environment for the background daemon (see below) |
| `~/.cache/lazythunder/` | image cache |
| `~/.local/state/lazythunder/` | hub log, and Thunderbird's when lazythunder started it |

## How it works

- **Talking to Thunderbird.** lazythunder's add-on (`addon/`) asks Thunderbird to start
  `lazythunder hub` over native messaging; the hub shares that pipe with the UI and the daemon
  over a unix socket (see `addon/PROTOCOL.md`). A call takes a few milliseconds, and the add-on
  pushes new mail and changes, so nothing waits for the next poll.
- **Search** reads Thunderbird's own search index (gloda, `global-messages-db.sqlite`)
  read-only, which matches everything in about 0.2 s, then looks the hits up in Thunderbird 20 at
  a time. Mail that arrived in the last minute or so may not be indexed yet.
- **Folder lists** page by date windows instead of sorting whole folders, so a 26,000-message
  folder opens as fast as a small one.
- **Summaries** run in a background queue, one at a time and paused while you chat with the
  assistant, and are stored by Message-ID. New mail is summarized as it arrives, by the
  background daemon if it runs, else by lazythunder itself while it is open.
- **Unsubscribe** tries RFC 8058 one-click (an HTTP POST) first, then the List-Unsubscribe
  `mailto:` address, sent from the account the mail came to with only its subject and body used.
  Last, the assistant reads the unsubscribe page over plain HTTP and works through it, up to 5
  pages. Pages that need JavaScript, a CAPTCHA or a login are reported, not opened.
- **Replies** are detected by matching later messages in your Sent folders by subject and
  recipient, so very generic subjects can misfire.
- Every call to Thunderbird has a hard timeout, so a stuck Thunderbird can never freeze the UI.

## Background daemon

`lazythunder daemon` does the background work without the UI: as new mail arrives (and every
minute as a fallback) it looks for new mail in every inbox, writes its summary, category and priority, finds its unsubscribe options,
and checks your Sent folders to record what you replied to or forwarded. When you open
lazythunder, it's all there already. While a daemon runs, the UI leaves this work to it; without
one, the UI does it while open.

Install it as a systemd user service:

```sh
go build -o ~/.local/bin/lazythunder .
mkdir -p ~/.config/systemd/user ~/.config/lazythunder
cp contrib/systemd/lazythunder.service ~/.config/systemd/user/
# the AI settings the daemon should use (same variables as above)
cat > ~/.config/lazythunder/env <<'ENV'
LAZYTHUNDER_LLM_URL=http://127.0.0.1:8085/v1
LAZYTHUNDER_MODEL=
LAZYTHUNDER_API_KEY=
ENV
chmod 600 ~/.config/lazythunder/env
systemctl --user daemon-reload
systemctl --user enable --now lazythunder
journalctl --user -u lazythunder -f   # its log
```

The daemon starts `thunderbird --headless` if needed, and keeps retrying while
Thunderbird is unreachable; if Thunderbird goes away later, it starts it again. While it can't
reach Thunderbird, an open UI does the work instead. The unit sets `KillMode=process`: the
Thunderbird it started lives in the service's cgroup, and stopping the service must not kill
them.

A headless Thunderbird holds your profile, so the normal Thunderbird launcher opens nothing
while it runs. Press `T` in lazythunder, or run `lazythunder open-thunderbird`, to swap it for
the windowed Thunderbird; lazythunder keeps working with the window. Failed summaries are retried up to 3 times, 15 minutes apart. It only processes mail that arrives after it first looks at a
folder; older mail is summarized when you rest on it. Everything is stored in
`~/.local/share/lazythunder/lazythunder.db`. The old summary cache
(`~/.cache/lazythunder/summaries-v2.json`) is imported on first start and then removed; a
corrupt one is renamed to `summaries-v2.json.bad`.

## Privacy note

Remote images load automatically, which tells senders when you open their mail. Tracking pixels
(2 px or smaller) are skipped.

## License

[MIT](LICENSE)
