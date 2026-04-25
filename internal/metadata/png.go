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

// maxPNGChunkBytes bounds any individual chunk we'll buffer. A1111
// parameter blobs and ComfyUI workflow JSON are kilobytes; the 32-bit
// chunk length field is otherwise attacker-controlled and a forged
// header could try to allocate up to ~4 GiB.
const maxPNGChunkBytes = 16 * 1024 * 1024

// readPNGTextChunks returns every tEXt and iTXt chunk from a PNG reader
// as keyword -> text.
func readPNGTextChunks(r io.Reader) (map[string]string, error) {
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

		// Skip oversized chunks without materialising them, but still
		// advance the reader past their body + CRC.
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

		crc := make([]byte, 4)
		if _, err := io.ReadFull(r, crc); err != nil {
			break
		}

		switch chunkType {
		case "tEXt":
			// keyword\x00text
			null := strings.IndexByte(string(data), 0)
			if null < 0 {
				continue
			}
			result[string(data[:null])] = string(data[null+1:])

		case "iTXt":
			// keyword \x00 cflag \x00 cmethod \x00 lang \x00 tkeyword \x00 text
			null1 := strings.IndexByte(string(data), 0)
			if null1 < 0 {
				continue
			}
			key := string(data[:null1])
			rest := data[null1+1:]
			if len(rest) < 2 {
				continue
			}
			rest = rest[2:] // cflag + cmethod
			null2 := strings.IndexByte(string(rest), 0)
			if null2 < 0 {
				continue
			}
			rest = rest[null2+1:] // skip language tag
			null3 := strings.IndexByte(string(rest), 0)
			if null3 < 0 {
				continue
			}
			result[key] = string(rest[null3+1:])
		}

		if chunkType == "IEND" {
			break
		}
	}

	return result, nil
}

// extractComfyUI reads ComfyUI metadata from a PNG reader, trying the
// "prompt" chunk (API format) first and falling back to "workflow".
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

// extractFromPNG reads SD and ComfyUI metadata from a PNG file.
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

	// "prompt" is ComfyUI's primary API-format chunk; fall back to
	// "workflow" (node-graph format) if it doesn't parse.
	if raw, ok := chunks["prompt"]; ok {
		comfy = parseComfyPromptChunk(raw)
	}
	if raw, ok := chunks["workflow"]; ok && comfy == nil {
		comfy = parseComfyWorkflow(raw)
	}

	return sd, comfy, nil
}
