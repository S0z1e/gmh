package app

import (
	"minimalharness/internal/llm"
	"minimalharness/internal/session"
	"os"
	"path/filepath"
	"testing"
)

func TestImageHistoryDoesNotReloadItsOrigin(t *testing.T) {
	for _, change := range []string{"overwrite", "delete", "move", "workspace", "model", "session"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("OPENAI_API_KEY", "test")
			root := t.TempDir()
			writeMockProvider(t, root)
			writeTestPNG(t, root, "pic.png")
			a, err := Setup(Params{WorkspaceTrust: "trust", Root: root, Mode: "auto"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { a.Store.Close() }()
			a.Cfg.Exec.ImageInput = true
			img, err := a.Cfg.Exec.LoadImage("pic.png")
			if err != nil {
				t.Fatal(err)
			}
			id := a.Store.ID
			if err := a.Store.Append(llm.Message{Role: "user", Images: []llm.Image{img}}); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "overwrite":
				err = os.WriteFile(filepath.Join(root, "pic.png"), []byte("replacement"), 0600)
			case "delete":
				err = os.Remove(filepath.Join(root, "pic.png"))
			case "move":
				err = os.Rename(filepath.Join(root, "pic.png"), filepath.Join(root, "moved.png"))
			case "workspace":
				a.Cfg.Exec.Root = t.TempDir()
			case "model":
				a.Cfg.Exec.ImageInput = false
			case "session":
				_, err = a.NewSession()
			}
			if err != nil {
				t.Fatal(err)
			}
			var history []llm.Message
			if change == "session" {
				history, err = a.LoadSession(id)
			} else {
				history, err = session.Load(id)
				a.hydrateImages(history)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(history) != 1 || len(history[0].Images) != 1 || history[0].Images[0].Data != img.Data {
				t.Fatalf("historical bytes changed after %s: %+v", change, history)
			}
		})
	}
}
