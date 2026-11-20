package llm

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
)

func TestUsageValidationRetainsInvalidWireEvidence(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"negative cache", `{"prompt_tokens":10,"completion_tokens":5,"cached_tokens":-1}`},
		{"negative reasoning", `{"prompt_tokens":10,"completion_tokens":5,"reasoning_tokens":-1}`},
		{"sum overflow", fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":1}`, math.MaxInt)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var u Usage
			if err := json.Unmarshal([]byte(tc.raw), &u); err != nil {
				t.Fatal(err)
			}
			if u.Valid() {
				t.Fatalf("normalization erased invalid evidence: %+v", u)
			}
		})
	}
	for _, raw := range []string{
		`{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":-1}}`,
		`{"input_tokens":10,"output_tokens":5,"output_tokens_details":{"reasoning_tokens":-1}}`,
		fmt.Sprintf(`{"input_tokens":%d,"output_tokens":1}`, math.MaxInt),
	} {
		var u responsesUsage
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			t.Fatal(err)
		}
		if u.sharedUsage().Valid() {
			t.Fatalf("responses normalization accepted invalid usage: %s", raw)
		}
	}
}
