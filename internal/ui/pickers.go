package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/davidhyldgaard/lazythunder/internal/render"
	"github.com/davidhyldgaard/lazythunder/internal/tb"
)

type link struct{ url, label string }

type pickItem struct {
	n             int // link number, as [n] in the text
	title, detail string
	link          link
	att           tb.Attachment
	folder        tb.Folder
}

type picker struct {
	kind  string // "links", "attachments", "folders" or "move"
	msgID int
	items []pickItem
	cur   int
	num   string // digits typed so far (links: o3, o12)
}

// linkNumTick opens the typed link number once no second digit follows.
type linkNumTick struct {
	p   *picker
	num string
}

type (
	attPreviewMsg struct {
		picker             *picker // only applies while this picker is still open
		title, text, image string
		html               bool
		err                error
	}
	// noteMsg reports the outcome of a background action in the status line.
	noteMsg struct {
		text string
		err  error
	}
)

const maxPreviewBytes = 2 << 20

func (m Model) openLinkPicker() (tea.Model, tea.Cmd) {
	if m.full == nil || len(m.links) == 0 {
		m.status = "no links in this message"
		return m, nil
	}
	p := &picker{kind: "links", msgID: m.full.ID}
	sum, _, _ := m.sum.get(m.full.HeaderMessageID)
	info := linkInfos(sum)
	for i, l := range m.links {
		if n := i + 1; !info[n].Suspicious {
			p.items = append(p.items, pickItem{n: n, title: m.linkTitle(n, info), detail: shortURL(l.url), link: l})
		}
	}
	if len(p.items) == 0 {
		m.status = "no safe links in this message"
		return m, nil
	}
	m.picker, m.mode = p, mPicker
	return m, nil
}

// openFolderPicker lists the tab's folders (every inbox on the All tab).
func (m Model) openFolderPicker() (tea.Model, tea.Cmd) {
	return m.folderPicker("folders", m.tabAccount())
}

// openMovePicker lists the folders of the message's own account.
func (m Model) openMovePicker(acct string) (tea.Model, tea.Cmd) {
	return m.folderPicker("move", acct)
}

func (m Model) folderPicker(kind, acct string) (tea.Model, tea.Cmd) {
	p := &picker{kind: kind}
	for _, f := range m.foldersOf(acct) {
		title := strings.Repeat("  ", max(0, f.Depth-1)) + render.Sanitize(f.Name)
		if acct == "" {
			title = render.Sanitize(m.accounts[f.AccountID].Name)
		}
		detail := ""
		if f.Unread > 0 {
			detail = fmt.Sprintf("%d unread", f.Unread)
		}
		if len(m.view.folderIDs) == 1 && m.view.folderIDs[0] == f.ID {
			p.cur = len(p.items)
		}
		p.items = append(p.items, pickItem{title: title, detail: detail, folder: f})
	}
	if len(p.items) == 0 {
		m.status = "no folders"
		return m, nil
	}
	m.picker, m.mode = p, mPicker
	return m, nil
}

func (m Model) openAttachmentPicker() (tea.Model, tea.Cmd) {
	if m.full == nil || len(m.full.Parts.Attachments) == 0 {
		m.status = "no attachments in this message"
		return m, nil
	}
	p := &picker{kind: "attachments", msgID: m.full.ID}
	for _, a := range m.full.Parts.Attachments {
		p.items = append(p.items, pickItem{title: oneLine(a.Name), detail: fmt.Sprintf("%s · %s", oneLine(a.ContentType), humanSize(a.Size)), att: a})
	}
	m.picker, m.mode = p, mPicker
	return m, nil
}

func humanSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", (n+1023)/1024)
	}
	return fmt.Sprintf("%d B", n)
}

