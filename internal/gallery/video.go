package gallery

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var (
	ffmpegOnce sync.Once
	ffmpegOK   bool
)

// ffmpegAvailable reports whether ffmpeg is on PATH (cached).
func ffmpegAvailable() bool {
	ffmpegOnce.Do(func() {
		_, err := exec.LookPath("ffmpeg")
		ffmpegOK = err == nil
	})
	return ffmpegOK
}

// generateVideoThumb extracts a frame at ~10% of the video's duration.
func generateVideoThumb(srcPath, dstPath string) error {
	if !ffmpegAvailable() {
		return fmt.Errorf("ffmpeg not available")
	}

	duration, err := probeDuration(srcPath)
	if err != nil || duration <= 0 {
		duration = 0
	}

	offset := duration * 0.10
	offsetStr := strconv.FormatFloat(offset, 'f', 3, 64)

	dir := filepath.Dir(dstPath)
	tmp, err := os.CreateTemp(dir, ".vthumb.*.jpg")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmp.Close()
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	// `--` terminates option parsing so a tmpName beginning with `-`
	// stays a positional output path.
	args := []string{
		"-y",
		"-ss", offsetStr,
		"-i", srcPath,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-1", thumbMaxDim),
		"-q:v", "2",
		"--",
		tmpName,
	}
	cmd := exec.Command("ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg thumbnail: %w\n%s", err, string(out))
	}

	return os.Rename(tmpName, dstPath)
}

// generateVideoHover writes a ~4-second animated WebP hover preview.
func generateVideoHover(srcPath, dstPath string) error {
	if !ffmpegAvailable() {
		return fmt.Errorf("ffmpeg not available")
	}

	duration, err := probeDuration(srcPath)
	if err != nil || duration <= 0 {
		duration = 0
	}

	offset := duration * 0.10

	dir := filepath.Dir(dstPath)
	tmp, err := os.CreateTemp(dir, ".vhover.*.webp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmp.Close()
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	args := []string{
		"-y",
		"-ss", strconv.FormatFloat(offset, 'f', 3, 64),
		"-t", "4",
		"-i", srcPath,
		"-vf", fmt.Sprintf("scale=%d:-1", thumbMaxDim),
		"-an",        // no audio
		"-loop", "0", // infinite loop
		"--",
		tmpName,
	}
	cmd := exec.Command("ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg hover: %w\n%s", err, string(out))
	}

	return os.Rename(tmpName, dstPath)
}

// generateGIFHover converts an animated GIF into a scaled WebP preview.
// Silently skipped without ffmpeg; the static first-frame thumbnail
// stays in place.
func generateGIFHover(srcPath, dstPath string) error {
	if !ffmpegAvailable() {
		return fmt.Errorf("ffmpeg not available")
	}

	dir := filepath.Dir(dstPath)
	tmp, err := os.CreateTemp(dir, ".ghover.*.webp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmp.Close()
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	args := []string{
		"-y",
		"-i", srcPath,
		"-vf", fmt.Sprintf("scale=%d:-1", thumbMaxDim),
		"-loop", "0",
		"--",
		tmpName,
	}
	cmd := exec.Command("ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg gif hover: %w\n%s", err, string(out))
	}

	return os.Rename(tmpName, dstPath)
}

// ExtractVideoFrames writes one JPEG per relative offset (0.0..1.0) from
// the video into tmpDir. Frames whose extraction fails are skipped, so a
// shorter-than-requested return slice means partial success.
func ExtractVideoFrames(srcPath, tmpDir string, positions []float64) ([]string, error) {
	if !ffmpegAvailable() {
		return nil, fmt.Errorf("ffmpeg not available")
	}
	duration, _ := probeDuration(srcPath)
	if duration <= 0 {
		duration = 0
	}
	var out []string
	for i, pos := range positions {
		if pos < 0 {
			pos = 0
		} else if pos > 1 {
			pos = 1
		}
		offset := duration * pos
		tmp, err := os.CreateTemp(tmpDir, fmt.Sprintf(".frame-%d.*.jpg", i))
		if err != nil {
			return out, fmt.Errorf("creating temp frame file: %w", err)
		}
		tmp.Close()
		args := []string{
			"-y",
			"-ss", strconv.FormatFloat(offset, 'f', 3, 64),
			"-i", srcPath,
			"-frames:v", "1",
			"-q:v", "2",
			"--",
			tmp.Name(),
		}
		cmd := exec.Command("ffmpeg", args...)
		if _, err := cmd.CombinedOutput(); err != nil {
			os.Remove(tmp.Name())
			continue
		}
		out = append(out, tmp.Name())
	}
	return out, nil
}

// probeDuration returns the video's duration in seconds via ffprobe.
func probeDuration(srcPath string) (float64, error) {
	// `--` terminates option parsing so a filename beginning with `-`
	// is treated as positional rather than a flag.
	cmd := exec.Command("ffprobe",
		"-v", "quiet",
		"-print_format", "csv=p=0",
		"-show_entries", "format=duration",
		"--",
		srcPath,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(out))
	return strconv.ParseFloat(s, 64)
}
