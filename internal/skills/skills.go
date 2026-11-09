// Package skills discovers Agent Skills (SKILL.md) and loads only
// name + description at startup. The full file loads on demand.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Skill struct {
	Name        string
	Description string
	Path        string
}

// parseFrontmatter extracts name/description from a YAML frontmatter block.
// Format: ---\nname: x\ndescription: y\n---\n<body>
func parseFrontmatter(path string) (Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}
	s := Skill{Path: path, Name: filepath.Base(filepath.Dir(path))}
	text := string(data)
	if !strings.HasPrefix(text, "---") {
		return s, nil
	}
	end := strings.Index(text[3:], "---")
	if end < 0 {
		return s, nil
	}
	front := text[3 : 3+end]
	for _, line := range strings.Split(front, "\n") {
		kv := strings.SplitN(line, ":", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.TrimSpace(kv[1])
		v = strings.Trim(v, `"'`)
		if k == "name" {
			s.Name = v
		}
		if k == "description" {
			s.Description = v
		}
	}
	return s, nil
}

// Discover scans root/.agents/skills/*/SKILL.md and, if present,
// ~/.config/minimal-harness/skills/*/SKILL.md. Project skills win on name clash.
func Discover(root string) []Skill {
	var out []Skill
	seen := map[string]bool{}
	add := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, en := range entries {
			if !en.IsDir() {
				continue
			}
			p := filepath.Join(dir, en.Name(), "SKILL.md")
			sk, err := parseFrontmatter(p)
			if err != nil {
				continue
			}
			if seen[sk.Name] {
				continue
			}
			seen[sk.Name] = true
			out = append(out, sk)
		}
	}
	add(filepath.Join(root, ".agents", "skills"))
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".config", "minimal-harness", "skills"))
	}
	return out
}

func (s Skill) Body() (string, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Listing renders the compact listing injected into the system prompt.
func Listing(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nAvailable skills (use /skill:<name> to load the full text, or call the model with the skill name):\n")
	for _, sk := range skills {
		fmt.Fprintf(&b, "- %s: %s\n", sk.Name, sk.Description)
	}
	return b.String()
}
