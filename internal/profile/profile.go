// Package profile finds the default Thunderbird profile directory.
package profile

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Root is Thunderbird's per-user directory, ~/.thunderbird.
func Root() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".thunderbird"), nil
}

// Dir returns the default profile: the first install's Default= in
// installs.ini, else the profiles.ini profile with Default=1.
func Dir() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	if secs, err := readINI(filepath.Join(root, "installs.ini")); err == nil {
		for _, s := range secs {
			if p := s.kv["Default"]; p != "" {
				return resolve(root, p, true), nil
			}
		}
	}
	secs, err := readINI(filepath.Join(root, "profiles.ini"))
	if err != nil {
		return "", err
	}
	for _, s := range secs {
		if strings.HasPrefix(s.name, "Profile") && s.kv["Default"] == "1" && s.kv["Path"] != "" {
			return resolve(root, s.kv["Path"], s.kv["IsRelative"] != "0"), nil
		}
	}
	return "", errors.New("no default Thunderbird profile in " + filepath.Join(root, "profiles.ini"))
}

func resolve(root, p string, relative bool) string {
	if relative && !filepath.IsAbs(p) {
		return filepath.Join(root, p)
	}
	return p
}

type section struct {
	name string
	kv   map[string]string
}

func readINI(path string) ([]section, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var secs []section
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			secs = append(secs, section{name: line[1 : len(line)-1], kv: map[string]string{}})
		} else if k, v, ok := strings.Cut(line, "="); ok && len(secs) > 0 {
			secs[len(secs)-1].kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return secs, sc.Err()
}
