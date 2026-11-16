// Package toolargs validates the JSON-schema subset used by harness tools.
// Unsupported assertions are errors, never silently unenforced constraints.
package toolargs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"strconv"
	"unicode/utf8"
)

// Schema checks the supported subset: types, object properties/required,
// additionalProperties, array items and bounds, string bounds/pattern, numeric
// bounds, enum/const, and allOf/anyOf/oneOf/not. Local/remote references and other
// assertions are deliberately rejected. Metadata keywords have no assertions.
func Schema(raw json.RawMessage) error {
	var schema any
	if err := decodeJSON(raw, &schema); err != nil {
		return fmt.Errorf("invalid schema: %w", err)
	}
	return checkSchema(schema, 0)
}

func Validate(raw json.RawMessage, args string) error {
	if err := Schema(raw); err != nil {
		return err
	}
	var schema, value any
	_ = decodeJSON(raw, &schema)
	if err := decodeJSON([]byte(args), &value); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return validate(schema, value, "arguments", 0)
}

func checkSchema(value any, depth int) error {
	if depth > 64 {
		return fmt.Errorf("schema nesting exceeds 64")
	}
	if _, ok := value.(bool); ok {
		return nil
	}
	s, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("schema must be an object or boolean")
	}
	for key, v := range s {
		switch key {
		case "$schema", "$id", "$comment", "title", "description", "default", "examples", "deprecated", "readOnly", "writeOnly":
		case "type":
			var types []any
			if name, ok := v.(string); ok {
				types = []any{name}
			} else {
				types, ok = v.([]any)
				if !ok || len(types) == 0 {
					return fmt.Errorf("schema type must be a name or nonempty array")
				}
			}
			for _, name := range types {
				switch name {
				case "object", "array", "string", "integer", "number", "boolean", "null":
				default:
					return fmt.Errorf("unsupported schema type %v", name)
				}
			}
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("schema properties must be an object")
			}
			for _, child := range props {
				if err := checkSchema(child, depth+1); err != nil {
					return err
				}
			}
		case "items", "additionalProperties", "not":
			if err := checkSchema(v, depth+1); err != nil {
				return err
			}
		case "required":
			names, ok := v.([]any)
			if !ok {
				return fmt.Errorf("schema required must be an array")
			}
			for _, name := range names {
				if _, ok := name.(string); !ok {
					return fmt.Errorf("schema required entries must be strings")
				}
			}
		case "allOf", "anyOf", "oneOf":
			children, ok := v.([]any)
			if !ok || len(children) == 0 {
				return fmt.Errorf("schema %s must be a nonempty array", key)
			}
			for _, child := range children {
				if err := checkSchema(child, depth+1); err != nil {
					return err
				}
			}
		case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum":
			if _, ok := number(v); !ok {
				return fmt.Errorf("schema %s must be a number", key)
			}
		case "minItems", "maxItems", "minLength", "maxLength":
			n, ok := number(v)
			if !ok || n.Sign() < 0 || !n.IsInt() {
				return fmt.Errorf("schema %s must be a nonnegative integer", key)
			}
		case "pattern":
			pattern, ok := v.(string)
			if !ok {
				return fmt.Errorf("schema pattern must be a string")
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("invalid schema pattern: %w", err)
			}
		case "enum":
			options, ok := v.([]any)
			if !ok || len(options) == 0 {
				return fmt.Errorf("schema enum must be a nonempty array")
			}
		case "const":
		default:
			return fmt.Errorf("unsupported schema keyword %q", key)
		}
	}
	return nil
}