func (m Model) keyPicker(s string) (tea.Model, tea.Cmd) {
	p := m.picker
	switch s {
	case "esc", "q":
		m.picker, m.mode = nil, mNormal
		return m, nil
	case "j", "down", "k", "up", "g", "G", "home", "end", "ctrl+d", "ctrl+u", "pgdown", "pgup":
		p.cur = move(p.cur, len(p.items), s, m.preview.Height/2)
		return m, nil
	}
	it := p.items[p.cur]
	switch p.kind {
	case "folders":
		if s == "enter" || s == "l" || s == "right" {
			m.picker, m.mode, m.focus = nil, mNormal, pMessages
			return m.openFolder(it.folder)
		}
		return m, nil
	case "move":
		if s == "enter" || s == "l" || s == "right" {
			ids, to := m.moveIDs, it.folder
			m.picker, m.mode, m.moveIDs = nil, mNormal, nil
			c, ctx := m.c, m.ctx
			return m, m.act("moved to "+to.Name, ids, func() error { return c.Move(ctx, ids, to.ID) })
		}
		return m, nil
	}
	if p.kind == "links" {
		if len(s) == 1 && s[0] >= '0' && s[0] <= '9' && (p.num != "" || s != "0") {
			p.num += s
			n, _ := strconv.Atoi(p.num)
			if n*10 > len(m.links) { // no longer number starts with these digits
				return m.openLinkNumber(p)
			}
			num := p.num
			return m, tea.Tick(600*time.Millisecond, func(time.Time) tea.Msg { return linkNumTick{p, num} })
		}
		switch s {
		case "enter", "o":
			return m.openLink(it.link)
		case "y":
			if err := exec.Command("wl-copy", it.link.url).Run(); err != nil {
				m.err = "copy: " + err.Error()
				return m, nil
			}
			m.status = "copied link"
		}
		return m, nil
	}
	c, ctx, store, id, a := m.c, m.ctx, m.images, p.msgID, it.att
	switch s {
	case "enter", "l", "right":
		if kind, why := classify(a.Name, a.ContentType); kind == attRisky {
			m.err = fmt.Sprintf("⚠ %s is %s: no preview (d downloads, o opens after a warning)", it.title, why)
			return m, nil
		}
		m.status = "loading " + it.title + "…"
		return m, func() tea.Msg {
			r := previewAttachment(ctx, c, store, id, a)
			r.picker = p
			return r
		}
	case "d":
		m.status = "downloading " + it.title + "…"
		return m, func() tea.Msg {
			dst, err := saveAttachment(ctx, c, store, id, a, downloadsDir())
			return noteMsg{"saved " + dst, err}
		}
	case "o":
		open := func() tea.Msg {
			// a persistent dir: the external app may read the file after we exit
			dst, err := openCopy(ctx, c, store, id, a)
			if err == nil {
				// a file that claims to be harmless but is an executable/script is disguised malware: never open
				if why := sniffRisky(dst); why != "" && classifyKind(a) != attRisky {
					return noteMsg{err: fmt.Errorf("⚠ %s is really %s despite its name: not opened", it.title, why)}
				}
				err = openExternal(dst)
			}
			return noteMsg{"opened " + filepath.Base(dst), err}
		}
		switch kind, why := classify(a.Name, a.ContentType); kind {
		case attRisky:
			m.confirm = &confirmation{text: fmt.Sprintf("⚠ %s is %s and could harm your computer. Open it anyway?", it.title, why), onYes: open}
			m.mode = mConfirm
			return m, nil
		case attOther: // unknown type: the default app might do anything with it
			m.confirm = &confirmation{text: fmt.Sprintf("%s is an unrecognised file type. Open it with the default app?", it.title), onYes: open}
			m.mode = mConfirm
			return m, nil
		}
		m.status = "opening " + it.title + "…"
		return m, open
	}
	return m, nil
}

// fetchAttachment downloads a part to the session temp dir (cached).
func fetchAttachment(ctx context.Context, c *tb.Client, store *imageStore, id int, a tb.Attachment) (string, error) {
	if store == nil {
		return "", fmt.Errorf("no temp directory")
	}
	path := filepath.Join(store.dir, fmt.Sprintf("att-%d-%s-%s", id, hash(a.PartName), safeName(a.Name)))
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	part := path + ".part" // a concurrent caller must never see a half-written file
	if err := c.Download(ctx, id, a.PartName, part); err != nil {
		os.Remove(part)
		return "", err
	}
	return path, os.Rename(part, path)
}

// safeName keeps an attachment's file name from escaping the target directory.
func safeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(render.Sanitize(name), "\\", "/"))
	if name == "." || name == "/" || name == ".." || name == "" {
		return "attachment"
	}
	if len(name) > 150 { // stay under NAME_MAX with our prefixes, keep the extension
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		name = strings.ToValidUTF8(name[:150-len(ext)], "") + ext
	}
	return name
}

