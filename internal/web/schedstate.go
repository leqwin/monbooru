package web

import (
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/monbooru/monbooru/internal/fsx"
	"github.com/monbooru/monbooru/internal/logx"
)

// scheduleState is what the nightly run leaves behind so a machine that was
// asleep at schedule.time can tell it missed one. It sits beside the config
// rather than in a gallery database, because the schedule is global and
// iterates every gallery, and not in the config itself, because a timestamp
// monbooru rewrites nightly would show up in every diff of the operator's
// own file.
type scheduleState struct {
	LastRun time.Time `toml:"last_scheduled_run"`
}

func (s *Server) scheduleStatePath() string {
	return filepath.Join(filepath.Dir(s.configPath), "state.toml")
}

// lastScheduledRun reads the recorded time. A missing or unparseable file
// reads as "never ran", which is not an error: the worst it costs is one
// catch-up pass.
func (s *Server) lastScheduledRun() time.Time {
	var st scheduleState
	if _, err := toml.DecodeFile(s.scheduleStatePath(), &st); err != nil {
		if !os.IsNotExist(err) {
			logx.Warnf("schedule state: %v", err)
		}
		return time.Time{}
	}
	return st.LastRun
}

func (s *Server) saveScheduledRun(at time.Time) {
	path := s.scheduleStatePath()
	err := fsx.WriteAtomic(path, ".state.toml.*", func(f *os.File) error {
		return toml.NewEncoder(f).Encode(scheduleState{LastRun: at.UTC()})
	})
	if err != nil {
		logx.Warnf("schedule state: %v", err)
	}
}
