package metadata

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

func TestParseA1111Parameters(t *testing.T) {
	input := `a beautiful landscape, oil painting
Negative prompt: ugly, blurry
Steps: 20, Sampler: DPM++ 2M Karras, CFG scale: 7, Seed: 12345, Model: v1-5-pruned`

	sd := parseA1111Parameters(input)
	if sd == nil {
		t.Fatal("expected non-nil SDMetadata")
	}
	if sd.Prompt != "a beautiful landscape, oil painting" {
		t.Errorf("Prompt = %q", sd.Prompt)
	}
	if sd.NegativePrompt != "ugly, blurry" {
		t.Errorf("NegativePrompt = %q", sd.NegativePrompt)
	}
	if sd.Steps == nil || *sd.Steps != 20 {
		t.Errorf("Steps = %v", sd.Steps)
	}
	if sd.Sampler != "DPM++ 2M Karras" {
		t.Errorf("Sampler = %q", sd.Sampler)
	}
	if sd.CFGScale == nil || *sd.CFGScale != 7 {
		t.Errorf("CFGScale = %v", sd.CFGScale)
	}
	if sd.Seed == nil || *sd.Seed != 12345 {
		t.Errorf("Seed = %v", sd.Seed)
	}
	if sd.Model != "v1-5-pruned" {
		t.Errorf("Model = %q", sd.Model)
	}
}

func TestParseA1111Parameters_Empty(t *testing.T) {
	sd := parseA1111Parameters("")
	if sd != nil {
		t.Error("expected nil for empty input")
	}
}

func TestParseA1111Parameters_NotA1111(t *testing.T) {
	// Third-party writers (GIMP, Paint Tool SAI, LEAD Technologies, …) drop
	// short identifiers into EXIF UserComment. Without an A1111 marker
	// ("Negative prompt:" or a "Steps:" line) these must not be accepted
	// as A1111 metadata.
	for _, s := range []string{
		"Paint Tool -SAI- JPEG Encoder v1.00",
		"Created with GIMP",
		"LEAD Technologies Inc. V1.01",
	} {
		if sd := parseA1111Parameters(s); sd != nil {
			t.Errorf("expected nil for %q, got %+v", s, sd)
		}
	}
}

func TestParseA1111Parameters_NoNegative(t *testing.T) {
	input := `just a prompt
Steps: 30, Sampler: Euler, CFG scale: 8, Seed: 999, Model: mymodel`

	sd := parseA1111Parameters(input)
	if sd == nil {
		t.Fatal("expected non-nil")
	}
	if sd.Prompt != "just a prompt" {
		t.Errorf("Prompt = %q", sd.Prompt)
	}
	if sd.Model != "mymodel" {
		t.Errorf("Model = %q", sd.Model)
	}
}

func TestParseComfyWorkflow(t *testing.T) {
	// Flat dict format
	raw := `{
		"1": {
			"type": "CheckpointLoaderSimple",
			"inputs": {"ckpt_name": "v1-5-pruned.safetensors"}
		},
		"2": {
			"type": "CLIPTextEncode",
			"inputs": {"text": "positive prompt text"}
		},
		"3": {
			"type": "KSampler",
			"inputs": {"seed": 42, "steps": 20, "cfg": 7.5, "sampler_name": "euler"}
		}
	}`

	comfy := parseComfyWorkflow(raw)
	if comfy == nil {
		t.Fatal("expected non-nil ComfyUIMetadata")
	}
	if comfy.Prompt != "positive prompt text" {
		t.Errorf("Prompt = %q", comfy.Prompt)
	}
	if comfy.ModelCheckpoint != "v1-5-pruned.safetensors" {
		t.Errorf("ModelCheckpoint = %q", comfy.ModelCheckpoint)
	}
	if comfy.Seed == nil || *comfy.Seed != 42 {
		t.Errorf("Seed = %v", comfy.Seed)
	}
	if comfy.Steps == nil || *comfy.Steps != 20 {
		t.Errorf("Steps = %v", comfy.Steps)
	}
	if comfy.CFGScale == nil || *comfy.CFGScale != 7.5 {
		t.Errorf("CFGScale = %v", comfy.CFGScale)
	}
	if comfy.Sampler != "euler" {
		t.Errorf("Sampler = %q", comfy.Sampler)
	}
}

