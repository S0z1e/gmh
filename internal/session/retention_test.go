package session

import (
	"context"
	"encoding/base64"
	"minimalharness/internal/checkpoint"
	"minimalharness/internal/llm"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArtifactRetentionPreservesArchiveReferences(t *testing.T) {
	setTestHome(t, t.TempDir())
	workspace := t.TempDir()
	s, err := Open("images", OpenOptions{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := llm.Message{Role: "user", Images: []llm.Image{{Data: base64.StdEncoding.EncodeToString([]byte("original")), MediaType: "image/png"}}}
	if err := s.Append(m); err != nil {
		t.Fatal(err)
	}
	msgs, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	digest := msgs[0].Images[0].Digest
	path := filepath.Join(imageBlobDir(), digest)
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ArchiveCopy("custom-image-archive"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rewrite(nil); err != nil {
		t.Fatal(err)
	}
	report, err := PruneArtifacts(context.Background(), now)
	if err != nil || report.DeletedBlobs != 0 {
		t.Fatalf("deleted archived image: %+v %v", report, err)
	}
	if err := s.Rename("renamed-images"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := DeleteSession("renamed-images"); err != nil {
		t.Fatal(err)
	}
	report, err = PruneArtifacts(context.Background(), now)
	if err != nil || report.DeletedBlobs != 1 || report.ReclaimedBytes != 8 {
		t.Fatalf("orphan not reclaimed: %+v %v", report, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("old orphan left: %v", err)
	}
}

func TestArtifactRetentionGracePeriodAndUncertainScan(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, err := New("safe")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	if err := os.MkdirAll(imageBlobDir(), 0700); err != nil {
		t.Fatal(err)
	}
	recent := filepath.Join(imageBlobDir(), strings.Repeat("a", 64))
	aged := filepath.Join(imageBlobDir(), strings.Repeat("b", 64))
	for _, p := range []string{recent, aged} {
		if err := os.WriteFile(p, []byte("blob"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(aged, old, old); err != nil {
		t.Fatal(err)
	}
	backup := s.Path + "-torn-ABCDEF2.bak"
	if err := os.WriteFile(backup, []byte(`{"images":[{"digest":"`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneArtifacts(context.Background(), now); err == nil {
		t.Fatal("pruned with uncertain recovery fragment")
	}
	if _, err := os.Stat(aged); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PruneArtifacts(ctx, now); err != context.Canceled {
		t.Fatalf("cancel ignored: %v", err)
	}
	report, err := PruneArtifacts(context.Background(), now)
	if err != nil || report.DeletedBlobs != 1 {
		t.Fatalf("prune=%+v %v", report, err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatal("recent unpublished blob removed", err)
	}
}

func TestArtifactRetentionCheckpointAndConcurrentPublication(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, err := New("checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := llm.Message{Role: "user", Images: []llm.Image{{Data: base64.StdEncoding.EncodeToString([]byte("checkpoint")), MediaType: "image/png"}}}
	if err := s.MarkCompactMessages([]llm.Message{m}); err != nil {
		t.Fatal(err)
	}
	msgs, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	img := msgs[0].Images[0]
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	path := filepath.Join(imageBlobDir(), img.Digest)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		done := make(chan error, 1)
		go func() { done <- s.Append(llm.Message{Role: "user", Images: []llm.Image{img}}) }()
		report, err := PruneArtifacts(context.Background(), now)
		if err != nil || report.DeletedBlobs != 0 {
			t.Fatalf("concurrent reference lost: %+v %v", report, err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactRetentionPreservesCheckpointOnlyImageReference(t *testing.T) {
	setTestHome(t, t.TempDir())
	s, err := New("checkpoint-only")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	image := llm.Image{Data: base64.StdEncoding.EncodeToString([]byte("checkpoint image")), MediaType: "image/png"}
	if err := s.Append(llm.Message{Role: "user", Images: []llm.Image{image}}); err != nil {
		t.Fatal(err)
	}
	messages, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	digest := messages[0].Images[0].Digest
	state, err := checkpoint.Merge(checkpoint.State{}, checkpoint.Delta{Artifacts: []checkpoint.Artifact{{
		Kind: "image", Path: "example.png", Source: "test", SHA256: digest, MediaType: "image/png",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	facts, err := checkpoint.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Rewrite(nil); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkCompactMessages([]llm.Message{{Role: "user", Kind: "checkpoint", Content: string(facts)}}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	blob := filepath.Join(imageBlobDir(), digest)
	if err := os.Chtimes(blob, old, old); err != nil {
		t.Fatal(err)
	}
	report, err := PruneArtifacts(context.Background(), now)
	if err != nil || report.DeletedBlobs != 0 {
		t.Fatalf("checkpoint reference was lost: %+v %v", report, err)
	}
	if _, err := os.Stat(blob); err != nil {
		t.Fatal(err)
	}
}
