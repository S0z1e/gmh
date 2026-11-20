package release

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Target struct{ OS, Arch string }

func ParseTarget(s string) (Target, error) {
	switch s {
	case "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64":
		p := strings.Split(s, "/")
		return Target{p[0], p[1]}, nil
	}
	return Target{}, fmt.Errorf("unsupported target %q", s)
}

type Manifest struct {
	Version string            `json:"version"`
	OS      string            `json:"os"`
	Arch    string            `json:"arch"`
	Files   map[string]string `json:"sha256"`
}
type Build func(context.Context, Target, string, string) error

func BuildGo(ctx context.Context, t Target, pkg, dest string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags=-s -w", "-o", dest, pkg)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GOOS=") && !strings.HasPrefix(v, "GOARCH=") && !strings.HasPrefix(v, "CGO_ENABLED=") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "GOOS="+t.OS, "GOARCH="+t.Arch, "CGO_ENABLED=0")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
func Package(ctx context.Context, t Target, version, out string, build Build) (string, error) {
	if _, err := ParseTarget(t.OS + "/" + t.Arch); err != nil {
		return "", err
	}
	if version == "" || len(version) > 100 {
		return "", fmt.Errorf("invalid release version")
	}
	for _, r := range version {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-+", r)) {
			return "", fmt.Errorf("invalid release version")
		}
	}
	if build == nil {
		build = BuildGo
	}
	if err := os.MkdirAll(out, 0755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(out, ".package-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	manifest := Manifest{Version: version, OS: t.OS, Arch: t.Arch, Files: map[string]string{}}
	ext := ""
	if t.OS == "windows" {
		ext = ".exe"
	}
	names := []string{"harness" + ext, "harness-tui" + ext}
	for _, name := range names {
		path := filepath.Join(staging, name)
		if err := build(ctx, t, "./cmd/"+strings.TrimSuffix(name, ext), path); err != nil {
			return "", fmt.Errorf("build %s: %w", name, err)
		}
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		sum := sha256.New()
		_, copyErr := io.Copy(sum, f)
		closeErr := f.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		manifest.Files[name] = hex.EncodeToString(sum.Sum(nil))
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), append(data, '\n'), 0644); err != nil {
		return "", err
	}
	names = append(names, "manifest.json")
	suffix := ".tar.gz"
	if t.OS == "windows" {
		suffix = ".zip"
	}
	final := filepath.Join(out, "harness-"+version+"-"+t.OS+"-"+t.Arch+suffix)
	f, err := os.CreateTemp(out, ".archive-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	err = writeArchive(f, staging, names, t.OS == "windows")
	if err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	// Both paths are on the same filesystem. Link publishes exclusively:
	// concurrent packages cannot replace an already published release.
	if err := os.Link(f.Name(), final); err != nil {
		return "", err
	}
	return final, nil
}
func copyFile(w io.Writer, dir, name string) error {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
func writeArchive(w io.Writer, dir string, names []string, windows bool) error {
	if windows {
		zw := zip.NewWriter(w)
		for _, name := range names {
			h := &zip.FileHeader{Name: name, Method: zip.Deflate}
			mode := os.FileMode(0755)
			if name == "manifest.json" {
				mode = 0644
			}
			h.SetMode(mode)
			h.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
			entry, err := zw.CreateHeader(h)
			if err != nil {
				return err
			}
			if err := copyFile(entry, dir, name); err != nil {
				return err
			}
		}
		return zw.Close()
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		mode := int64(0755)
		if name == "manifest.json" {
			mode = 0644
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: st.Size(), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if err := copyFile(tw, dir, name); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
