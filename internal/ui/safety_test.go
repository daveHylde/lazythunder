package ui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := map[[2]string]attKind{
		{"faktura.pdf", "application/pdf"}:          attPDF,
		{"faktura.pdf.exe", "application/pdf"}:      attRisky, // double extension
		{"Faktura.PDF .EXE", ""}:                    attRisky,
		{"photo.jpg", "image/jpeg"}:                 attImage,
		{"logo.svg", "image/svg+xml"}:               attRisky, // svg can carry scripts
		{"budget.xlsm", "application/vnd.ms-excel"}: attRisky,
		{"report.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}: attOther,
		{"notes.txt", "text/plain"}:             attText,
		{"invoice", "application/x-msdownload"}: attRisky, // declared type
		{"archive.tar.gz", "application/gzip"}:  attRisky,
		{"README", "application/octet-stream"}:  attOther,
	}
	for in, want := range cases {
		if got, _ := classify(in[0], in[1]); got != want {
			t.Errorf("%v: got %v want %v", in, got, want)
		}
	}
}

func TestSniffRisky(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{"a": "MZ\x90\x00", "b": "\x7fELF\x02", "c": "#!/bin/sh\n", "d": "%PDF-1.7",
		"e": "\xef\xbb\xbf  #!/bin/sh", "f": "[Desktop Entry]\nExec=rm -rf ~", "g": "PK\x03\x04...", "h": "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1",
		"i": "  <!DOCTYPE html><script>", "j": "\xfe\xed\xfa\xcf", "k": "Hello, plain text"}
	for name, content := range files {
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	}
	for name, risky := range map[string]bool{"a": true, "b": true, "c": true, "d": false, "e": true, "f": true, "g": true, "h": true, "i": true, "j": true, "k": false} {
		if got := sniffRisky(filepath.Join(dir, name)) != ""; got != risky {
			t.Errorf("%s: risky=%v", name, got)
		}
	}
}

func TestPDFTextSandboxed(t *testing.T) {
	if _, err := os.Stat("/tmp/lt-harness/t.pdf"); err != nil {
		t.Skip("no sample pdf")
	}
	out, err := pdfText(t.Context(), "/tmp/lt-harness/t.pdf", 1)
	if err != nil || len(out) == 0 {
		t.Fatalf("err=%v len=%d", err, len(out))
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("a\nb\x1b[31m c\u202e"); got != "a b[31m c" {
		t.Fatalf("got %q", got)
	}
}