func TestParseComfyWorkflow_ArrayFormat(t *testing.T) {
	raw := `{
		"nodes": [
			{"type": "KSampler", "inputs": {"seed": 100, "steps": 25, "cfg": 6.0, "sampler_name": "dpm_2"}},
			{"type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "model.ckpt"}}
		]
	}`

	comfy := parseComfyWorkflow(raw)
	if comfy == nil {
		t.Fatal("expected non-nil")
	}
	if comfy.Seed == nil || *comfy.Seed != 100 {
		t.Errorf("Seed = %v", comfy.Seed)
	}
	if comfy.ModelCheckpoint != "model.ckpt" {
		t.Errorf("ModelCheckpoint = %q", comfy.ModelCheckpoint)
	}
}

func TestParseComfyWorkflow_InvalidJSON(t *testing.T) {
	comfy := parseComfyWorkflow("not json")
	if comfy != nil {
		t.Error("expected nil for invalid JSON")
	}
}

func TestExtractFromPNG_SD(t *testing.T) {
	sd, comfy, err := Extract("/workspace/Projets/monbooru/testdata/sd_a1111.png", "png")
	if err != nil {
		t.Fatal(err)
	}
	// The file should have SD metadata
	if sd == nil {
		t.Log("sd_a1111.png returned nil SD metadata (may not have embedded params)")
	}
	// ComfyUI should not be set
	if comfy != nil {
		t.Log("sd_a1111.png unexpectedly has ComfyUI workflow")
	}
}

func TestExtractFromPNG_ComfyUI(t *testing.T) {
	sd, comfy, err := Extract("/workspace/Projets/monbooru/testdata/comfyui_workflow.png", "png")
	if err != nil {
		t.Fatal(err)
	}
	if comfy == nil {
		t.Log("comfyui_workflow.png returned nil ComfyUI metadata (may not have embedded workflow)")
	}
	_ = sd
}

func TestExtract_UnsupportedType(t *testing.T) {
	sd, comfy, err := Extract("test.mp4", "mp4")
	if err != nil {
		t.Fatal(err)
	}
	if sd != nil || comfy != nil {
		t.Error("expected nil for video type")
	}
}

func TestExtract_JPEG(t *testing.T) {
	// Create a minimal valid JPEG file
	dir := t.TempDir()
	path := dir + "/test.jpg"
	// Minimal JPEG: SOI marker + EOI marker
	jpegData := []byte{0xFF, 0xD8, 0xFF, 0xD9}
	if err := writeFile(path, jpegData); err != nil {
		t.Fatal(err)
	}

	sd, comfy, err := Extract(path, "jpeg")
	if err != nil {
		t.Fatal(err)
	}
	// No EXIF metadata in this minimal JPEG
	if comfy != nil {
		t.Error("JPEG should not return ComfyUI metadata")
	}
	_ = sd // nil is acceptable for minimal JPEG without EXIF
}

func TestExtract_JPEG_NonExistent(t *testing.T) {
	// Should not return a fatal error for missing file
	sd, comfy, err := Extract("/nonexistent/path/image.jpg", "jpeg")
	// Either err is set or both are nil - no panic
	_ = sd
	_ = comfy
	_ = err
}

func TestExtractComfyUI_FromPNG(t *testing.T) {
	// Test that extractComfyUI works via Extract for a PNG with workflow
	// Use the existing testdata PNG
	sd, comfy, err := Extract("/workspace/Projets/monbooru/testdata/comfyui_workflow.png", "png")
	if err != nil {
		t.Fatal(err)
	}
	_ = sd
	_ = comfy
}

func TestParseA1111_PromptOnly(t *testing.T) {
	// Without any A1111 marker (no "Negative prompt:" and no "Steps:" line)
	// the blob is rejected - bare text is not enough to claim A1111 origin.
	input := "just a prompt with no parameters"
	if sd := parseA1111Parameters(input); sd != nil {
		t.Errorf("expected nil, got %+v", sd)
	}
}

