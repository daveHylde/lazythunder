// Package install sets up the lazythunder add-on in the default Thunderbird
// profile, registers lazythunder as its native-messaging host, and removes
// the thunderbird-cli add-on and npm packages it replaces.
package install

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/daveHylde/lazythunder/internal/hub"
	"github.com/daveHylde/lazythunder/internal/profile"
	"github.com/daveHylde/lazythunder/internal/tb"
)

// prefs are set in user.js: enable the profile-scope sideload without a
// prompt, and let the hub send the add-on messages up to 64 MiB.
var prefs = [][2]string{
	{"extensions.autoDisableScopes", "14"},
	{"webextensions.native-messaging.max-input-message-bytes", "67108864"},
}

// Run installs addon (the add-on's files, manifest.json at the root),
// printing each step to out.
func Run(ctx context.Context, addon fs.FS, out io.Writer) error {
	step := func(format string, args ...any) { fmt.Fprintf(out, "==> "+format+"\n", args...) }
	note := func(s string) { fmt.Fprintln(out, "    "+s) }

	step("finding the default Thunderbird profile")
	prof, err := profile.Dir()
	if err != nil {
		return err
	}
	note(prof)

	step("checking for a running Thunderbird")
	if len(tb.WindowedPIDs()) > 0 {
		return errors.New("close Thunderbird first")
	}
	if err := tb.StopHeadless(ctx, note); err != nil {
		return err
	}

	xpi := filepath.Join(prof, "extensions", hub.AddonID+".xpi")
	step("installing the add-on to %s", xpi)
	if err := writeXPI(addon, xpi); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	step("registering %s as the native-messaging host", exe)
	// Thunderbird on Linux looks in ~/.mozilla, not ~/.thunderbird (verified on 160).
	manifest, err := writeHostManifest(filepath.Join(home, ".mozilla"), exe)
	if err != nil {
		return err
	}
	note(manifest)

	userJS := filepath.Join(prof, "user.js")
	step("setting preferences in %s", userJS)
	if err := setPrefs(userJS); err != nil {
		return err
	}

	step("removing the thunderbird-cli add-on and npm packages")
	if removed, err := removeStockAddon(prof); err != nil {
		return err
	} else if removed != "" {
		note("removed " + removed)
	}
	if _, err := exec.LookPath("npm"); err != nil {
		note("warning: npm not found; skipping npm uninstall")
	} else if b, err := exec.CommandContext(ctx, "npm", "uninstall", "-g", "thunderbird-cli", "thunderbird-cli-bridge").CombinedOutput(); err != nil {
		note(fmt.Sprintf("warning: npm uninstall failed: %v: %s", err, strings.TrimSpace(string(b))))
	}

	step("starting Thunderbird headless and waiting for the add-on")
	if err := tb.New().Ensure(ctx, note); err != nil {
		return err
	}
	fmt.Fprintln(out, "lazythunder is installed: run `lazythunder`")
	return nil
}

// writeXPI zips addon (minus PROTOCOL.md and tests) to path, replacing it atomically.
func writeXPI(addon fs.FS, path string) error {
	if _, err := fs.Stat(addon, "manifest.json"); err != nil {
		return fmt.Errorf("embedded add-on has no manifest.json: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".lazythunder-*.xpi")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	z := zip.NewWriter(f)
	err = fs.WalkDir(addon, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || name == "PROTOCOL.md" || strings.HasSuffix(name, ".test.js") {
			return err
		}
		b, err := fs.ReadFile(addon, name)
		if err != nil {
			return err
		}
		w, err := z.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	})
	if err == nil {
		err = z.Close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// writeHostManifest registers exe as the "lazythunder" native-messaging host
// under root (~/.mozilla) and returns the manifest's path.
func writeHostManifest(root, exe string) (string, error) {
	dir := filepath.Join(root, "native-messaging-hosts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(map[string]any{
		"name":               "lazythunder",
		"description":        "lazythunder hub: relays the lazythunder add-on to the lazythunder TUI and daemon",
		"path":               exe,
		"type":               "stdio",
		"allowed_extensions": []string{hub.AddonID},
	}, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "lazythunder.json")
	return path, os.WriteFile(path, append(b, '\n'), 0o644)
}

// setPrefs sets prefs in user.js, replacing earlier lines for them and keeping the rest.
func setPrefs(path string) error {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(old), "\n"), "\n") {
		ours := false
		for _, p := range prefs {
			ours = ours || strings.Contains(l, `"`+p[0]+`"`) && strings.HasPrefix(strings.TrimSpace(l), "user_pref(")
		}
		if !ours && (l != "" || len(old) > 0) {
			lines = append(lines, l)
		}
	}
	for _, p := range prefs {
		lines = append(lines, fmt.Sprintf("user_pref(%q, %s);", p[0], p[1]))
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// removeStockAddon deletes the thunderbird-cli add-on's xpi, returning its
// path if it was there.
func removeStockAddon(prof string) (string, error) {
	path := filepath.Join(prof, "extensions", "thunderbird-ai@extension.xpi")
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return path, nil
}
