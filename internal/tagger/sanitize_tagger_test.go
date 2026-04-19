//go:build tagger

package tagger

import "testing"

// TestSanitizeLabel_PreservesColon locks in the colon-in-label contract:
// the allowlist was widened to keep `:` so labels like `:3`, `nier:automata`,
// and WD14's `rating:general` pseudo-tag round-trip into the tags table as
// written. Stripping `:` would make the wd14RatingTags lookup miss and
// would collapse `:3` to `3`, silently merging it with an unrelated tag.
func TestSanitizeLabel_PreservesColon(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{":3", ":3"},
		{"nier:automata", "nier:automata"},
		{"rating:general", "rating:general"},
	}
	for _, c := range cases {
		if got := sanitizeLabel(c.in, 0); got != c.want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSanitizeLabel_PlaceholderForAllPunct keeps the existing
// invariant that a label made of only disallowed runes (or of
// punctuation with no alphanumerics) is replaced by an
// `_unsupported_<idx>` placeholder so slice indices stay aligned
// with the model's output channels.
func TestSanitizeLabel_PlaceholderForAllPunct(t *testing.T) {
	if got := sanitizeLabel(":::", 7); got != "_unsupported_7" {
		t.Errorf("sanitizeLabel(%q, 7) = %q, want _unsupported_7", ":::", got)
	}
}
