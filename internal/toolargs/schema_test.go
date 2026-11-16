package toolargs

import (
	"encoding/json"
	"testing"
)

func TestSchemaAssertions(t *testing.T) {
	for _, tc := range []struct{ schema, good, bad string }{
		{`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}`, `{"x":""}`, `{"x":null}`},
		{`{"type":"integer","minimum":1,"maximum":5}`, `3`, `0`},
		{`{"type":"number","exclusiveMinimum":1,"exclusiveMaximum":5}`, `3.5`, `5`},
		{`{"type":"string","minLength":2,"maxLength":3,"pattern":"^世"}`, `"世界"`, `"abc"`},
		{`{"type":"array","items":{"type":"boolean"},"minItems":1,"maxItems":2}`, `[true]`, `[null]`},
		{`{"type":["string","null"],"enum":["yes",null]}`, `null`, `"no"`},
		{`{"const":{"x":1}}`, `{"x":1}`, `{"x":2}`},
		{`{"allOf":[{"type":"number"},{"minimum":1}]}`, `2`, `0`},
		{`{"anyOf":[{"const":"a"},{"const":"b"}]}`, `"a"`, `"c"`},
		{`{"oneOf":[{"type":"number"},{"type":"integer"}]}`, `2.5`, `2`},
		{`{"not":{"type":"null"}}`, `false`, `null`},
		{`{"additionalProperties":{"type":"number"}}`, `{"x":1}`, `{"x":"1"}`},
		{`true`, `{}`, `{broken`},
		{`{"type":"integer"}`, `9007199254740993`, `1.0000000000000001`},
		{`{"enum":[9007199254740993]}`, `9007199254740993`, `9007199254740992`},
		{`{"const":{"x":1}}`, `{"x":1.0}`, `{"x":1.0000000000000001}`},
		{`{"minimum":9007199254740993}`, `9007199254740993`, `9007199254740992`},
	} {
		t.Run(tc.schema, func(t *testing.T) {
			if err := Validate(json.RawMessage(tc.schema), tc.good); err != nil {
				t.Fatalf("valid args: %v", err)
			}
			if err := Validate(json.RawMessage(tc.schema), tc.bad); err == nil {
				t.Fatal("invalid args accepted")
			}
		})
	}
}

func TestUnsupportedOrInvalidSchemaFailsClosed(t *testing.T) {
	for _, schema := range []string{`{"$ref":"https://example.com/schema"}`, `{"format":"date"}`, `{"type":"unknown"}`, `{"minimum":null}`, `{"required":[false]}`, `{"items":3}`, `{"pattern":"["}`, `{"minItems":-1}`, `{"oneOf":[]}`, `[]`, `false`} {
		if err := Validate(json.RawMessage(schema), `{}`); err == nil {
			t.Fatalf("schema accepted: %s", schema)
		}
	}
}

func TestNumericExponentExpansionIsBounded(t *testing.T) {
	for _, args := range []string{`1e-1000000000`, `1e1000000000`} {
		if err := Validate(json.RawMessage(`{"type":"number"}`), args); err == nil {
			t.Fatalf("unrepresentable number accepted: %s", args)
		}
	}
	if err := Validate(json.RawMessage(`{"const":0}`), `0e1000000000`); err != nil {
		t.Fatalf("zero exponent should not expand: %v", err)
	}
}