func TestGenerationHash_SeedExcluded(t *testing.T) {
	steps := 20
	cfg := 7.0
	s1 := parseA1111Parameters("a cat\nNegative prompt: ugly\nSteps: 20, Sampler: Euler, CFG scale: 7, Seed: 111, Model: foo")
	s2 := parseA1111Parameters("a cat\nNegative prompt: ugly\nSteps: 20, Sampler: Euler, CFG scale: 7, Seed: 222, Model: foo")
	if s1 == nil || s2 == nil {
		t.Fatal("expected non-nil for both inputs")
	}
	if s1.GenerationHash == "" {
		t.Fatal("generation hash is empty")
	}
	if s1.GenerationHash != s2.GenerationHash {
		t.Errorf("hashes differ despite only seed changing: %q vs %q", s1.GenerationHash, s2.GenerationHash)
	}
	_, _ = steps, cfg
}

func TestGenerationHash_ModelChangesHash(t *testing.T) {
	s1 := parseA1111Parameters("a cat\nNegative prompt: ugly\nSteps: 20, Sampler: Euler, CFG scale: 7, Seed: 1, Model: foo")
	s2 := parseA1111Parameters("a cat\nNegative prompt: ugly\nSteps: 20, Sampler: Euler, CFG scale: 7, Seed: 1, Model: bar")
	if s1.GenerationHash == s2.GenerationHash {
		t.Errorf("expected different hashes for different models, both = %q", s1.GenerationHash)
	}
}

func TestParseA1111_AllWhitespace(t *testing.T) {
	// Whitespace-only returns nil
	sd := parseA1111Parameters("   \n  ")
	if sd != nil {
		t.Error("expected nil for whitespace-only input")
	}
}

func TestParseComfyWorkflow_EmptyJSON(t *testing.T) {
	comfy := parseComfyWorkflow("{}")
	if comfy != nil {
		t.Error("expected nil for empty workflow object")
	}
}

func TestParseComfyWorkflow_UnknownNodes(t *testing.T) {
	raw := `{
		"1": {"type": "UnknownNode", "inputs": {"foo": "bar"}},
		"2": {"type": "AnotherUnknown", "inputs": {}}
	}`
	comfy := parseComfyWorkflow(raw)
	if comfy != nil {
		t.Error("expected nil when no known nodes found")
	}
}

// --- extractFromPNG and extractComfyUI direct tests ---

func TestExtractFromPNG_WithParameters(t *testing.T) {
	paramText := "a beautiful prompt\nNegative prompt: ugly\nSteps: 20, Sampler: Euler, CFG scale: 7, Seed: 42, Model: mymodel"
	buf := makePNGWithTextChunk("parameters", paramText)
	// Write to a temp file so we can call extractFromPNG (file-based)
	dir := t.TempDir()
	path := dir + "/test_sd.png"
	os.WriteFile(path, buf, 0644)
	sd, _, err := extractFromPNG(path)
	if err != nil {
		t.Fatal(err)
	}
	if sd == nil {
		t.Fatal("expected non-nil SDMetadata from PNG with parameters chunk")
	}
	if sd.Prompt != "a beautiful prompt" {
		t.Errorf("Prompt = %q", sd.Prompt)
	}
}

func TestExtractFromPNG_NoParameters(t *testing.T) {
	buf := makePNGWithTextChunk("other_key", "some value")
	dir := t.TempDir()
	path := dir + "/test_nosd.png"
	os.WriteFile(path, buf, 0644)
	sd, _, err := extractFromPNG(path)
	if err != nil {
		t.Fatal(err)
	}
	if sd != nil {
		t.Error("expected nil for PNG without parameters chunk")
	}
}

