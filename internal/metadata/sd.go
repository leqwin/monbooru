package metadata

import (
	"os"
	"strconv"
	"strings"

	"github.com/rwcarlsen/goexif/exif"
	"github.com/leqwin/monbooru/internal/models"
)

// extractSDFromJPEG reads A1111 metadata from a JPEG's EXIF UserComment.
func extractSDFromJPEG(path string) (*models.SDMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
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

	raw, err := tag.StringVal()
	if err != nil {
		return nil, nil
	}

	// EXIF UserComment may carry a charset prefix like "ASCII\x00\x00\x00".
	text := strings.TrimPrefix(raw, "ASCII\x00\x00\x00")
	text = strings.TrimLeft(text, "\x00")

	return parseA1111Parameters(text), nil
}

// parseA1111Parameters parses A1111's parameter-string format. Returns
// nil when the blob lacks the "Negative prompt:" / "Steps:" markers
// that distinguish A1111 from random EXIF UserComment writers (GIMP,
// Paint Tool SAI, etc.).
func parseA1111Parameters(text string) *models.SDMetadata {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

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

	sd.RawParams = rawParams
	if rawParams != "" {
		sd.ParsedParams = parseAllA1111Params(rawParams)
	}
	sd.GenerationHash = computeGenerationHash(
		sd.Prompt, sd.NegativePrompt, sd.Model, sd.Sampler, sd.Steps, sd.CFGScale,
	)
	return sd
}

// findParamLineIndex returns the byte index of the first line starting
// with "Steps:" (the A1111 parameter line marker), or -1.
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

// parseAllA1111Params extracts every "Key: Value" pair from an A1111
// parameter line. Order is preserved; values with braces are kept whole.
func parseAllA1111Params(paramLine string) []models.SDParam {
	paramLine = strings.ReplaceAll(paramLine, "\n", " ")
	var result []models.SDParam
	seen := map[string]bool{}

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

// splitA1111Params splits an A1111 parameter line on commas while
// respecting nested braces.
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
	paramLine = strings.ReplaceAll(paramLine, "\n", " ")
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
