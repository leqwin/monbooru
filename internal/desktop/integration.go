package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Hook is the pair of desktop integrations an app can add and remove for
// itself: an applications-menu entry and a start-at-login entry. Both read
// their state back from disk, so a file removed by hand shows as off.
//
// Icon is a PNG installed where the platform wants one; nil skips it.
type Hook struct {
	App     string
	Name    string
	Comment string
	Icon    []byte
}

// MenuSupported reports whether writing a menu entry would help. A packaged
// or sandboxed install already ships one, and a second entry is a duplicate
// in the launcher rather than a fix.
func (h Hook) MenuSupported() bool { return menuSupported() }

// MenuEnabled reports whether the entry is on disk.
func (h Hook) MenuEnabled() bool { return menuEnabled(h.App) }

// SetMenu adds or removes the entry.
func (h Hook) SetMenu(on bool) error {
	if !h.MenuSupported() {
		return fmt.Errorf("the applications menu entry is provided by the install here")
	}
	if on {
		return enableMenu(h)
	}
	return disableMenu(h.App)
}

// AutostartSupported reports whether start-at-login can be written here.
func (h Hook) AutostartSupported() bool { return autostartSupported() }

// AutostartEnabled reports whether the app starts at login.
func (h Hook) AutostartEnabled() bool { return autostartEnabled(h.App) }

// SetAutostart turns start-at-login on or off.
func (h Hook) SetAutostart(on bool) error {
	if !h.AutostartSupported() {
		return fmt.Errorf("start at login is not supported on this platform")
	}
	if on {
		return enableAutostart(h)
	}
	return disableAutostart(h.App)
}

// launchTarget is what starts this install: the program and its arguments,
// unquoted, because each sink quotes them by its own rules. Inside a
// sandbox it is the host's view of the app, not the path inside it, which
// is the only form the host can launch.
func launchTarget() (string, []string) {
	if id := os.Getenv("FLATPAK_ID"); id != "" {
		return "flatpak", []string{"run", id, "-desktop"}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", nil
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, []string{"-desktop"}
}

// LaunchCommand is the Exec line a .desktop entry runs.
func LaunchCommand() string {
	exe, args := launchTarget()
	if exe == "" {
		return ""
	}
	return strings.Join(append([]string{execQuote(exe)}, args...), " ")
}

// execQuote wraps a path for a .desktop Exec line, which reserves a handful
// of characters inside its own quoting rules.
func execQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\$`") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`)
	return `"` + r.Replace(s) + `"`
}