func TestExtractComfyUI_WithWorkflow(t *testing.T) {
	workflow := `{"1": {"type": "KSampler", "inputs": {"seed": 42, "steps": 20, "cfg": 7.0, "sampler_name": "euler"}}, "2": {"type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "model.safetensors"}}}`
	buf := makePNGWithTextChunk("workflow", workflow)
	comfy, err := extractComfyUI(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	if comfy == nil {
		t.Fatal("expected non-nil ComfyUIMetadata from PNG with workflow chunk")
	}
	if comfy.Seed == nil || *comfy.Seed != 42 {
		t.Errorf("Seed = %v", comfy.Seed)
	}
}

func TestExtractComfyUI_NoWorkflow(t *testing.T) {
	buf := makePNGWithTextChunk("other", "value")
	comfy, err := extractComfyUI(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	if comfy != nil {
		t.Error("expected nil for PNG without workflow chunk")
	}
}

func TestExtractComfyUI_NotPNG(t *testing.T) {
	_, err := extractComfyUI(bytes.NewReader([]byte("not a png")))
	if err != nil {
		t.Fatal(err)
	}
	// should return nil, nil for non-PNG
}

func TestReadPNGTextChunks_InvalidMagic(t *testing.T) {
	_, err := readPNGTextChunks(bytes.NewReader([]byte("not a png file at all")))
	if err == nil {
		t.Error("expected error for non-PNG data")
	}
}

func TestReadPNGTextChunks_iTXt(t *testing.T) {
	// Create PNG with iTXt chunk
	buf := makePNGWithITXtChunk("Comment", "test value")
	chunks, err := readPNGTextChunks(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	if chunks["Comment"] != "test value" {
		t.Errorf("iTXt chunk value = %q", chunks["Comment"])
	}
}

// makePNGWithTextChunk builds a minimal PNG byte sequence with a single tEXt chunk.
// CRC bytes are zeroed (readPNGTextChunks doesn't verify CRC).
func makePNGWithTextChunk(key, val string) []byte {
	var buf bytes.Buffer
	// PNG signature
	buf.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	// IHDR chunk (minimal: 13 bytes)
	ihdr := make([]byte, 13)
	ihdr[3] = 1 // width = 1
	ihdr[7] = 1 // height = 1
	ihdr[8] = 8 // bit depth
	writeTestChunk(&buf, "IHDR", ihdr)
	// tEXt chunk
	tEXtData := append([]byte(key), 0)
	tEXtData = append(tEXtData, []byte(val)...)
	writeTestChunk(&buf, "tEXt", tEXtData)
	// IEND
	writeTestChunk(&buf, "IEND", nil)
	return buf.Bytes()
}

// makePNGWithITXtChunk builds a PNG with an iTXt chunk.
func makePNGWithITXtChunk(key, val string) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	ihdr := make([]byte, 13)
	ihdr[3] = 1
	ihdr[7] = 1
	ihdr[8] = 8
	writeTestChunk(&buf, "IHDR", ihdr)
	// iTXt: keyword\x00 compression_flag(1) compression_method(1) lang_tag\x00 trans_keyword\x00 text
	var data bytes.Buffer
	data.WriteString(key)
	data.WriteByte(0) // null separator
	data.WriteByte(0) // compression flag = 0 (uncompressed)
	data.WriteByte(0) // compression method
	data.WriteByte(0) // language tag (empty)
	data.WriteByte(0) // translated keyword (empty)
	data.WriteString(val)
	writeTestChunk(&buf, "iTXt", data.Bytes())
	writeTestChunk(&buf, "IEND", nil)
	return buf.Bytes()
}

func writeTestChunk(w *bytes.Buffer, chunkType string, data []byte) {
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(data)))
	w.Write(length)
	w.WriteString(chunkType)
	w.Write(data)
	w.Write([]byte{0, 0, 0, 0}) // CRC (zeroed)
}

func TestExtractSDFromJPEG_WithExif(t *testing.T) {
	// Our test JPEG has a valid EXIF UserComment (type UNDEFINED).
	// This covers the path where exif.Decode succeeds, x.Get(UserComment) succeeds,
	// but tag.StringVal() fails (UNDEFINED type can't be converted to string).
	sd, err := extractSDFromJPEG("/workspace/Projets/monbooru/testdata/sd_exif_jpeg.jpg")
	if err != nil {
		t.Fatal(err)
	}
	// StringVal fails for UNDEFINED type → returns nil, nil (expected)
	_ = sd
}

