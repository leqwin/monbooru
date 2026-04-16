package metadata

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/leqwin/monbooru/internal/models"
)

var errNotPNG = errors.New("not a PNG file")

// maxPNGChunkBytes bounds any individual tEXt/iTXt/etc. chunk we will buffer.
// A1111 parameter blobs and ComfyUI workflow JSON are kilobytes; the 32-bit
// chunk length field is otherwise attacker-controlled and a forged header
// would otherwise make us allocate up to ~4 GiB per bad file.
const maxPNGChunkBytes = 16 * 1024 * 1024

// readPNGTextChunks reads all tEXt and iTXt chunks from a PNG reader.
// Returns a map of keyword -> text value.
func readPNGTextChunks(r io.Reader) (map[string]string, error) {
	// PNG signature: 8 bytes
	sig := make([]byte, 8)
	if _, err := io.ReadFull(r, sig); err != nil {
		return nil, errNotPNG
	}
	if sig[0] != 0x89 || sig[1] != 0x50 || sig[2] != 0x4E || sig[3] != 0x47 {
		return nil, errNotPNG
	}

	result := map[string]string{}

	for {
		// 4-byte length, 4-byte type
		header := make([]byte, 8)
		if _, err := io.ReadFull(r, header); err != nil {
			break
		}
		length := binary.BigEndian.Uint32(header[:4])
		chunkType := string(header[4:8])

		// Refuse to buffer an oversized chunk. We still need to advance the
		// reader past the chunk body and its CRC to look at anything after it,
		// but we don't materialise the bytes.
		if length > maxPNGChunkBytes {
			if _, err := io.CopyN(io.Discard, r, int64(length)+4); err != nil {
				break
			}
			if chunkType == "IEND" {
				break
			}
			continue
		}

		data := make([]byte, length)
		if _, err := io.ReadFull(r, data); err != nil {
			break
		}

		// Skip CRC (4 bytes)
		crc := make([]byte, 4)
		if _, err := io.ReadFull(r, crc); err != nil {
			break
		}

		switch chunkType {
		case "tEXt":
			// Format: keyword\x00text
			null := strings.IndexByte(string(data), 0)
			if null < 0 {
				continue
			}
			key := string(data[:null])
			val := string(data[null+1:])
			result[key] = val

		case "iTXt":
			// Format: keyword\x00compression_flag\x00compression_method\x00language_tag\x00translated_keyword\x00text
			null1 := strings.IndexByte(string(data), 0)
			if null1 < 0 {
				continue
			}
			key := string(data[:null1])
			rest := data[null1+1:]
			// Skip compression_flag (1), compression_method (1), then two null-terminated strings
			if len(rest) < 2 {
				continue
			}
			rest = rest[2:] // skip flags
			// Skip language tag
			null2 := strings.IndexByte(string(rest), 0)
			if null2 < 0 {
				continue
			}
			rest = rest[null2+1:]
			// Skip translated keyword
			null3 := strings.IndexByte(string(rest), 0)
			if null3 < 0 {
				continue
			}
			val := string(rest[null3+1:])
			result[key] = val
		}

		if chunkType == "IEND" {
			break
		}
	}

	return result, nil
}

// extractComfyUI reads ComfyUI metadata from a PNG reader.
// It tries the "prompt" chunk (API format) first, then falls back to "workflow".
func extractComfyUI(r io.Reader) (*models.ComfyUIMetadata, error) {
	chunks, err := readPNGTextChunks(r)
	if err != nil {
		return nil, nil //nolint:nilerr // non-PNG files return nil gracefully
	}
	if raw, ok := chunks["prompt"]; ok {
		if meta := parseComfyPromptChunk(raw); meta != nil {
			return meta, nil
		}
	}
	if raw, ok := chunks["workflow"]; ok {
		return parseComfyWorkflow(raw), nil
	}
	return nil, nil
}

// extractFromPNG reads both SD and ComfyUI metadata from a PNG file.
func extractFromPNG(path string) (*models.SDMetadata, *models.ComfyUIMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil
	}
	defer f.Close()

	chunks, err := readPNGTextChunks(f)
	if err != nil {
		return nil, nil, nil
	}

	var sd *models.SDMetadata
	var comfy *models.ComfyUIMetadata

	if text, ok := chunks["parameters"]; ok {
		sd = parseA1111Parameters(text)
	}

	// Try "prompt" chunk first (ComfyUI API format — primary format when saving images)
	if raw, ok := chunks["prompt"]; ok {
		comfy = parseComfyPromptChunk(raw)
	}
	// Fallback: try "workflow" chunk (ComfyUI node graph format)
	if raw, ok := chunks["workflow"]; ok && comfy == nil {
		comfy = parseComfyWorkflow(raw)
	}

	return sd, comfy, nil
}
