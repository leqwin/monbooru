package metadata

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"strings"

	"github.com/leqwin/monbooru/internal/models"
	"github.com/rwcarlsen/goexif/exif"
)

// extractSDFromWebP reads A1111 metadata from a WebP file's EXIF chunk.
// WebP is a RIFF container; we walk the chunks to find "EXIF", then hand
// the payload to goexif (after re-prefixing the "Exif\x00\x00" magic so
// exif.Decode's scanner finds it regardless of whether the chunk already
// includes the prefix).
func extractSDFromWebP(path string) (*models.SDMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()

	exifData, err := readWebPEXIF(f)
	if err != nil || exifData == nil {
		return nil, nil
	}

	x, err := exif.Decode(io.MultiReader(bytes.NewReader([]byte("Exif\x00\x00")), bytes.NewReader(exifData)))
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
	text := strings.TrimPrefix(raw, "ASCII\x00\x00\x00")
	text = strings.TrimLeft(text, "\x00")
	return parseA1111Parameters(text), nil
}

// maxWebPChunkBytes bounds any individual RIFF chunk we will buffer. EXIF
// payloads that carry A1111 parameters are a few KB; a forged size field
// would otherwise trigger a ~4 GiB allocation before ReadFull failed.
const maxWebPChunkBytes = 16 * 1024 * 1024

// readWebPEXIF returns the raw EXIF chunk bytes from a WebP RIFF stream,
// or nil if the file is not a valid WebP or has no EXIF chunk.
func readWebPEXIF(r io.Reader) ([]byte, error) {
	header := make([]byte, 12)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WEBP" {
		return nil, nil
	}
	for {
		chunk := make([]byte, 8)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return nil, nil
		}
		chunkType := string(chunk[0:4])
		size := binary.LittleEndian.Uint32(chunk[4:8])
		if size > maxWebPChunkBytes {
			// Skip oversize chunks wholesale - advance over the payload and
			// padding so subsequent chunks still line up.
			toSkip := int64(size)
			if size%2 == 1 {
				toSkip++
			}
			if _, err := io.CopyN(io.Discard, r, toSkip); err != nil {
				return nil, nil
			}
			continue
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, nil
		}
		if size%2 == 1 {
			// RIFF chunks are word-aligned; skip the 1-byte padding.
			pad := make([]byte, 1)
			io.ReadFull(r, pad)
		}
		if chunkType == "EXIF" {
			// Some encoders prepend the JPEG-style "Exif\x00\x00" magic; strip it
			// so the caller can prepend a known-good copy unconditionally.
			data = bytes.TrimPrefix(data, []byte("Exif\x00\x00"))
			return data, nil
		}
	}
}

// genericFromWebP returns EXIF tags from a WebP file (UserComment excluded).
func genericFromWebP(path string) []models.SDParam {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	exifData, err := readWebPEXIF(f)
	if err != nil || exifData == nil {
		return nil
	}
	x, err := exif.Decode(io.MultiReader(bytes.NewReader([]byte("Exif\x00\x00")), bytes.NewReader(exifData)))
	if err != nil {
		return nil
	}
	return collectEXIFTags(x)
}
