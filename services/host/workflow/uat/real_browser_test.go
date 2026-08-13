package uat

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserAllocatorArgv(t *testing.T) {
	tmpDir := t.TempDir()
	fakeBin := filepath.Join(tmpDir, "fake-chrome")
	os.WriteFile(fakeBin, []byte("#!/bin/sh\necho \"$@\" > "+filepath.Join(tmpDir, "args.txt")+"\nsleep 1\n"), 0755)

	os.Setenv("PIX_CHROME_BIN", fakeBin)
	defer os.Unsetenv("PIX_CHROME_BIN")

	factory := NewRealBrowserFactory()
	
	u, _ := url.Parse("https://public.com/test")
	valU := &ValidatedURL{
		URL:        u,
		ResolvedIP: "93.184.216.34",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	b, err := factory.NewContext(ctx, "testrun1", valU, &PublicLinkPolicy{})
	if b != nil {
		defer b.Close()
	}

	// wait a bit for script to write args
	time.Sleep(100 * time.Millisecond)

	// read args.txt
	argsBytes, err := os.ReadFile(filepath.Join(tmpDir, "args.txt"))
	if err != nil {
		t.Fatalf("failed to read args: %v", err)
	}
	args := string(argsBytes)

	if !strings.Contains(args, "--host-resolver-rules=MAP public.com 93.184.216.34") {
		t.Errorf("expected host-resolver-rules in argv, got: %s", args)
	}
}
