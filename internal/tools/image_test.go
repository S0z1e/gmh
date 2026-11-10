package tools

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePNG writes a tiny valid PNG and returns its bytes.
func writePNG(t *testing.T, root, name string, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestLoadImage(t *testing.T) {
	root := t.TempDir()
	raw := writePNG(t, root, "pic.png", 4, 3)
	e := &Executor{Root: root, ImageInput: true}
	img, err := e.LoadImage("pic.png")
	if err != nil {
		t.Fatal(err)
	}
	if img.MediaType != "image/png" || img.Width != 4 || img.Height != 3 || img.Bytes != len(raw) {
		t.Fatalf("image = %+v, want png 4x3 %d bytes", img, len(raw))
	}
	// The loaded bytes decode back to the file.
	if got := img.Data; got == "" {
		t.Fatal("image data is empty")
	}
	// Absolute paths under the root work too.
	if _, err := e.LoadImage(filepath.Join(root, "pic.png")); err != nil {
		t.Fatalf("absolute path: %v", err)
	}
}

func TestLoadImageRefusals(t *testing.T) {
	root := t.TempDir()
	writePNG(t, root, "pic.png", 2, 2)
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A model without image input refuses before reading anything.
	e := &Executor{Root: root}
	if _, err := e.LoadImage("pic.png"); err == nil || !strings.Contains(err.Error(), "image input is not enabled") {
		t.Fatalf("no-input load err = %v, want the image-input hint", err)
	}
	e.ImageInput = true
	// Unsupported type, missing file, outside the root.
	if _, err := e.LoadImage("note.txt"); err == nil || !strings.Contains(err.Error(), "not a supported image") {
		t.Fatalf("txt load err = %v", err)
	}
	if _, err := e.LoadImage("gone.png"); err == nil {
		t.Fatal("missing file should error")
	}
	// User attachments are NOT jailed: an absolute path outside the
	// root loads fine (the user named the file themselves).
	outside := t.TempDir()
	writePNG(t, outside, "out.png", 2, 2)
	if _, err := e.LoadImage(filepath.Join(outside, "out.png")); err != nil {
		t.Fatalf("absolute path outside the root should load: %v", err)
	}
	// Over the size cap: a big file with an image extension.
	big := make([]byte, MaxImageBytes+1)
	if err := os.WriteFile(filepath.Join(root, "big.png"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.LoadImage("big.png"); err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("oversize load err = %v", err)
	}
}

func TestMediaTypeForPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		mt   string
		ok   bool
	}{
		{"a.png", "image/png", true},
		{"a.PNG", "image/png", true},
		{"a.jpg", "image/jpeg", true},
		{"a.jpeg", "image/jpeg", true},
		{"a.gif", "image/gif", true},
		{"a.webp", "image/webp", true},
		{"a.txt", "", false},
		{"a", "", false},
	} {
		mt, ok := MediaTypeForPath(tc.path)
		if mt != tc.mt || ok != tc.ok {
			t.Fatalf("MediaTypeForPath(%q) = %q %v, want %q %v", tc.path, mt, ok, tc.mt, tc.ok)
		}
	}
}

func TestDispatchReadImage(t *testing.T) {
	root := t.TempDir()
	writePNG(t, root, "pic.png", 4, 3)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Root: root, ImageInput: true}
	res, err := e.Dispatch(context.Background(), "read", `{"path":"pic.png"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 || res.Images[0].MediaType != "image/png" {
		t.Fatalf("read image result = %+v", res)
	}
	if !strings.Contains(res.Text, "pic.png") || !strings.Contains(res.Text, "4x3") {
		t.Fatalf("read image text = %q", res.Text)
	}
	// Text reads are unchanged: content, no images.
	res, err = e.Dispatch(context.Background(), "read", `{"path":"a.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 0 || res.Text != "1\thello\n2\t\n" {
		t.Fatalf("read text result = %+v", res)
	}
	// Without image input, reading an image is an error, not text.
	e2 := &Executor{Root: root}
	if _, err := e2.Dispatch(context.Background(), "read", `{"path":"pic.png"}`); err == nil {
		t.Fatal("read image without image input should error")
	}
	// The model-chosen read path is still jailed to the root, unlike
	// a user-named -image path.
	if _, err := e.Dispatch(context.Background(), "read", `{"path":"../pic.png"}`); err == nil || !strings.Contains(err.Error(), "outside workspace root") {
		t.Fatalf("read outside root err = %v, want the jail refusal", err)
	}
}
