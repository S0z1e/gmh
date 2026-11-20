package release

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func fixtureBuild(_ context.Context, t Target, pkg, dest string) error {
	return os.WriteFile(dest, []byte(t.OS+"/"+t.Arch+":"+pkg), 0755)
}
func readArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	if strings.HasSuffix(path, ".zip") {
		z, err := zip.OpenReader(path)
		if err != nil {
			t.Fatal(err)
		}
		defer z.Close()
		for _, f := range z.File {
			r, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(r)
			r.Close()
			if err != nil {
				t.Fatal(err)
			}
			out[f.Name] = b
		}
	} else {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if h.Typeflag != tar.TypeReg || strings.ContainsAny(h.Name, "/\\") {
				t.Fatalf("unsafe archive member: %+v", h)
			}
			if h.Name != "manifest.json" && h.Mode != 0755 {
				t.Fatalf("executable mode: %o", h.Mode)
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			out[h.Name] = b
		}
	}
	return out
}
func TestPackageContract(t *testing.T) {
	for _, target := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"} {
		t.Run(target, func(t *testing.T) {
			target, _ := ParseTarget(target)
			out := t.TempDir()
			path, err := Package(context.Background(), target, "v1.2.3", out, fixtureBuild)
			if err != nil {
				t.Fatal(err)
			}
			files := readArchive(t, path)
			var manifest Manifest
			if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
				t.Fatal(err)
			}
			if len(files) != 3 || manifest.Version != "v1.2.3" || manifest.OS != target.OS || manifest.Arch != target.Arch {
				t.Fatalf("manifest=%+v files=%v", manifest, files)
			}
			ext := ""
			if target.OS == "windows" {
				ext = ".exe"
			}
			for _, name := range []string{"harness" + ext, "harness-tui" + ext} {
				b, ok := files[name]
				if !ok {
					t.Fatalf("missing sibling %s", name)
				}
				sum := sha256.Sum256(b)
				if manifest.Files[name] != hex.EncodeToString(sum[:]) {
					t.Fatalf("checksum mismatch: %s", name)
				}
			}
			second, err := Package(context.Background(), target, "v1.2.3", t.TempDir(), fixtureBuild)
			if err != nil {
				t.Fatal(err)
			}
			b1, _ := os.ReadFile(path)
			b2, _ := os.ReadFile(second)
			if !bytes.Equal(b1, b2) {
				t.Fatal("same inputs produced different archive bytes")
			}
			if _, err := Package(context.Background(), target, "v1.2.3", out, fixtureBuild); err == nil {
				t.Fatal("replaced existing release")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(after, b1) {
				t.Fatal("existing release changed")
			}
		})
	}
}
func TestPackageFailureLeavesNoArchive(t *testing.T) {
	target, _ := ParseTarget("linux/amd64")
	for _, version := range []string{"", "../bad", "bad/name"} {
		if _, err := Package(context.Background(), target, version, t.TempDir(), fixtureBuild); err == nil {
			t.Fatalf("accepted %q", version)
		}
	}
	out := t.TempDir()
	if _, err := Package(context.Background(), target, "v1", out, func(context.Context, Target, string, string) error { return fmt.Errorf("build failed") }); err == nil {
		t.Fatal("ignored build failure")
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Fatalf("left partial release: %v", entries)
	}
	if _, err := ParseTarget("plan9/amd64"); err == nil {
		t.Fatal("accepted unsupported target")
	}
}