func validate(schema, value any, path string, depth int) error {
	if depth > 64 {
		return fmt.Errorf("%s exceeds schema nesting limit", path)
	}
	if accept, ok := schema.(bool); ok {
		if !accept {
			return fmt.Errorf("%s is forbidden", path)
		}
		return nil
	}
	s := schema.(map[string]any)
	if kind, ok := s["type"]; ok {
		matches := func(kind any) bool {
			switch kind {
			case "object":
				_, ok := value.(map[string]any)
				return ok
			case "array":
				_, ok := value.([]any)
				return ok
			case "string":
				_, ok := value.(string)
				return ok
			case "number":
				_, ok := value.(json.Number)
				return ok
			case "integer":
				n, ok := number(value)
				return ok && n.IsInt()
			case "boolean":
				_, ok := value.(bool)
				return ok
			case "null":
				return value == nil
			}
			return false
		}
		matched := matches(kind)
		if kinds, ok := kind.([]any); ok {
			for _, k := range kinds {
				matched = matched || matches(k)
			}
		}
		if !matched {
			return fmt.Errorf("%s must have type %v", path, kind)
		}
	}
	if options, ok := s["enum"].([]any); ok {
		matched := false
		for _, option := range options {
			matched = matched || equalJSON(option, value)
		}
		if !matched {
			return fmt.Errorf("%s must match an enum value", path)
		}
	}
	if constant, ok := s["const"]; ok && !equalJSON(constant, value) {
		return fmt.Errorf("%s must match its constant", path)
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if children, ok := s[key].([]any); ok {
			matched := 0
			for _, child := range children {
				if validate(child, value, path, depth+1) == nil {
					matched++
				}
			}
			if (key == "allOf" && matched != len(children)) || (key == "anyOf" && matched == 0) || (key == "oneOf" && matched != 1) {
				return fmt.Errorf("%s fails %s", path, key)
			}
		}
	}
	if child, ok := s["not"]; ok && validate(child, value, path, depth+1) == nil {
		return fmt.Errorf("%s matches a forbidden schema", path)
	}
	switch v := value.(type) {
	case map[string]any:
		if required, ok := s["required"].([]any); ok {
			for _, name := range required {
				if _, ok := v[name.(string)]; !ok {
					return fmt.Errorf("%s.%s is required", path, name)
				}
			}
		}
		props, _ := s["properties"].(map[string]any)
		for name, item := range v {
			child, known := props[name]
			if !known {
				var restricted bool
				child, restricted = s["additionalProperties"]
				if !restricted {
					continue
				}
			}
			if err := validate(child, item, path+"."+name, depth+1); err != nil {
				return err
			}
		}
	case []any:
		if err := bounds(s, "Items", len(v), path); err != nil {
			return err
		}
		if child, ok := s["items"]; ok {
			for i, item := range v {
				if err := validate(child, item, fmt.Sprintf("%s[%d]", path, i), depth+1); err != nil {
					return err
				}
			}
		}
	case string:
		if err := bounds(s, "Length", utf8.RuneCountInString(v), path); err != nil {
			return err
		}
		if pattern, ok := s["pattern"].(string); ok {
			re, _ := regexp.Compile(pattern)
			if !re.MatchString(v) {
				return fmt.Errorf("%s does not match pattern", path)
			}
		}
	case json.Number:
		n, _ := number(v)
		for _, key := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum"} {
			if limit, ok := number(s[key]); ok && ((key == "minimum" && n.Cmp(limit) < 0) || (key == "maximum" && n.Cmp(limit) > 0) || (key == "exclusiveMinimum" && n.Cmp(limit) <= 0) || (key == "exclusiveMaximum" && n.Cmp(limit) >= 0)) {
				return fmt.Errorf("%s violates %s", path, key)
			}
		}
	}
	return nil
}

func bounds(schema map[string]any, suffix string, size int, path string) error {
	for _, prefix := range []string{"min", "max"} {
		if limit, ok := number(schema[prefix+suffix]); ok && ((prefix == "min" && big.NewRat(int64(size), 1).Cmp(limit) < 0) || (prefix == "max" && big.NewRat(int64(size), 1).Cmp(limit) > 0)) {
			return fmt.Errorf("%s violates %s%s", path, prefix, suffix)
		}
	}
	return nil
}

// Keep decimal precision during validation, including integers above 2^53.
// Reject numbers outside the finite float64 range used by plugin JavaScript;
// this also bounds exponent expansion when constructing exact rationals.
func number(value any) (*big.Rat, bool) {
	n, ok := value.(json.Number)
	if !ok {
		return nil, false
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil {
		return nil, false
	}
	if f == 0 {
		// ParseFloat allows underflow and enormous exponents on zero.
		// Handle zero without expanding the exponent; reject underflow
		// before constructing a potentially enormous denominator.
		for _, digit := range n.String() {
			if digit == 'e' || digit == 'E' {
				break
			}
			if digit >= '1' && digit <= '9' {
				return nil, false
			}
		}
		return new(big.Rat), true
	}
	r, ok := new(big.Rat).SetString(n.String())
	return r, ok
}

func decodeJSON(raw []byte, out *any) error {
	if !json.Valid(raw) {
		return fmt.Errorf("malformed JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var check func(any) error
	check = func(v any) error {
		switch x := v.(type) {
		case json.Number:
			if _, ok := number(x); !ok {
				return fmt.Errorf("number outside supported finite range")
			}
		case map[string]any:
			for _, child := range x {
				if err := check(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range x {
				if err := check(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return check(*out)
}

func equalJSON(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		left, ok := number(x)
		right, other := number(b)
		return ok && other && left.Cmp(right) == 0
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for key, value := range x {
			other, ok := y[key]
			if !ok || !equalJSON(value, other) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i, value := range x {
			if !equalJSON(value, y[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}
