package tb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCaseVariants(t *testing.T) {
	got := strings.Join(caseVariants("faktura fra"), "|")
	if got != "faktura fra|FAKTURA FRA|Faktura fra|Faktura Fra" {
		t.Fatalf("got %q", got)
	}
	if got := strings.Join(caseVariants("GitHub"), "|"); got != "GitHub|github|GITHUB|Github" {
		t.Fatalf("got %q", got)
	}
}

func TestRunNeverHangs(t *testing.T) {
	runSlack = 200 * time.Millisecond
	defer func() { runSlack = 30 * time.Second }()
	bin := filepath.Join(t.TempDir(), "tb")
	os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30 &\nsleep 30\n"), 0o755) // a child keeps stdout open too
	start := time.Now()
	err := (&Client{Bin: bin}).Run(context.Background(), nil, "--timeout", "100", "read", "1")
	if err == nil || time.Since(start) > 7*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}
