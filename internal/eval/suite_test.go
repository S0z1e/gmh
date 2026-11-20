package eval

import (
	"strings"
	"testing"
)

func TestParseRequiresActualPassingTest(t *testing.T) {
	good := "{\"Action\":\"run\",\"Test\":\"TestFixed\"}\n{\"Action\":\"pass\",\"Test\":\"TestFixed\",\"Elapsed\":1.25}\n{\"Action\":\"pass\"}\n"
	if elapsed, err := parse(strings.NewReader(good), "TestFixed"); err != nil || elapsed != 1.25 {
		t.Fatalf("%g %v", elapsed, err)
	}
	for _, bad := range []string{"", `{"Action":"pass"}`, strings.ReplaceAll(good, "TestFixed", "TestOther"), strings.Replace(good, `"pass","Test"`, `"skip","Test"`, 1), strings.TrimSuffix(good, "{\"Action\":\"pass\"}\n"), good + "not-json\n", good + "{\"Action\":\"fail\"}\n"} {
		if _, err := parse(strings.NewReader(bad), "TestFixed"); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
func TestScenarioCatalogUnique(t *testing.T) {
	ids := map[string]bool{}
	for _, s := range Scenarios {
		if ids[s.ID] || s.Test == "" || s.Oracle == "" {
			t.Fatalf("invalid scenario %+v", s)
		}
		ids[s.ID] = true
	}
	if len(ids) != 10 {
		t.Fatalf("scenarios=%d", len(ids))
	}
}
