// Package outputreceipt records durable delivery separately from output bytes.
// Missing or malformed receipts never authorize expiration.
package outputreceipt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const Retention = 7 * 24 * time.Hour

func directory(outputDir string) string {
	return filepath.Clean(outputDir) + "-delivered"
}

func validateTarget(outputDir, outputPath string) (string, string, error) {
	path, err := filepath.Abs(outputPath)
	if err != nil {
		return "", "", err
	}
	dir, err := filepath.Abs(outputDir)
	if err != nil {
		return "", "", err
	}
	if filepath.Dir(path) != dir {
		return "", "", fmt.Errorf("output receipt outside owner directory")
	}
	name := filepath.Base(path)
	if !strings.HasPrefix(name, "bash-") || !strings.HasSuffix(name, ".log") {
		return "", "", fmt.Errorf("invalid bash output name")
	}
	return dir, name, nil
}

// Path returns the delivery receipt path for outputPath inside outputDir, or ""
// if outputPath is empty or outside outputDir.
func Path(outputDir, outputPath string) string {
	if outputDir == "" || outputPath == "" {
		return ""
	}
	dir, name, err := validateTarget(outputDir, outputPath)
	if err != nil {
		return ""
	}
	return filepath.Join(directory(dir), name)
}

// Exists reports whether a valid delivery receipt file exists for outputPath.
func Exists(outputDir, outputPath string) bool {
	if outputDir == "" || outputPath == "" {
		return false
	}
	dir, name, err := validateTarget(outputDir, outputPath)
	if err != nil {
		return false
	}
	root, err := os.OpenRoot(directory(dir))
	if err != nil {
		return false
	}
	defer root.Close()
	info, err := root.Lstat(name)
	return err == nil && info.Mode().IsRegular() && info.Size() == 0
}

// Record creates a receipt after a successful transcript Sync. The filename is
// an exclusive random output name; an existing receipt preserves its first
// delivery time. No receipt is created for a foreign path or nonregular output.
func Record(outputDir, outputPath string) error {
	if outputPath == "" {
		return nil
	}
	dir, name, err := validateTarget(outputDir, outputPath)
	if err != nil {
		return err
	}
	output, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer output.Close()
	info, err := output.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("output receipt requires a regular file")
	}
	if err := os.MkdirAll(directory(dir), 0700); err != nil {
		return err
	}
	receipts, err := os.OpenRoot(directory(dir))
	if err != nil {
		return err
	}
	defer receipts.Close()
	f, err := receipts.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Expired checks only metadata: no unbounded or potentially blocking file read.
func Expired(outputDir, name string, now time.Time) bool {
	root, err := os.OpenRoot(directory(outputDir))
	if err != nil {
		return false
	}
	defer root.Close()
	info, err := root.Lstat(name)
	return err == nil && info.Mode().IsRegular() && info.Size() == 0 && !info.ModTime().After(now.Add(-Retention))
}

func Remove(outputDir, name string) {
	root, err := os.OpenRoot(directory(outputDir))
	if err != nil {
		return
	}
	defer root.Close()
	_ = root.Remove(name)
}
