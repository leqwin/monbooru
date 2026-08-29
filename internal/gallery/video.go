package gallery

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/fsx"
)

var (
	toolsOnce   sync.Once
	ffmpegPath  string
	ffprobePath string
)

// resolveTools finds ffmpeg and ffprobe once, as absolute paths. A copy
// shipped beside the executable wins over one on PATH: that rung is the
// whole reason a downloaded bundle works, where nothing sets anything up
// before the process starts. Inside a container or a sandbox the tools are
// on PATH anyway, so it is harmless there rather than load bearing.
func resolveTools() {
	toolsOnce.Do(func() {
		ffmpegPath = resolveTool("ffmpeg")
		ffprobePath = resolveTool("ffprobe")
	})
}

func resolveTool(name string) string {
	binary := name
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if dir := fsx.ExeDir(); dir != "" {
		if p := filepath.Join(dir, binary); runnable(p) {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// runnable rejects a file that is there but cannot be executed - a tarball
// unpacked without the mode bits - so it does not shadow a working copy on
// PATH.
func runnable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || fi.Mode()&0o111 != 0
}

// ffmpegTimeout caps any single ffmpeg/ffprobe run. The per-file size cap
// bounds bytes, not decode time, so a truncated or pathological-but-small
// media file could otherwise wedge the ingest/thumbnail goroutine (and its
// held write transaction) until killed by hand.
const ffmpegTimeout = 60 * time.Second

// runFFmpeg executes ffmpeg/ffprobe under ffmpegTimeout. A timeout surfaces
// as a normal command error, which every caller already turns into a skip.
func runFFmpeg(combinedOutput bool, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ffmpegTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if combinedOutput {
		return cmd.CombinedOutput()
	}
	return cmd.Output()
}

// ffmpegAvailable reports whether an ffmpeg was resolved (cached).
func ffmpegAvailable() bool {
	resolveTools()
	return ffmpegPath != ""
}

// ffprobeAvailable is the same for the probe half. The two are shipped
// together but nothing guarantees both are present.
func ffprobeAvailable() bool {
	resolveTools()
	return ffprobePath != ""
}

// runFFmpegToFile runs one ffmpeg encode into a temp file next to dstPath
// and renames it over on success. args receives the temp path and returns
// the full argument list; every caller ends its list with `--` + the temp
// path so a name beginning with `-` stays a positional output. label names
// the step in the error.
func runFFmpegToFile(dstPath, tmpPattern, label string, args func(tmp string) []string) error {
	if !ffmpegAvailable() {
		return fmt.Errorf("ffmpeg not available")
	}
	tmp, err := os.CreateTemp(filepath.Dir(dstPath), tmpPattern)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	_ = tmp.Close()
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if out, err := runFFmpeg(true, ffmpegPath, args(tmpName)...); err != nil {
		return fmt.Errorf("ffmpeg %s: %w\n%s", label, err, string(out))
	}
	return os.Rename(tmpName, dstPath)
}

// NormalizeImage re-encodes srcPath in place to a baseline JPEG via
// ffmpeg. Some CDN image resizers emit JPEGs with a luma/chroma
// subsampling ratio Go's image/jpeg refuses ("unsupported JPEG
// feature"); ffmpeg decodes them, and the re-encode lands a file the
// stdlib decode path - dimension probe, thumbnail, phash - can read.
// The caller passes only a freshly uploaded file it owns, so no
// operator file on disk is rewritten. Returns an error when ffmpeg is
// absent or the re-encode fails, leaving the original in place.
func NormalizeImage(srcPath string) error {
	// `-update 1` writes a single still image rather than a numbered
	// sequence.
	return runFFmpegToFile(srcPath, ".normalize.*.jpg", "normalize", func(tmp string) []string {
		return []string{
			"-y",
			"-i", srcPath,
			"-update", "1",
			"-frames:v", "1",
			"-q:v", "2",
			"--",
			tmp,
		}
	})
}

// generateVideoThumb extracts a frame at ~10% of the video's duration.
func generateVideoThumb(srcPath, dstPath string) error {
	duration, err := probeDuration(srcPath)
	if err != nil || duration <= 0 {
		duration = 0
	}
	offsetStr := strconv.FormatFloat(duration*0.10, 'f', 3, 64)
	return runFFmpegToFile(dstPath, ".vthumb.*.jpg", "thumbnail", func(tmp string) []string {
		return []string{
			"-y",
			"-ss", offsetStr,
			"-i", srcPath,
			"-frames:v", "1",
			"-vf", fmt.Sprintf("scale=%d:-1", thumbMaxDim),
			"-q:v", "2",
			"--",
			tmp,
		}
	})
}

// generateVideoHover writes a ~4-second animated WebP hover preview.
func generateVideoHover(srcPath, dstPath string) error {
	duration, err := probeDuration(srcPath)
	if err != nil || duration <= 0 {
		duration = 0
	}
	offsetStr := strconv.FormatFloat(duration*0.10, 'f', 3, 64)
	return runFFmpegToFile(dstPath, ".vhover.*.webp", "hover", func(tmp string) []string {
		return []string{
			"-y",
			"-ss", offsetStr,
			"-t", "4",
			"-i", srcPath,
			"-vf", fmt.Sprintf("scale=%d:-1", thumbMaxDim),
			"-an",        // no audio
			"-loop", "0", // infinite loop
			"--",
			tmp,
		}
	})
}

// generateGIFHover converts an animated GIF into a scaled WebP preview.
// Silently skipped without ffmpeg; the static first-frame thumbnail
// stays in place.
func generateGIFHover(srcPath, dstPath string) error {
	return runFFmpegToFile(dstPath, ".ghover.*.webp", "gif hover", func(tmp string) []string {
		return []string{
			"-y",
			"-i", srcPath,
			"-vf", fmt.Sprintf("scale=%d:-1", thumbMaxDim),
			"-loop", "0",
			"--",
			tmp,
		}
	})
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
		offset := duration * min(max(pos, 0), 1)
		tmp, err := os.CreateTemp(tmpDir, fmt.Sprintf(".frame-%d.*.jpg", i))
		if err != nil {
			return out, fmt.Errorf("creating temp frame file: %w", err)
		}
		_ = tmp.Close()
		args := []string{
			"-y",
			"-ss", strconv.FormatFloat(offset, 'f', 3, 64),
			"-i", srcPath,
			"-frames:v", "1",
			"-q:v", "2",
			"--",
			tmp.Name(),
		}
		if _, err := runFFmpeg(true, ffmpegPath, args...); err != nil {
			_ = os.Remove(tmp.Name())
			continue
		}
		out = append(out, tmp.Name())
	}
	return out, nil
}

// probeDuration returns the video's duration in seconds via ffprobe.
func probeDuration(srcPath string) (float64, error) {
	// The thumbnail path reaches this before anything has resolved the
	// tools, and an unresolved ffprobe would report no duration at all.
	resolveTools()
	// `--` terminates option parsing so a filename beginning with `-`
	// is treated as positional rather than a flag.
	out, err := runFFmpeg(false, ffprobePath,
		"-v", "quiet",
		"-print_format", "csv=p=0",
		"-show_entries", "format=duration",
		"--",
		srcPath,
	)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(out))
	return strconv.ParseFloat(s, 64)
}

// ProbeDurationSeconds is the public-package wrapper around the
// internal duration probe. Callers in the ingest and re-extract paths
// use it to populate images.duration_seconds for video rows. Returns
// (0, false) when ffmpeg is unavailable or probing fails; callers
// leave the column NULL in that case.
func ProbeDurationSeconds(srcPath string) (float64, bool) {
	if !ffprobeAvailable() {
		return 0, false
	}
	d, err := probeDuration(srcPath)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// ProbeVideoDimensions returns the first video stream's width and
// height via ffprobe. Mirrors ProbeDurationSeconds: (0, 0, false) when
// ffmpeg is unavailable or the probe fails so callers leave width and
// height NULL in that case.
func ProbeVideoDimensions(srcPath string) (int, int, bool) {
	if !ffprobeAvailable() {
		return 0, 0, false
	}
	out, err := runFFmpeg(false, ffprobePath,
		"-v", "quiet",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-print_format", "csv=p=0:s=x",
		"--",
		srcPath,
	)
	if err != nil {
		return 0, 0, false
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "x", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err := strconv.Atoi(parts[0])
	if err != nil || w <= 0 {
		return 0, 0, false
	}
	h, err := strconv.Atoi(parts[1])
	if err != nil || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}
