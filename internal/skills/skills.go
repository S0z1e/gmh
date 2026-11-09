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

// Load returns the SKILL.md body plus, when the skill has a references/
// directory, a footer listing its files. Reference files themselves are
// NOT loaded here — they load on demand via LoadReference, so a big
// skill stays cheap until its details are actually needed.
func (s Skill) Load() (string, error) {
	body, err := s.Body()
	if err != nil {
		return "", err
	}
	refs, err := s.References()
	if err != nil || len(refs) == 0 {
		return body, nil
	}
	var b strings.Builder
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\nReferences (load one with load_skill {name, reference}):\n")
	for _, r := range refs {
		fmt.Fprintf(&b, "- %s (%d bytes)\n", r.Name, r.Size)
	}
	return b.String(), nil
}

// Reference is one file under a skill's references/ directory.
type Reference struct {
	Name string
	Size int64
}

// References lists the files in <skill dir>/references/, sorted by name.
// An absent directory is not an error: it just means no references.
func (s Skill) References() ([]Reference, error) {
	dir := filepath.Join(filepath.Dir(s.Path), "references")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Reference
	for _, en := range entries {
		if en.IsDir() {
			continue
		}
		info, err := en.Info()
		if err != nil {
			continue
		}
		out = append(out, Reference{Name: en.Name(), Size: info.Size()})
	}
	return out, nil
}

// LoadReference reads one file from the skill's references/ directory.
// The path is jailed to that directory: absolute paths and ".." escapes
// are refused.
func (s Skill) LoadReference(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("load_skill needs a reference file name")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("reference must be a relative path: %s", rel)
	}
	base := filepath.Join(filepath.Dir(s.Path), "references")
	clean := filepath.Join(base, filepath.Clean(rel))
	if rel2, err := filepath.Rel(base, clean); err != nil || rel2 == ".." || strings.HasPrefix(rel2, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("reference outside references/: %s", rel)
	}
	data, err := os.ReadFile(clean)
	if err != nil {
		return "", fmt.Errorf("cannot read reference %q: %w", rel, err)
	}
	return string(data), nil
}

// Listing renders the compact listing injected into the system prompt.
func Listing(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nAvailable skills (call load_skill with the name to read the full instructions, or the user can /skill:<name>):\n")
	for _, sk := range skills {
		fmt.Fprintf(&b, "- %s: %s\n", sk.Name, sk.Description)
	}
	return b.String()
}

// MatchKeywords returns the skills whose name appears in text as a whole
// word (case-insensitive). "demo" matches "use the demo skill" but not
// "demonstrate"; "code-review" does not match "code-reviews".
func MatchKeywords(text string, skills []Skill) []Skill {
	lower := strings.ToLower(text)
	var out []Skill
	for _, sk := range skills {
		name := strings.ToLower(sk.Name)
		if name == "" {
			continue
		}
		for i := 0; i+len(name) <= len(lower); {
			j := strings.Index(lower[i:], name)
			if j < 0 {
				break
			}
			start := i + j
			end := start + len(name)
			beforeOK := start == 0 || !isWordChar(lower[start-1])
			afterOK := end == len(lower) || !isWordChar(lower[end])
			if beforeOK && afterOK {
				out = append(out, sk)
				break
			}
			i = start + 1
		}
	}
	return out
}

func isWordChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}
