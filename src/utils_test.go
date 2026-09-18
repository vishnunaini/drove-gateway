package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomicWritesContentAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	content := []byte(`{"version":1}`)

	if err := writeFileAtomic(path, content, 0o640); err != nil {
		t.Fatalf("writeFileAtomic() error = %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if string(written) != string(content) {
		t.Fatalf("written content = %q, want %q", written, content)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("os.Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("written mode = %o, want 640", info.Mode().Perm())
	}
}
