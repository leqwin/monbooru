// Monbooru is a Linux-only deployment; path handling assumes forward slashes.
package gallery

import (
	"os"

	"github.com/leqwin/monbooru/internal/logx"
)

// ClaimOwnership chowns path to the current process UID/GID so later
// rename/delete operations don't hit EACCES on files originally written
// by a different user (rsynced from another machine, rootless Podman
// where the container's UID 0 doesn't match the bind mount's owner).
//
// Best-effort: failures log at debug and never abort the caller. ENOENT
// is silenced because callers race deletions and watcher events.
func ClaimOwnership(path string) {
	if err := os.Chown(path, os.Getuid(), os.Getgid()); err != nil && !os.IsNotExist(err) {
		logx.Debugf("chown %q: %v", path, err)
	}
}
