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
		got, ok := sanitizeLabel(c.in, 0)
		if got != c.want || !ok {
			t.Errorf("sanitizeLabel(%q) = (%q, %v), want (%q, true)", c.in, got, ok, c.want)
		}
	}
}

// TestSanitizeLabel_PlaceholderForAllPunct keeps the existing
// invariant that a label made of only disallowed runes (or of
// punctuation with no alphanumerics) is replaced by an
// `_unsupported_<idx>` placeholder so slice indices stay aligned
// with the model's output channels. The `ok` return is false so the
// caller can flag the slot as a placeholder and skip it at emission.
func TestSanitizeLabel_PlaceholderForAllPunct(t *testing.T) {
	got, ok := sanitizeLabel(":::", 7)
	if got != "_unsupported_7" || ok {
		t.Errorf("sanitizeLabel(%q, 7) = (%q, %v), want (_unsupported_7, false)", ":::", got, ok)
	}
}

// TestSanitizeLabel_AllowsEmoticonLabels keeps the auto-tagger label
// pipeline in sync with ValidateTagName: emoticon-class characters
// (?, <, >, =, ^) count as content so labels like "??", ">_<", "^_^",
// "<3", "=3" survive instead of dropping to the placeholder slot.
func TestSanitizeLabel_AllowsEmoticonLabels(t *testing.T) {
	cases := []string{"??", ">_<", "^_^", "<3", "=3", "=w=", "nani?"}
	for _, in := range cases {
		got, ok := sanitizeLabel(in, 0)
		if got != in || !ok {
			t.Errorf("sanitizeLabel(%q) = (%q, %v), want (%q, true)", in, got, ok, in)
		}
	}
}