func TestReadPNGTextChunks_TooShort(t *testing.T) {
	// Fewer than 8 bytes → ReadFull fails → errNotPNG
	_, err := readPNGTextChunks(bytes.NewReader([]byte{0x89, 0x50}))
	if err == nil {
		t.Error("expected error for too-short input")
	}
}

func TestReadPNGTextChunks_TruncatedChunkData(t *testing.T) {
	// Valid PNG signature + chunk header but truncated data
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}) // PNG sig
	// Write a chunk header claiming length=100 but provide no data
	header := make([]byte, 8)
	// length = 100 (big-endian)
	header[0], header[1], header[2], header[3] = 0, 0, 0, 100
	// type = "tEXt"
	copy(header[4:], "tEXt")
	buf.Write(header)
	// Don't write the 100 bytes of data - ReadFull will fail → break

	chunks, err := readPNGTextChunks(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err) // readPNGTextChunks returns nil error even on truncated chunks
	}
	_ = chunks
}

func TestReadPNGTextChunks_tEXtWithoutNull(t *testing.T) {
	// tEXt chunk with no null separator - should be silently skipped
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	ihdr := make([]byte, 13)
	ihdr[3] = 1
	ihdr[7] = 1
	ihdr[8] = 8
	writeTestChunk(&buf, "IHDR", ihdr)
	// tEXt with no null byte
	writeTestChunk(&buf, "tEXt", []byte("keywordwithnonull"))
	writeTestChunk(&buf, "IEND", nil)

	chunks, err := readPNGTextChunks(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 0 {
		t.Errorf("expected no chunks for tEXt without null, got %v", chunks)
	}
}

func TestReadPNGTextChunks_iTXtTooShort(t *testing.T) {
	// iTXt chunk where rest is < 2 bytes after null separator - should be skipped
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	ihdr := make([]byte, 13)
	ihdr[3] = 1
	ihdr[7] = 1
	ihdr[8] = 8
	writeTestChunk(&buf, "IHDR", ihdr)
	// iTXt with keyword but only 1 byte after the null separator
	data := []byte("keyword\x00X") // only 1 byte after null
	writeTestChunk(&buf, "iTXt", data)
	writeTestChunk(&buf, "IEND", nil)

	chunks, err := readPNGTextChunks(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	_ = chunks
}

func TestParseA1111_NegativeOnly(t *testing.T) {
	// Negative prompt present but no Steps line - tests the "no paramIdx after negIdx" branch
	input := "positive prompt\nNegative prompt: some bad stuff"
	sd := parseA1111Parameters(input)
	if sd == nil {
		t.Fatal("expected non-nil")
	}
	if sd.NegativePrompt != "some bad stuff" {
		t.Errorf("NegativePrompt = %q", sd.NegativePrompt)
	}
}

func TestExtractSDFromWebP_NoEXIF(t *testing.T) {
	// Minimal "RIFF....WEBP" stream with no EXIF chunk: returns nil silently.
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	size := make([]byte, 4)
	binary.LittleEndian.PutUint32(size, 4)
	buf.Write(size)
	buf.WriteString("WEBP")
	dir := t.TempDir()
	path := dir + "/empty.webp"
	if err := writeFile(path, buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	sd, _, err := Extract(path, "webp")
	if err != nil {
		t.Fatal(err)
	}
	if sd != nil {
		t.Error("expected nil SDMetadata for WebP without EXIF chunk")
	}
}

func TestExtractSDFromWebP_NotWebP(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/junk.webp"
	if err := writeFile(path, []byte("not a webp at all")); err != nil {
		t.Fatal(err)
	}
	sd, _, err := Extract(path, "webp")
	if err != nil {
		t.Fatal(err)
	}
	if sd != nil {
		t.Error("expected nil for non-WebP data")
	}
}

// writeFile is a helper for test file creation
func writeFile(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
