package textutil

import (
	"testing"
	"unicode/utf8"
)

func TestCutsRespectUTF8AndByteBudgets(t *testing.T) {
	for _, s := range []string{"abc", "中🙂文abc🙂尾"} {
		for n := -1; n <= len(s)+1; n++ {
			for _, got := range []string{Prefix(s, n), Suffix(s, n)} {
				if !utf8.ValidString(got) || len(got) > max(n, 0) {
					t.Fatalf("n=%d cut=%q", n, got)
				}
			}
		}
	}
}
