package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// genHashLen is how many hex chars (48 bits) we keep. Small enough to
// paste into a search bar, large enough for a personal library.
const genHashLen = 12

// computeGenerationHash returns a short hex digest identifying a
// generation recipe. Seed is intentionally excluded so re-rolls of the
// same recipe collide. Empty inputs return "" so images without
// metadata don't all collapse into one bucket.
//
// Canonical form: fixed-order "key=value\n" lines.
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
		// Two-decimal rounding absorbs float noise (7 vs 7.0 vs 7.00).
		fmt.Fprintf(&b, "%.2f", *cfg)
	}
	b.WriteByte('\n')

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:genHashLen]
}
