package session

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"minimalharness/internal/llm"
)

func TestImageBlobsSurviveEveryTranscriptWriter(t *testing.T) {
	for _, writer := range []string{"append", "checkpoint", "rewrite"} {
		t.Run(writer, func(t *testing.T) {
			setTestHome(t, t.TempDir())
			s, err := New("images")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			data := base64.StdEncoding.EncodeToString([]byte("original"))
			m := llm.Message{Role: "user", Images: []llm.Image{{Path: "origin.png", MediaType: "image/png", Data: data}, {Path: "alias.png", MediaType: "image/png", Data: data, Workspace: true}}}
			switch writer {
			case "append":
				err = s.Append(m)
			case "checkpoint":
				err = s.MarkCompactMessages([]llm.Message{m})
			case "rewrite":
				err = s.Rewrite([]llm.Message{m})
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(s.Path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), data) {
				t.Fatal("inline image bytes in transcript")
			}
			msgs, err := Load(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			img := msgs[0].Images[0]
			if !validImageDigest(img.Digest) || img.Bytes != 8 || img.Data != "" || msgs[0].Images[1].Digest != img.Digest || !msgs[0].Images[1].Workspace {
				t.Fatalf("lost reference: %+v", msgs)
			}
			files, err := os.ReadDir(imageBlobDir())
			if err != nil || len(files) != 1 {
				t.Fatalf("dedup: %v %v", files, err)
			}
			blob := filepath.Join(imageBlobDir(), img.Digest)
			if runtime.GOOS != "windows" {
				for _, path := range []string{imageBlobDir(), blob} {
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm()&0077 != 0 {
						t.Fatalf("private storage: %s %v %v", path, info, err)
					}
				}
			}
			HydrateImages(msgs)
			if msgs[0].Images[0].Data != data {
				t.Fatal("lost immutable image bytes")
			}
			// Rewriting hydrated messages preserves the same reference and blob.
			if err := s.Rewrite(msgs); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(blob); err != nil {
				t.Fatal(err)
			}
			msgs, err = Load(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			HydrateImages(msgs)
			if msgs[0].Images[0].Data != "" || msgs[0].Images[0].Unavailable == "" {
				t.Fatal("missing blob not reported")
			}
			if err := os.WriteFile(blob, []byte("corrupt"), 0600); err != nil {
				t.Fatal(err)
			}
			HydrateImages(msgs)
			if msgs[0].Images[0].Data != "" || msgs[0].Images[0].Unavailable == "" {
				t.Fatal("corrupt blob accepted")
			}
		})
	}
}

func TestImageBlobFailureCannotCommitReference(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, err := New("failed-image")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := os.MkdirAll(filepath.Dir(imageBlobDir()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imageBlobDir(), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	err = s.Append(llm.Message{Role: "user", Images: []llm.Image{{Data: "aGVsbG8="}}})
	if err == nil {
		t.Fatal("blob failure ignored")
	}
	msgs, err := Load(s.ID)
	if err != nil || len(msgs) != 0 {
		t.Fatalf("failed blob committed: %+v %v", msgs, err)
	}
}

func TestConcurrentImageBlobPublication(t *testing.T) {
	setTestHome(t, t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			img, err := persistImage(llm.Image{Data: "aGVsbG8="})
			if err != nil || !validImageDigest(img.Digest) {
				t.Errorf("publish: %+v %v", img, err)
			}
		}()
	}
	wg.Wait()
	files, err := os.ReadDir(imageBlobDir())
	if err != nil || len(files) != 1 {
		t.Fatalf("publication leftovers: %v %v", files, err)
	}
}

func TestImageBlobReferencesNeverFollowOriginOrEscapeStore(t *testing.T) {
	setTestHome(t, t.TempDir())
	origin := filepath.Join(t.TempDir(), "source.png")
	if err := os.WriteFile(origin, []byte("new contents"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, digest := range []string{"", "../source.png", strings.Repeat("A", 64), strings.Repeat("0", 64)} {
		msgs := []llm.Message{{Role: "user", Images: []llm.Image{{Path: origin, Digest: digest, MediaType: "image/png"}}}}
		HydrateImages(msgs)
		if msgs[0].Images[0].Data != "" || msgs[0].Images[0].Unavailable == "" {
			t.Fatalf("unsafe reference: %+v", msgs)
		}
	}
}
