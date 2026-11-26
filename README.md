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
│From: Fiken <noreply@fiken.no>                       │ ✦ Summary ─────────────────────────────│
│Date: Wed 2026-11-25 09:14                           │ Invoice of 4 200 kr is due on Friday.  │
│📎 faktura-118.pdf (84 KB)                           │                                        │
│──────────────────────────────────────────────────── │ To do · o+n opens ─────────────────────│
│Hei!                                                 │ ☐ Pay the invoice by Friday [1]        │
│                                                     │   fiken.no/betal/2026-118              │
│Fakturaen under forfaller fredag.                    │ [2] View invoice online                │
│                                                     │   fiken.no/faktura/2026-118            │
│Betal nå[1] · Se faktura[2]                          │                                        │
│                                                     │ Attachments (1) · A ───────────────────│
│                                                     │ 📎 faktura-118.pdf 84 KB               │
│                                                     │ FAKTURA 2026-118  Beløp: 4 200,00      │
╰──────────────────────────────────────────────────────────────────────────────────────────────╯
 12 messages        l folders · r reply · o links · A attachments · e archive · a agent · ? help
```

## Why you'll like it

- **One inbox for everything.** `alt+1` merges every account's inbox, newest first. `alt+2…9`
  jump straight to an account, and each tab shows its unread count.
- **Fast search.** `/` searches sender and subject across every account and all time in a few
  seconds, ignoring case, then merges in matches from the message text. Newest first.
- **Real HTML in your terminal.** Bold, italics, headings, lists and quotes. Layout tables are
  flattened, hidden preview text is dropped, and remote and embedded images show up inline: sharp
  with sixel or kitty graphics, and as half-blocks in any other terminal. `i` opens them full
  screen.
- **A sidebar that tells you what matters.** A one-line AI summary, then a **To do** list: action
  items tied to the link that does them, followed by every other safe link with a short title of
  what it opens. Type `o` and the number to open one.
- **Unsubscribe with one key, no browser.** `U` uses one-click unsubscribe when the sender supports
  it, sends the unsubscribe email when that's the method, or lets the assistant read the web page
  and fill in the form for you. Then the message goes to the trash.
- **An assistant that works your mail.** `a` and ask: "what did Kari say about the workshop?",
  "find the train tickets from October", "archive all GitHub notifications from last week". It
  searches, reads and acts across every account. It asks y/n before changing anything and never
  sends on its own.
- **Replies in your own style.** `w`, type "yes, Friday works", and get a full reply in the email's
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

lazythunder talks to Thunderbird through
[thunderbird-cli](https://github.com/vitalio-sh/thunderbird-cli): a small Thunderbird add-on, a
local bridge, and the `tb` command.

1. **Install the Thunderbird add-on.** Download the latest `.xpi` from
   [thunderbird-cli releases](https://github.com/vitalio-sh/thunderbird-cli/releases/latest). In
   Thunderbird, open **Add-ons and Themes**, then ⚙ → **Install Add-on From File…**. Thunderbird
   128 or newer is required.
2. **Install the bridge and CLI:**
   ```sh
   npm install -g thunderbird-cli thunderbird-cli-bridge
   ```
3. **Install lazythunder** (Go 1.26+):
   ```sh
   go install github.com/daveHylde/lazythunder@latest
   ```
4. **Run it:**
   ```sh
   lazythunder
   ```
   On start, lazythunder starts the bridge and `thunderbird --headless` if they aren't running,
   and waits for the add-on to connect. It still works if Thunderbird is already open.
5. **Optional: let `S` send mail.** In the add-on's options in Thunderbird, tick **Allow agents
   to send email directly**. Without it, drafts are saved to Drafts and sending fails with
   `SEND_DISABLED`.

### Optional extras

| For | Install |
|---|---|
| PDF previews | `pdftotext` (poppler) and `bwrap` (bubblewrap) |
| Copying links with `y` | `wl-copy` (wl-clipboard, Wayland) |
| Sharp images | a sixel or kitty-graphics terminal: foot, wezterm, kitty, ghostty |
| AI features | any OpenAI-compatible endpoint, see below |

lazythunder runs on Linux. Links and files open through `xdg-open`.

## AI setup

AI is optional. It powers summaries, To do items, link titles, the assistant (`a`, `s`), AI
replies (`w`), suspicious-link detection, and unsubscribing through web pages. The endpoint must
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
| **Move** | `j` `k` `g` `G` · `tab` or `1` `2` switch pane · `enter` read · `l` folders · `b` sidebar |
| **Scroll** | `pgup` `pgdown` always scroll the message · `space` pages · `ctrl+d` `ctrl+u` |
| **Find** | `/` search · `u` unread only · `esc` back from search · `ctrl+r` refresh |
| **Mail** | `r` reply · `R` reply all · `f` forward · `c` compose · `e` archive · `d` trash · `M` move · `m` read/unread · `F` flag |
| **Links** | `o` link list · `o3` opens link [3] · `y` copy |
| **Attachments** | `A` list · `enter` preview · `d` save to ~/Downloads · `o` open · `i` images full screen |
| **Clean up** | `U` unsubscribe and trash |
| **AI** | `a` ask · `s` summarize message or thread · `w` write a reply · `ctrl+l` new chat · `esc` cancel |
| **Drafts** | `e` edit in `$EDITOR` · `enter` save to Drafts · `S` send · `x` discard |

Resting on a message for a second marks it read and moves its summary to the front of the queue.

## Configuration

Everything is set with environment variables.

| Variable | Default | |
|---|---|---|
| `LAZYTHUNDER_LLM_URL` | `http://127.0.0.1:8085/v1` | OpenAI-compatible base URL. `off` disables AI. |
| `LAZYTHUNDER_MODEL` | (the server's default) | model name. Hosted providers need it. |
| `LAZYTHUNDER_API_KEY` | (none) | sent as `Authorization: Bearer …` |
| `LAZYTHUNDER_BRIDGE_PORT` | `7710` | tb-bridge HTTP port. The add-on always connects on WebSocket port 7701. |
| `LAZYTHUNDER_GRAPHICS` | auto | `kitty` or `sixel` to force an image protocol |
| `VISUAL` / `EDITOR` | `vi` | editor for drafts |

| File | |
|---|---|
| `~/.config/lazythunder/reply-guide.md` | how AI replies are written: language, tone, length, greeting and sign-off. Created on first use; edit it to taste. |
| `~/.cache/lazythunder/` | summaries and image cache |
| `~/.local/state/lazythunder/` | bridge and Thunderbird logs when lazythunder started them |

## How it works

- **Search** runs sender and subject matches as native Thunderbird queries, which take seconds
  even over all time. The full-text index is slower, so text matches cover the last 90 days and
  are merged in when they arrive.
- **Folder lists** page by date windows instead of sorting whole folders, so a 26,000-message
  folder opens as fast as a small one.
- **Summaries** run in a background queue, one at a time and paused while you chat with the
  assistant, and are cached by Message-ID. New mail is summarized as it arrives.
- **Unsubscribe** tries RFC 8058 one-click (an HTTP POST) first, then the List-Unsubscribe
  `mailto:` address, sent from the account the mail came to with only its subject and body used.
  Last, the assistant reads the unsubscribe page over plain HTTP and works through it, up to 5
  pages. Pages that need JavaScript, a CAPTCHA or a login are reported, not opened.
- **Replies** are detected by matching later messages in your Sent folders by subject and
  recipient, so very generic subjects can misfire.
- Every call to Thunderbird has a hard timeout, so a stuck Thunderbird can never freeze the UI.

## Privacy note

Remote images load automatically, which tells senders when you open their mail. Tracking pixels
(2 px or smaller) are skipped.

## License

[MIT](LICENSE)
