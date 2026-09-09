package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveBaseDirFindsProjectRootFromServerSubdirectory(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "cmd", "server")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module obsigon\n\ngo 1.23.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveBaseDir(nested); got != root {
		t.Fatalf("base=%q; se esperaba %q", got, root)
	}
}

func TestResolveBaseDirKeepsStandaloneWorkingDirectory(t *testing.T) {
	standalone := t.TempDir()
	if got := resolveBaseDir(standalone); got != standalone {
		t.Fatalf("base=%q; se esperaba %q", got, standalone)
	}
}
