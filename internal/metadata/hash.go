package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// genHashLen is the number of hex characters kept from the SHA-256 of the
// canonical parameter string. 12 hex chars = 48 bits, plenty for a personal
// library and short enough to paste into a search bar.
const genHashLen = 12

// computeGenerationHash returns a deterministic short hex digest derived from
// the generation parameters that identify a recipe. The seed is intentionally
// excluded so re-rolls of the same recipe collide.
//
// The canonical form is "key=value\n" pairs, one per line, with keys sorted
// implicitly by a fixed ordering. Empty inputs yield "" (not a hash of an
// empty string) so images without usable metadata don't all share a bucket.
func computeGenerationHash(prompt, negPrompt, model, sampler string, steps *int, cfg *float64) string {
	prompt = strings.TrimSpace(prompt)
	negPrompt = strings.TrimSpace(negPrompt)
	model = strings.TrimSpace(model)
	sampler = strings.TrimSpace(sampler)

	if prompt == "" && negPrompt == "" && model == "" && sampler == "" && steps == nil && cfg == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("prompt=")
	b.WriteString(prompt)
	b.WriteByte('\n')
	b.WriteString("negative=")
	b.WriteString(negPrompt)
	b.WriteByte('\n')
	b.WriteString("model=")
	b.WriteString(model)
	b.WriteByte('\n')
	b.WriteString("sampler=")
	b.WriteString(sampler)
	b.WriteByte('\n')
	b.WriteString("steps=")
	if steps != nil {
		fmt.Fprintf(&b, "%d", *steps)
	}
	b.WriteByte('\n')
	b.WriteString("cfg=")
	if cfg != nil {
		// Two-decimal rounding absorbs float noise ("7" vs "7.0" vs "7.00").
		fmt.Fprintf(&b, "%.2f", *cfg)
	}
	b.WriteByte('\n')

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:genHashLen]
}
