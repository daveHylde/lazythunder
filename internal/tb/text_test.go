package tb

import "testing"

func TestHTMLToText(t *testing.T) {
	got := HTMLToText("<html><head><style>p{}</style></head><body><p>Hei &amp; hallo</p><div>linje<br>to</div>\r\n\r\n\r\n\r\n<b>slutt</b></body></html>")
	want := "Hei & hallo\nlinje\nto\n\nslutt"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestPlainTextPrefersText(t *testing.T) {
	var m FullMessage
	m.Parts.Text = "hi\r\nthere"
	m.Parts.HTML = "<p>other</p>"
	if got := m.PlainText(); got != "hi\nthere" {
		t.Fatalf("got %q", got)
	}
}

func TestFolderRefID(t *testing.T) {
	if got := (FolderRef{AccountID: "account3", Path: "/INBOX/Sent"}).ID(); got != "account3://INBOX/Sent" {
		t.Fatalf("got %q", got)
	}
}
