package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDenylistDestinationsAndWrappers(t *testing.T) {
	for _, cmd := range []string{"ls ~/.ssh 2>/dev/null", "cat ~/.aws/credentials > local.txt", "echo 'x > ~/.ssh/config'"} {
		if why := DeniedCommand(cmd); why != "" {
			t.Fatalf("%q blocked: %s", cmd, why)
		}
	}
	for _, cmd := range []string{`echo hi >& ~/.ssh/config`, `echo hi &>~/.ssh/config`, `echo hi &>> ~/.ssh/config`, `echo hi >>& ~/.ssh/config`, `cp 'a&b' ~/.ssh/config`, `bash -c 'echo hi > ~/.ssh/config'`, "echo \"$(echo hi > ~/.ssh/config)\"", `cat <(tee ~/.ssh/config) >/dev/null`, `echo $(tee ~/.ssh/config; true) >/dev/null`, `cat >(tee ~/.ssh/config) >/dev/null`, `echo "$(true; rm -rf /; true)"`, `echo "$(true; mkfs.ext4 /dev/sda; true)"`, `bash -c 'true; dd of=/dev/sda; true'`} {
		if DeniedCommand(cmd) == "" {
			t.Fatalf("%q allowed", cmd)
		}
	}
	for _, cmd := range []string{"cp key ~/.ssh/config < /dev/null", "cp key ~/.ssh/config </dev/null", "cp key ~/.ssh/config 0</dev/null", "sudo rm -rf /", "sudo -u root rm -rf /", "rm -rf ~/", "rm -rf ${HOME}/", "/bin/rm -rf /", "echo hi >~/.ssh/config", "sudo cp x ~/.aws/credentials", "cp x ~/.ssh/config 2>/dev/null", `cp --target-directory="$HOME/.ssh" key`, `cp key "$HOME/.ssh/config" -v`, `echo hi >| ~/.ssh/config`, "cp key ~/.ssh/config --suffix .bak", "mv key ~/.ssh/config -S .bak", "cp key -t~/.ssh -S.bak", "install -d ~/.ssh/new local-dir", "install --directory ~/.aws/new local-dir", "install -m 600 key ~/.ssh/config -S .bak"} {
		if DeniedCommand(cmd) == "" {
			t.Fatalf("%q allowed", cmd)
		}
	}
	for _, option := range []string{"--log-file", "--backup-dir", "--temp-dir", "--partial-dir", "--write-batch", "--only-write-batch", "-T"} {
		for _, sep := range []string{" ", "="} {
			if option == "-T" && sep == "=" {
				sep = ""
			}
			cmd := "rsync src dst " + option + sep + "~/.ssh/config"
			if DeniedCommand(cmd) == "" {
				t.Fatalf("%q allowed", cmd)
			}
		}
	}
	for _, cmd := range []string{`bash -c 'true; cp key ~/.ssh/config; true'`, `echo "$(true; touch ~/.ssh/config; true)"`, `cat ~/.ssh/config; cp key local-dir`, `cat ~/.ssh/config; true; mkdir local-dir`} {
		if DeniedCommand(cmd) == "" {
			t.Fatalf("%q lost conservative credential protection", cmd)
		}
	}
}

func TestReadRejectsBinaryWithoutLosingUTF8(t *testing.T) {
	root := t.TempDir()
	e := &Executor{Root: root}
	for name, data := range map[string][]byte{"nul.bin": {0, 0, 0}, "invalid.bin": {0xff, 0xfe, 0xff}, "control.bin": {1, 2, 3}} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Read(ReadArgs{Path: name}); err == nil {
			t.Fatalf("%s read as text", name)
		}
	}
	text := strings.Repeat("中", MaxReadBytes/3+2)
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := e.Read(ReadArgs{Path: "long.txt"})
	if err != nil || !utf8.ValidString(got) {
		t.Fatalf("text cap: %v valid=%v", err, utf8.ValidString(got))
	}
	e.OutputDir = t.TempDir()
	_, err = e.Bash(context.Background(), BashArgs{Command: "printf secret"})
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(e.OutputDir)
	for _, entry := range entries {
		info, _ := entry.Info()
		if info.Mode().Perm() != 0600 {
			t.Fatal("bash output is not private")
		}
	}
}
