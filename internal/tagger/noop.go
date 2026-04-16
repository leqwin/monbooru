//go:build !tagger

package tagger

import (
	"context"
	"errors"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/jobs"
)

// CheckCUDAAvailable always errors in the no-tagger build because inference is
// compiled out; the tagger-build variant has the real probe.
func CheckCUDAAvailable() error {
	return errors.New("auto-tagger disabled (built without -tags tagger)")
}

// IsAvailable returns false when built without the tagger build tag.
func IsAvailable(_ *config.Config) bool { return false }

// AvailableTaggers lists the auto-taggers that could run, along with the
// reason each is or isn't active. The noop build reports every configured
// tagger as unavailable because inference is disabled at build time.
func AvailableTaggers(cfg *config.Config) []TaggerStatus {
	list := DiscoverTaggers(cfg)
	for i := range list {
		list[i].Available = false
		list[i].Reason = "inference disabled (built without -tags tagger)"
	}
	return list
}

// RunWithTaggers is a no-op stub matching the tagger-build signature.
func RunWithTaggers(_ context.Context, _ *db.DB, _ *config.Config, _ []int64, _ []TaggerStatus, _ *jobs.Manager, _ bool) error {
	return nil
}
