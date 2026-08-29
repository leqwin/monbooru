// Package desktop resolves the per-OS locations and the one-time startup
// behaviour the -desktop profile needs. It knows nothing about the app it
// serves beyond the name it is handed, so both halves of the pair can use
// the same shape without sharing a config.
package desktop

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/monbooru/monbooru/internal/fsx"
)

// Layout is where one app keeps its files under the desktop profile.
// Portable records that the config came from beside the executable, which
// also moves the data directory there.
type Layout struct {
	ConfigPath string
	ConfigDir  string
	DataDir    string
	LogDir     string
	Portable   bool
}

// Resolve builds the layout for app ("monbooru"). explicitConfig is the
// -config value when the operator passed one and always wins, so the
// container entrypoint and the healthcheck are unaffected by the profile.
// Otherwise a config already sitting beside the executable is used in
// place (portable mode: a folder on a stick that carries its own data),
// and failing that the OS config directory, whose file Load then seeds.
func Resolve(app, explicitConfig string) (Layout, error) {
	data, err := dataHome(app)
	if err != nil {
		return Layout{}, err
	}
	l := Layout{DataDir: data}
	switch {
	case explicitConfig != "":
		l.ConfigPath = explicitConfig
	default:
		// A file test, never a write attempt, so an executable in a
		// read-only location does not try to seed one there. Skipped in a
		// sandbox, whose directories are already private to the install.
		if !Sandboxed() {
			if dir := fsx.ExeDir(); dir != "" {
				if p := filepath.Join(dir, app+".toml"); isFile(p) {
					l.ConfigPath, l.Portable = p, true
					l.DataDir = filepath.Join(dir, "data")
				}
			}
		}
		if l.ConfigPath == "" {
			cfgDir, err := os.UserConfigDir()
			if err != nil {
				return Layout{}, fmt.Errorf("locating the config directory: %w", err)
			}
			l.ConfigPath = filepath.Join(cfgDir, app, app+".toml")
		}
	}
	l.ConfigDir = filepath.Dir(l.ConfigPath)
	l.LogDir = filepath.Join(l.DataDir, "logs")
	return l, nil
}

// dataHome is the OS data directory for app. There is no stdlib
// equivalent of os.UserConfigDir for it, so each platform is spelled out;
// macOS does not separate the two, so its data lives under the config dir.
func dataHome(app string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("LocalAppData"); dir != "" {
			return filepath.Join(dir, app), nil
		}
	case "darwin":
	default:
		if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
			return filepath.Join(dir, app), nil
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", app), nil
		}
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating the data directory: %w", err)
	}
	return filepath.Join(dir, app, "data"), nil
}

// Sandboxed reports whether the process runs inside a Flatpak sandbox,
// which redirects the XDG directories and makes portable mode meaningless.
func Sandboxed() bool {
	if os.Getenv("FLATPAK_ID") != "" {
		return true
	}
	return isFile("/.flatpak-info")
}

// PicturesDir is where a fresh install proposes to look for images.
// Empty when nothing resolves, so the caller can fall back.
func PicturesDir() string {
	if runtime.GOOS == "windows" {
		if home := os.Getenv("USERPROFILE"); home != "" {
			return filepath.Join(home, "Pictures")
		}
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if dir := xdgUserDir("XDG_PICTURES_DIR", home); dir != "" {
		return dir
	}
	return filepath.Join(home, "Pictures")
}

// xdgUserDir reads one entry out of ~/.config/user-dirs.dirs, the file
// xdg-user-dirs writes and every desktop honours for a localised or
// relocated Pictures folder. Absent or unparseable reads as unset.
func xdgUserDir(key, home string) string {
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	f, err := os.Open(filepath.Join(cfgDir, "user-dirs.dirs"))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		rest, ok := strings.CutPrefix(line, key+"=")
		if !ok {
			continue
		}
		val := strings.Trim(strings.TrimSpace(rest), `"`)
		val = strings.TrimSpace(strings.Replace(val, "$HOME", home, 1))
		if val == "" {
			continue
		}
		// xdg-user-dirs writes the home directory itself for a folder the
		// user disabled; taking it would point the seed at the whole home.
		if val = filepath.Clean(val); val != home {
			return val
		}
	}
	return ""
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
