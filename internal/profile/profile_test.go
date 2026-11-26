package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".thunderbird")
	os.MkdirAll(root, 0o755)
	write := func(name, s string) { os.WriteFile(filepath.Join(root, name), []byte(s), 0o644) }

	if _, err := Dir(); err == nil {
		t.Fatal("no ini files: want error")
	}
	write("profiles.ini", "[General]\nStartWithLastProfile=1\n\n[Profile1]\nName=old\nIsRelative=1\nPath=old.default\n\n[Profile0]\nName=main\nIsRelative=1\nPath=abc.default-release\nDefault=1\n")
	if d, err := Dir(); err != nil || d != filepath.Join(root, "abc.default-release") {
		t.Fatalf("profiles.ini: %q %v", d, err)
	}
	write("installs.ini", "[308046B0AF4A39CB]\nDefault=xyz.default-esr\nLocked=1\n\n[OTHER]\nDefault=other\n")
	if d, err := Dir(); err != nil || d != filepath.Join(root, "xyz.default-esr") {
		t.Fatalf("installs.ini: %q %v", d, err)
	}
	write("installs.ini", "[308046B0AF4A39CB]\nDefault=/abs/profile\n")
	if d, _ := Dir(); d != "/abs/profile" {
		t.Fatalf("absolute: %q", d)
	}
}
