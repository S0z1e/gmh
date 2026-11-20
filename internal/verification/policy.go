package verification

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Policy lists required checks for changed work. Commands run only when the
// model requests the matching normal Bash invocation, through existing approval.
type Policy struct {
	Required []Check  `json:"required"`
	Exclude  []string `json:"exclude,omitempty"`
}

func (p *Policy) Validate() error {
	if p == nil {
		return nil
	}
	if len(p.Required) > 32 || len(p.Exclude) > MaxExclusions {
		return fmt.Errorf("verification policy exceeds check/exclusion limits")
	}
	names := map[string]bool{}
	for _, c := range p.Required {
		if strings.TrimSpace(c.Name) == "" || len(c.Name) > 64 || strings.TrimSpace(c.Command) == "" || len(c.Command) > 16384 || len(c.Cwd) > 4096 || strings.ContainsRune(c.Command+c.Cwd, 0) {
			return fmt.Errorf("verification check requires a bounded name, command and cwd")
		}
		if names[c.Name] {
			return fmt.Errorf("duplicate verification check name %q", c.Name)
		}
		names[c.Name] = true
	}
	total := 0
	for _, path := range p.Exclude {
		clean := filepath.Clean(path)
		total += len(path)
		if path == "" || strings.ContainsRune(path, 0) || filepath.IsAbs(path) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || total > MaxPathBytes {
			return fmt.Errorf("invalid verification exclusion")
		}
	}
	return nil
}
