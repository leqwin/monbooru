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

// buildSupportsInference reports whether this build can run inference at
// all. Always false in the noop build; true in the tagger build.
func buildSupportsInference() bool { return false }

// UnavailableReason explains why auto-tagging cannot run in this build.
// The string mirrors the one Settings → Auto-Tagger shows so flashes on
// the detail page stay consistent with what the settings page claims.
func UnavailableReason(_ *config.Config) string {
	return "inference disabled (built without -tags tagger)"
}

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
