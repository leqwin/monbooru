// Monbooru is a Linux-only deployment; path handling assumes forward slashes.
package gallery

import (
	"os"

	"github.com/leqwin/monbooru/internal/logx"
)

// ClaimOwnership chowns path to the current process UID/GID so later
// rename/delete operations don't hit permission errors when the file
// was originally written by a different user (e.g. files synced into
// the gallery from another machine before monbooru started managing
// them, or rootless Podman where container UID 0 maps to a host UID
// that doesn't own the bind-mounted files).
//
// Failures are logged at debug and otherwise ignored - chown is
// best-effort and must never abort the surrounding operation. ENOENT
// is silenced because callers race deletions and watcher events.
func ClaimOwnership(path string) {
	if err := os.Chown(path, os.Getuid(), os.Getgid()); err != nil && !os.IsNotExist(err) {
		logx.Debugf("chown %q: %v", path, err)
	}
}
