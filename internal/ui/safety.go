package ui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/davidhyldgaard/lazythunder/internal/render"
	"github.com/davidhyldgaard/lazythunder/internal/tb"
)

// attKind is what lazythunder may do with an attachment.
type attKind int

const (
	attOther attKind = iota // no preview; open/download only
	attImage                // decoded in-process by Go (memory-safe)
	attPDF                  // text extracted by pdftotext inside bwrap
	attText                 // shown as text, never interpreted
	attRisky                // never previewed; opening needs explicit confirmation
)

var riskyExts = map[string]string{}

func init() {
	for reason, exts := range map[string]string{
		"an executable":                        "exe msi com scr pif cpl dll so dylib bin run appimage apk app deb rpm jar msix appx",
		"a script":                             "bat cmd vbs vbe js jse mjs wsf wsh ps1 psm1 sh bash zsh fish py pyw pl rb php hta reg scpt applescript command",
		"a shortcut":                           "lnk url desktop webloc inf settingcontent-ms",
		"an archive":                           "zip rar 7z tar gz tgz bz2 xz zst cab arj lzh ace iso img dmg vhd vhdx",
		"a macro Office file":                  "docm dotm xlsm xltm xlam xlsb pptm potm ppsm ppam sldm one",
		"a web page (scripts run when opened)": "html htm xhtml svg mht mhtml",
	} {
		for _, e := range strings.Fields(exts) {
			riskyExts["."+e] = reason
		}
	}
}

// classify decides by name and declared type. Every dot-extension counts, so
// "invoice.pdf.exe" is an executable, and it checks both the raw name and the
// name the file is saved under (invisible characters, bidi overrides and path
// junk removed), so "a.e\u200bxe" or "evil.exe/" can't pass as harmless.
func classify(name, contentType string) (attKind, string) {
	ct := strings.ToLower(contentType)
	for _, n := range []string{name, safeName(name), stripAll(name)} {
		for _, part := range strings.Split(strings.ToLower(strings.TrimSpace(n)), ".")[1:] {
			if r, ok := riskyExts["."+strings.Trim(part, " \t/\\")]; ok {
				return attRisky, r
			}
		}
	}
	for _, t := range []string{"x-msdownload", "x-executable", "x-sh", "javascript", "x-msdos-program", "vnd.microsoft.portable-executable", "x-dosexec", "java-archive", "x-shellscript", "html", "zip", "x-rar", "x-7z", "macroenabled"} {
		if strings.Contains(ct, t) {
			return attRisky, "declared as " + oneLine(contentType)
		}
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case strings.HasPrefix(ct, "image/") && ct != "image/svg+xml" || ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".gif" || ext == ".webp":
		return attImage, ""
	case ct == "application/pdf" || ext == ".pdf":
		return attPDF, ""
	case isTextual(ct, ext):
		return attText, ""
	}
	return attOther, ""
}

func classifyKind(a tb.Attachment) attKind {
	k, _ := classify(a.Name, a.ContentType)
	return k
}

// stripAll removes every invisible, control and bidi character from a name.
func stripAll(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || isInvisibleRune(r) {
			return -1
		}
		return r
	}, s)
}

func isInvisibleRune(r rune) bool {
	return r >= 0x200b && r <= 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x2060 && r <= 0x2069 ||
		r == 0xfeff || r == 0x00ad || r == 0x034f || r == 0x180e
}

// oneLine makes untrusted text safe for a single UI row: no escapes, no newlines.
func oneLine(s string) string { return strings.Join(strings.Fields(render.Sanitize(s)), " ") }

// sniffRisky checks a downloaded file's first bytes: names and declared types
// lie, so executables, scripts, launchers, archives and active documents are
// caught by content too.
func sniffRisky(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "unreadable"
	}
	defer f.Close()
	head := make([]byte, 4096)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	trimmed := bytes.TrimLeft(bytes.TrimPrefix(head, []byte("\xef\xbb\xbf")), " \t\r\n")
	lower := bytes.ToLower(trimmed)
	switch {
	case bytes.HasPrefix(head, []byte("MZ")):
		return "a Windows executable (by content)"
	case bytes.HasPrefix(head, []byte("\x7fELF")):
		return "a Linux executable (by content)"
	case bytes.HasPrefix(trimmed, []byte("#!")):
		return "a script (by content)"
	case bytes.HasPrefix(head, []byte("\xcf\xfa\xed\xfe")), bytes.HasPrefix(head, []byte("\xce\xfa\xed\xfe")),
		bytes.HasPrefix(head, []byte("\xfe\xed\xfa\xce")), bytes.HasPrefix(head, []byte("\xfe\xed\xfa\xcf")),
		bytes.HasPrefix(head, []byte("\xca\xfe\xba\xbe")):
		return "a macOS executable (by content)"
	case bytes.Contains(head, []byte("[Desktop Entry]")):
		return "a desktop launcher (by content)"
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return "a zip-based file (archive, jar or Office document; by content)"
	case bytes.HasPrefix(head, []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1")):
		return "a legacy Office/OLE file that can carry macros (by content)"
	case bytes.HasPrefix(lower, []byte("<html")), bytes.HasPrefix(lower, []byte("<!doctype html")),
		bytes.HasPrefix(lower, []byte("<svg")), bytes.HasPrefix(lower, []byte("<script")), bytes.HasPrefix(lower, []byte("<?xml")) && bytes.Contains(lower, []byte("<svg")):
		return "a web page or SVG that can run scripts (by content)"
	}
	return ""
}

// pdfText extracts the first pages of a PDF inside bubblewrap: no network,
// read-only /usr, no home directory, only the one file visible. PDF parsers
// are C code handling hostile input, so they never run unconfined here.
func pdfText(ctx context.Context, path string, pages int) (string, error) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return "", fmt.Errorf("PDF preview needs bubblewrap (bwrap) for sandboxing")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bwrap",
		"--unshare-all", "--die-with-parent", "--new-session", "--clearenv",
		"--ro-bind", "/usr", "/usr", "--symlink", "usr/bin", "/bin", "--symlink", "usr/lib", "/lib", "--symlink", "usr/lib", "/lib64",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--ro-bind", path, "/tmp/in.pdf",
		"pdftotext", "-layout", "-l", fmt.Sprint(pages), "-enc", "UTF-8", "/tmp/in.pdf", "-")
	var out bytes.Buffer
	cmd.Stdout = &limitWriter{w: &out, n: maxPreviewBytes}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftotext: %w", err)
	}
	return out.String(), nil
}

// limitWriter keeps the first n bytes and discards the rest.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
