package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "file")
	for _, content := range []string{"first", "second"} {
		if err := Write(path, []byte(content)); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatalf("read %q, %v; want %q", got, err, content)
		}
	}

	for p, want := range map[string]os.FileMode{path: 0600, dir: 0700 | os.ModeDir} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != want {
			t.Errorf("%s mode = %s, want %s", p, info.Mode(), want)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir holds %d entries, want only the file", len(entries))
	}
}

func TestWrite_FailureLeavesFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := Write(path, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	if err := Write(path, []byte("new")); err == nil {
		t.Fatal("write into read-only dir succeeded")
	}
	if got, _ := os.ReadFile(path); string(got) != "old" {
		t.Fatalf("file = %q, want old", got)
	}
}

func TestWrite_SucceedsOnceFileIsReplaced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.Chmod(dir, 0300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	if err := Write(path, []byte("new")); err != nil {
		t.Fatalf("write reported failure after replacing the file: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatalf("file = %q, want new", got)
	}
}
