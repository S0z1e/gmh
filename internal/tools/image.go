package tools

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"

	"minimalharness/internal/llm"
)

const (
	// MaxImageBytes caps one attached image. 10 MiB is far above a
	// screenshot and below what would bloat a request or a session.
	MaxImageBytes = 10 << 20
	// MaxImagesPerMessage caps the images on one user message.
	MaxImagesPerMessage = 10
)

var imageMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// MediaTypeForPath returns the media type for a supported image
// extension (png, jpg, gif, webp), or false for anything else.
func MediaTypeForPath(path string) (string, bool) {
	mt, ok := imageMediaTypes[strings.ToLower(filepath.Ext(path))]
	return mt, ok
}

// LoadImage reads an image file for an explicit user attachment
// (-image, /image, RPC attach_image). There is no workspace jail
// here: the user named the file themselves, so an absolute path is
// used as-is and a relative path resolves against the workspace
// root. The file must be a supported image type under the size cap,
// and the selected model must declare image input in the config
// (Executor.ImageInput); otherwise the error says exactly what to
// change. Dimensions are filled in when the stdlib can decode the
// header (png/jpeg/gif); webp has no stdlib decoder, so it reports
// bytes only.
func (e *Executor) LoadImage(path string) (llm.Image, error) {
	if !e.ImageInput {
		return llm.Image{}, fmt.Errorf("image input is not enabled for this model: add \"image\" to the model's input in the config file")
	}
	mt, ok := MediaTypeForPath(path)
	if !ok {
		return llm.Image{}, fmt.Errorf("not a supported image file (png, jpg, gif, webp): %s", path)
	}
	var disk string
	if filepath.IsAbs(path) {
		disk = filepath.Clean(path)
	} else {
		disk = filepath.Join(e.Root, filepath.Clean(path))
	}
	return e.loadImageFile(disk, path, mt)
}

// loadImageFile is the shared core: stat, size cap, read, base64,
// dimensions. diskPath is where the bytes live; displayPath is the
// path recorded on the image (and shown to the model).
func (e *Executor) loadImageFile(diskPath, displayPath, mt string) (llm.Image, error) {
	info, err := os.Stat(diskPath)
	if err != nil {
		return llm.Image{}, err
	}
	if info.IsDir() {
		return llm.Image{}, fmt.Errorf("not a file: %s", displayPath)
	}
	if info.Size() > MaxImageBytes {
		return llm.Image{}, fmt.Errorf("image %s is %d bytes, over the %d-byte cap", displayPath, info.Size(), MaxImageBytes)
	}
	data, err := os.ReadFile(diskPath)
	if err != nil {
		return llm.Image{}, err
	}
	img := llm.Image{
		Path:      displayPath,
		MediaType: mt,
		Data:      base64.StdEncoding.EncodeToString(data),
		Bytes:     len(data),
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		img.Width, img.Height = cfg.Width, cfg.Height
	}
	return img, nil
}
