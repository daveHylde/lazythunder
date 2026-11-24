package tb

import "testing"

func TestFolderRefID(t *testing.T) {
	if got := (FolderRef{AccountID: "account3", Path: "/INBOX/Sent"}).ID(); got != "account3://INBOX/Sent" {
		t.Fatalf("got %q", got)
	}
}
