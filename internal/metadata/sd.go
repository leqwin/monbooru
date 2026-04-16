package metadata

import (
	"os"
	"strconv"
	"strings"

	"github.com/rwcarlsen/goexif/exif"
	"github.com/leqwin/monbooru/internal/models"
)

// extractSDFromJPEG reads A1111 metadata from JPEG EXIF UserComment.
func extractSDFromJPEG(path string) (*models.SDMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil // silently skip
	}
	defer f.Close()

	x, err := exif.Decode(f)
	if err != nil {
		return nil, nil
	}

	tag, err := x.Get(exif.UserComment)
	if err != nil {
		return nil, nil
	}

	// UserComment may be prefixed with charset identifier (e.g. "ASCII\x00\x00\x00")
	raw, err := tag.StringVal()
	if err != nil {
		return nil, nil
	}

	text := strings.TrimPrefix(raw, "ASCII\x00\x00\x00")
	text = strings.TrimLeft(text, "\x00")

	sd := parseA1111Parameters(text)
	return sd, nil
}

// parseA1111Parameters parses the A1111 parameter string format.
func parseA1111Parameters(text string) *models.SDMetadata {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	// Require at least one A1111 marker (Negative prompt: or a Steps: line).
	// Third-party writers (GIMP, Paint Tool SAI, LEAD Technologies, …) drop
	// short identifiers into the same EXIF UserComment slot, so a non-empty
	// blob alone is not enough evidence that the image came from A1111.
	negIdx := strings.Index(text, "\nNegative prompt:")
	paramIdx := findParamLineIndex(text)
	if negIdx < 0 && paramIdx < 0 {
		return nil
	}

	sd := &models.SDMetadata{}
	var rawParams string
	if negIdx >= 0 {
		sd.Prompt = strings.TrimSpace(text[:negIdx])
		afterNeg := text[negIdx+len("\nNegative prompt:"):]
		if paramIdx > negIdx {
			// negative prompt runs until param line
			relIdx := paramIdx - negIdx - len("\nNegative prompt:")
			if relIdx > 0 && relIdx <= len(afterNeg) {
				sd.NegativePrompt = strings.TrimSpace(afterNeg[:relIdx])
			}
			rawParams = strings.TrimSpace(text[paramIdx:])
			parseA1111Params(rawParams, sd)
		} else {
			sd.NegativePrompt = strings.TrimSpace(afterNeg)
		}
	} else {
		sd.Prompt = strings.TrimSpace(text[:paramIdx])
		rawParams = strings.TrimSpace(text[paramIdx:])
		parseA1111Params(rawParams, sd)
	}

	// Store the raw params line and parse all key-value pairs for display
	sd.RawParams = rawParams
	if rawParams != "" {
		sd.ParsedParams = parseAllA1111Params(rawParams)
	}
	sd.GenerationHash = computeGenerationHash(
		sd.Prompt, sd.NegativePrompt, sd.Model, sd.Sampler, sd.Steps, sd.CFGScale,
	)
	return sd
}

// findParamLineIndex finds the index of the first line starting with "Steps:" or known params.
func findParamLineIndex(text string) int {
	lines := strings.Split(text, "\n")
	pos := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Steps:") {
			return pos
		}
		pos += len(line) + 1 // +1 for newline
	}
	return -1
}

// ParseAllSDParams is the exported version for use in web handlers.
func ParseAllSDParams(rawParams string) []models.SDParam {
	return parseAllA1111Params(rawParams)
}

// parseAllA1111Params extracts all key-value pairs from the A1111 parameter line,
// preserving order. Values with braces are kept intact.
func parseAllA1111Params(paramLine string) []models.SDParam {
	paramLine = strings.ReplaceAll(paramLine, "\n", " ")
	var result []models.SDParam
	seen := map[string]bool{}

	// A1111 params are comma-separated "Key: Value" pairs.
	// Values can contain commas if they're inside braces, so we split carefully.
	parts := splitA1111Params(paramLine)
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		if key == "" {
			continue
		}
		if !seen[key] {
			seen[key] = true
			result = append(result, models.SDParam{Key: key, Val: val})
		}
	}
	return result
}

// splitA1111Params splits A1111 parameter text by commas, respecting brace nesting.
func splitA1111Params(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i, ch := range s {
		switch ch {
		case '{', '(', '[':
			depth++
		case '}', ')', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	if start < len(s) {
		parts = append(parts, s[start:])
	}
	return parts
}

func parseA1111Params(paramLine string, sd *models.SDMetadata) {
	// paramLine may span multiple lines if wrapped; join them
	paramLine = strings.ReplaceAll(paramLine, "\n", " ")

	// Use splitA1111Params for consistency with parseAllA1111Params,
	// so values containing commas inside braces are kept intact.
	parts := splitA1111Params(paramLine)
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])

		switch key {
		case "Steps":
			if n, err := strconv.Atoi(val); err == nil {
				sd.Steps = &n
			}
		case "Sampler":
			sd.Sampler = val
		case "CFG scale":
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				sd.CFGScale = &f
			}
		case "Seed":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				sd.Seed = &n
			}
		case "Model":
			if sd.Model == "" {
				sd.Model = val
			} else {
				sd.Model += ", " + val
			}
		case "Lora hashes":
			// Append LoRA info to model string so it's visible in the UI
			loraVal := strings.TrimSpace(val)
			if loraVal != "" && loraVal != "{}" {
				if sd.Model != "" {
					sd.Model += " | LoRA: " + loraVal
				} else {
					sd.Model = "LoRA: " + loraVal
				}
			}
		}
	}
}