// saveAttachment copies a part into dir under a free file name.
func saveAttachment(ctx context.Context, c *tb.Client, store *imageStore, id int, a tb.Attachment, dir string) (string, error) {
	src, err := fetchAttachment(ctx, c, store, id, a)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := safeName(a.Name)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 0; ; i++ {
		dst := filepath.Join(dir, name)
		if i > 0 {
			dst = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // never overwrite
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		in, err := os.Open(src)
		if err != nil {
			out.Close()
			return "", err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return dst, err
	}
}

// openCopy keeps one private copy per attachment for external apps.
func openCopy(ctx context.Context, c *tb.Client, store *imageStore, id int, a tb.Attachment) (string, error) {
	dir := filepath.Join(cacheDir(), "open", fmt.Sprintf("%d-%s", id, hash(a.PartName)))
	dst := filepath.Join(dir, safeName(a.Name))
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	src, err := fetchAttachment(ctx, c, store, id, a)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	return dst, os.WriteFile(dst, b, 0o600)
}

func previewAttachment(ctx context.Context, c *tb.Client, store *imageStore, id int, a tb.Attachment) attPreviewMsg {
	path, err := fetchAttachment(ctx, c, store, id, a)
	if err != nil {
		return attPreviewMsg{err: err}
	}
	if why := sniffRisky(path); why != "" {
		return attPreviewMsg{err: fmt.Errorf("⚠ %s is really %s: not previewed", oneLine(a.Name), why)}
	}
	title := oneLine(a.Name)
	ct := strings.ToLower(a.ContentType)
	ext := strings.ToLower(filepath.Ext(a.Name))
	switch {
	case strings.HasPrefix(ct, "image/"):
		return attPreviewMsg{title: title, image: path}
	case ct == "application/pdf" || ext == ".pdf":
		out, err := pdfText(ctx, path, 50)
		if err != nil {
			return attPreviewMsg{err: err}
		}
		return attPreviewMsg{title: title, text: out}
	case isTextual(ct, ext):
		f, err := os.Open(path)
		if err != nil {
			return attPreviewMsg{err: err}
		}
		defer f.Close()
		b, _ := io.ReadAll(io.LimitReader(f, maxPreviewBytes))
		if strings.Contains(ct, "html") || ext == ".html" || ext == ".htm" {
			return attPreviewMsg{title: title, text: string(b), html: true}
		}
		return attPreviewMsg{title: title, text: string(b)}
	}
	return attPreviewMsg{err: fmt.Errorf("no preview for %s: o opens it, d downloads it", oneLine(a.ContentType))}
}

var textExts = map[string]bool{".txt": true, ".md": true, ".csv": true, ".json": true, ".xml": true, ".log": true,
	".ics": true, ".vcf": true, ".html": true, ".htm": true, ".yaml": true, ".yml": true}

// isTextual: text/*, JSON, CSV and plain XML, but not zipped office formats
// (application/vnd.openxmlformats-…, …+zip) whose type merely mentions xml.
func isTextual(ct, ext string) bool {
	if strings.Contains(ct, "openxmlformats") || strings.Contains(ct, "zip") {
		return false
	}
	return strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "csv") ||
		ct == "application/xml" || strings.HasSuffix(ct, "+xml") || textExts[ext]
}

// openExternal hands a URL or file to the desktop's default app, detached.
// mailto: links lose their query (attach=, body=…): a hostile link must not
// pre-fill a draft, let alone attach local files.
func openExternal(target string) error {
	if strings.HasPrefix(strings.ToLower(target), "mailto:") {
		target, _, _ = strings.Cut(target, "?")
	}
	cmd := exec.Command("xdg-open", target)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap it
	return nil
}

func downloadsDir() string {
	if out, err := exec.Command("xdg-user-dir", "DOWNLOAD").Output(); err == nil {
		if d := strings.TrimSpace(string(out)); d != "" {
			return d
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads")
}

func cacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "lazythunder")
}

func (m Model) openLink(l link) (tea.Model, tea.Cmd) {
	m.picker, m.mode = nil, mNormal
	if err := openExternal(l.url); err != nil {
		m.err = err.Error()
		return m, nil
	}
	m.status = "opened " + ansi.Truncate(l.url, 60, "…")
	return m, nil
}

// openLinkNumber opens the link whose number was typed in the picker.
func (m Model) openLinkNumber(p *picker) (tea.Model, tea.Cmd) {
	n, _ := strconv.Atoi(p.num)
	p.num = ""
	for _, it := range p.items {
		if it.n == n {
			return m.openLink(it.link)
		}
	}
	m.status = fmt.Sprintf("no safe link [%d]", n)
	return m, nil
}

func (m Model) pickerLines(w int) []string {
	p := m.picker
	from, to := window(len(p.items), p.cur, m.bottomHeight()-4)
	var lines []string
	for i := from; i < to; i++ {
		it := p.items[i]
		n := i + 1
		if p.kind == "links" {
			n = it.n
		}
		line := fmt.Sprintf("%3d  %s  %s", n, render.Sanitize(it.title), dim.Render(render.Sanitize(it.detail)))
		if i == p.cur {
			line = highlight(line, w, true)
		}
		lines = append(lines, line)
	}
	return lines
}

func (m Model) pickerHelp() string {
	switch m.picker.kind {
	case "links":
		return "number or enter opens · y copy · esc close"
	case "folders":
		return "enter open · esc close"
	case "move":
		return "enter move here · esc cancel"
	}
	return "enter preview · d download · o open externally · esc close"
}

func pickerTitle(kind string) string {
	return map[string]string{"links": "Links", "attachments": "Attachments", "folders": "Folders", "move": "Move to…"}[kind]
}
