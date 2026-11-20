package config

import "testing"

func TestBashEnvMergeAndValidation(t *testing.T) {
	f := &File{BashEnv: []string{"PATH", "HOME"}}
	Merge(f, &File{})
	if len(f.BashEnv) != 2 {
		t.Fatal("unset environment list replaced global list")
	}
	Merge(f, &File{BashEnv: []string{}})
	if f.BashEnv == nil || len(f.BashEnv) != 0 {
		t.Fatal("explicit empty list must disable inheritance")
	}
	for _, name := range []string{"", "KEY=value", "$KEY", "KEY\x00"} {
		if err := Validate(&File{BashEnv: []string{name}}); err == nil {
			t.Fatal("invalid environment name accepted")
		}
	}
	if err := Validate(&File{BashEnv: []string{"PATH", "GMH_VENDOR_KEY_1"}}); err != nil {
		t.Fatal(err)
	}
}
