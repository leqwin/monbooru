//go:build !tagger

package tagger

import (
	"context"
	"errors"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/jobs"
)

// CheckCUDAAvailable always errors here; inference is compiled out.
func CheckCUDAAvailable() error {
	return errors.New("auto-tagger disabled (built without -tags tagger)")
}

func IsAvailable(_ *config.Config) bool { return false }

func buildSupportsInference() bool { return false }

func UnavailableReason(_ *config.Config) string {
	return "inference disabled (built without -tags tagger)"
}

// AvailableTaggers lists every configured tagger as unavailable because
// inference is disabled at build time.
func AvailableTaggers(cfg *config.Config) []TaggerStatus {
	list := DiscoverTaggers(cfg)
	for i := range list {
		list[i].Available = false
		list[i].Reason = "inference disabled (built without -tags tagger)"
	}
	return list
}

// RunWithTaggers is the no-op stub matching the tagger build signature.
func RunWithTaggers(_ context.Context, _ *db.DB, _ *config.Config, _ []int64, _ []TaggerStatus, _ *jobs.Manager, _ bool) error {
	return nil
}
