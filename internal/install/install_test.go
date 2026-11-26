package install

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestWriteXPI(t *testing.T) {
	addon := fstest.MapFS{
		"manifest.json":   {Data: []byte(`{"manifest_version":2}`)},
		"background.js":   {Data: []byte("connect()")},
		"lib/protocol.js": {Data: []byte("x")},
		"PROTOCOL.md":     {Data: []byte("docs")},
	}
	path := filepath.Join(t.TempDir(), "prof", "extensions", "lazythunder@lazythunder.local.xpi")
	for range 2 { // replacing an existing xpi works too
		if err := writeXPI(addon, path); err != nil {
			t.Fatal(err)
		}
	}
	z, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	var names []string
	for _, f := range z.File {
		names = append(names, f.Name)
	}
	slices.Sort(names)
	if got := strings.Join(names, " "); got != "background.js lib/protocol.js manifest.json" {
		t.Fatalf("xpi has %s", got)
	}
	if err := writeXPI(fstest.MapFS{"PROTOCOL.md": {}}, path); err == nil {
		t.Fatal("add-on without manifest.json accepted")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".lazythunder-*")); len(left) > 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestWriteHostManifest(t *testing.T) {
	root := t.TempDir()
	path, err := writeHostManifest(root, "/usr/local/bin/lazythunder")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(root, "native-messaging-hosts", "lazythunder.json") {
		t.Fatalf("path %s", path)
	}
	b, _ := os.ReadFile(path)
	var m struct {
		Name, Path, Type  string
		AllowedExtensions []string `json:"allowed_extensions"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "lazythunder" || m.Path != "/usr/local/bin/lazythunder" || m.Type != "stdio" ||
		len(m.AllowedExtensions) != 1 || m.AllowedExtensions[0] != "lazythunder@lazythunder.local" {
		t.Fatalf("manifest %+v", m)
	}
}

func TestSetPrefs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.js")
	want := "user_pref(\"extensions.autoDisableScopes\", 14);\nuser_pref(\"webextensions.native-messaging.max-input-message-bytes\", 67108864);\n"
	if err := setPrefs(path); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != want {
		t.Fatalf("new user.js:\n%s", b)
	}
	os.WriteFile(path, []byte("// mine\nuser_pref(\"mail.foo\", true);\nuser_pref(\"extensions.autoDisableScopes\", 15);\nuser_pref(\"extensions.autoDisableScopesX\", 1);\n"), 0o644)
	for range 2 {
		if err := setPrefs(path); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(path)
	if string(b) != "// mine\nuser_pref(\"mail.foo\", true);\nuser_pref(\"extensions.autoDisableScopesX\", 1);\n"+want {
		t.Fatalf("updated user.js:\n%s", b)
	}
}

func TestRemoveStockAddon(t *testing.T) {
	prof := t.TempDir()
	if p, err := removeStockAddon(prof); p != "" || err != nil {
		t.Fatalf("absent: %q %v", p, err)
	}
	os.MkdirAll(filepath.Join(prof, "extensions"), 0o755)
	xpi := filepath.Join(prof, "extensions", "thunderbird-ai@extension.xpi")
	os.WriteFile(xpi, nil, 0o644)
	if p, err := removeStockAddon(prof); p != xpi || err != nil {
		t.Fatalf("present: %q %v", p, err)
	}
	if _, err := os.Stat(xpi); !os.IsNotExist(err) {
		t.Fatal("still there")
	}
}
