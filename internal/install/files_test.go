package install

import (
	"archive/zip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestWriteXPIContents(t *testing.T) {
	addon := fstest.MapFS{
		"manifest.json":         {Data: []byte(`{"manifest_version":2}`)},
		"background.js":         {Data: []byte("connect()")},
		"background.test.js":    {Data: []byte("test")},
		"lib/protocol.test.js":  {Data: []byte("test")},
		"lib/protocol.js":       {Data: []byte("proto")},
		"lib/PROTOCOL.md":       {Data: []byte("nested docs ship")},
		"PROTOCOL.md":           {Data: []byte("docs")},
		"icons/icon.png":        {Data: []byte{0x89, 'P', 'N', 'G'}},
		"lib/test.js.bak/x.txt": {Data: []byte("kept")},
	}
	path := filepath.Join(t.TempDir(), "a.xpi")
	if err := writeXPI(addon, path); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Fatalf("xpi mode %v, want 0644 so Thunderbird can read it", fi.Mode().Perm())
	}
	z, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	got := map[string]string{}
	for _, f := range z.File {
		if strings.HasPrefix(f.Name, "/") || strings.HasPrefix(f.Name, "./") {
			t.Errorf("entry %q not relative to the xpi root", f.Name)
		}
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r)
		r.Close()
		got[f.Name] = string(b)
	}
	want := map[string]string{
		"manifest.json":         `{"manifest_version":2}`,
		"background.js":         "connect()",
		"lib/protocol.js":       "proto",
		"lib/PROTOCOL.md":       "nested docs ship",
		"icons/icon.png":        "\x89PNG",
		"lib/test.js.bak/x.txt": "kept",
	}
	if len(got) != len(want) {
		t.Fatalf("xpi entries %v", keys(got))
	}
	for name, data := range want {
		if got[name] != data {
			t.Errorf("%s = %q, want %q", name, got[name], data)
		}
	}
}

func TestWriteXPIManifestInSubdirRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.xpi")
	if err := writeXPI(fstest.MapFS{"addon/manifest.json": {Data: []byte("{}")}}, path); err == nil {
		t.Fatal("manifest.json must be at the root")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("xpi written for a bad add-on")
	}
}

func keys(m map[string]string) (ks []string) {
	for k := range m {
		ks = append(ks, k)
	}
	return
}

func TestWriteHostManifestReplaces(t *testing.T) {
	root := t.TempDir()
	if _, err := writeHostManifest(root, "/old/lazythunder"); err != nil {
		t.Fatal(err)
	}
	path, err := writeHostManifest(root, "/new/lazythunder")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(b), "}\n") {
		t.Fatalf("manifest not newline-terminated: %q", b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["path"] != "/new/lazythunder" {
		t.Fatalf("path %v, want the new executable", m["path"])
	}
	if d, _ := m["description"].(string); d == "" {
		t.Fatal("description missing")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

func TestSetPrefsPreservesUserContent(t *testing.T) {
	ours := "user_pref(\"extensions.autoDisableScopes\", 14);\nuser_pref(\"webextensions.native-messaging.max-input-message-bytes\", 67108864);\n"
	for _, tc := range []struct{ name, old, want string }{
		{"empty file", "", ours},
		{"no trailing newline", "user_pref(\"a\", 1);", "user_pref(\"a\", 1);\n" + ours},
		{"blank lines and comments kept",
			"/* block\n   comment */\n\nuser_pref(\"a\", 1);\n\n// tail\n",
			"/* block\n   comment */\n\nuser_pref(\"a\", 1);\n\n// tail\n" + ours},
		{"commented-out pref kept",
			"// user_pref(\"extensions.autoDisableScopes\", 0);\n",
			"// user_pref(\"extensions.autoDisableScopes\", 0);\n" + ours},
		{"indented old value replaced",
			"  user_pref(\"webextensions.native-messaging.max-input-message-bytes\", 1024);\nuser_pref(\"b\", \"x\");\n",
			"user_pref(\"b\", \"x\");\n" + ours},
		{"duplicate old values all replaced",
			"user_pref(\"extensions.autoDisableScopes\", 15);\nuser_pref(\"extensions.autoDisableScopes\", 0);\n",
			ours},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "user.js")
			os.WriteFile(path, []byte(tc.old), 0o644)
			for i := range 3 { // idempotent
				if err := setPrefs(path); err != nil {
					t.Fatal(err)
				}
				if b, _ := os.ReadFile(path); string(b) != tc.want {
					t.Fatalf("run %d:\n%s\nwant:\n%s", i+1, b, tc.want)
				}
			}
		})
	}
}

func TestSetPrefsUnreadableIsError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "user.js")
	os.Mkdir(dir, 0o755) // a directory where user.js should be
	if err := setPrefs(dir); err == nil {
		t.Fatal("want error")
	}
}

func TestRemoveStockAddonLeavesOthers(t *testing.T) {
	prof := t.TempDir()
	ext := filepath.Join(prof, "extensions")
	os.MkdirAll(ext, 0o755)
	mine := filepath.Join(ext, "lazythunder@lazythunder.local.xpi")
	os.WriteFile(mine, nil, 0o644)
	os.WriteFile(filepath.Join(ext, "thunderbird-ai@extension.xpi"), nil, 0o644)
	if _, err := removeStockAddon(prof); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("lazythunder's own add-on removed")
	}
	if p, err := removeStockAddon(prof); p != "" || err != nil {
		t.Fatalf("second run: %q %v", p, err)
	}
}

func TestRemoveStockAddonError(t *testing.T) {
	prof := t.TempDir()
	stuck := filepath.Join(prof, "extensions", "thunderbird-ai@extension.xpi")
	os.MkdirAll(filepath.Join(stuck, "child"), 0o755) // non-empty dir: os.Remove fails
	if p, err := removeStockAddon(prof); err == nil || p != "" {
		t.Fatalf("got %q %v, want error", p, err)
	}
}
