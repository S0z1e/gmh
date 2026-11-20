package workspace

import (
	"os"
	"testing"
)

func TestDirectoryFileIDMatchesPathIdentity(t *testing.T) {
	path := t.TempDir()
	identity, err := Identify(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	id, err := DirectoryFileID(f)
	if err != nil || id != identity.FileID {
		t.Fatalf("handle=%q path=%q err=%v", id, identity.FileID, err)
	}
	file, err := os.CreateTemp(path, "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := DirectoryFileID(file); err == nil {
		t.Fatal("regular file accepted as a directory")
	}
}
