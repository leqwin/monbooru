package jobs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/leqwin/monbooru/internal/models"
)

// ErrJobRunning is returned when a job is already running.
var ErrJobRunning = errors.New("a job is already running")

// Manager is a thread-safe singleton job state machine.
// Only one job (sync or autotag) may run at a time.
type Manager struct {
	mu     sync.Mutex
	state  *models.JobState
	timer  *time.Timer // auto-dismiss timer
	ctx    context.Context
	cancel context.CancelFunc
	// scheduleHeld blocks user-Start while a scheduler run is active so
	// the scheduler's per-phase Start/Complete pairs can't race against
	// a user job that slips into a phase boundary.
	scheduleHeld bool
	// viewed is set by MarkViewed when a completed job state has been
	// rendered to at least one client, so the auto-dismiss can shorten
	// from the 30s "no one is looking" cap to a few seconds.
	viewed bool
}

// NewManager returns a new Manager with no active job.
func NewManager() *Manager {
	return &Manager{}
}

// Start begins a new job. Returns ErrJobRunning if a job is already active
// or a scheduler run is in progress.
func (m *Manager) Start(jobType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scheduleHeld {
		return ErrJobRunning
	}
	return m.startLocked(jobType)
}

// StartScheduled is the scheduler's entry point. It bypasses the
// scheduleHeld guard (which would otherwise block the scheduler's own
// per-phase Start calls) but still refuses if another job is already
// running. Pair with the usual Complete/Fail; the schedule reservation
// is owned separately by BeginSchedule/EndSchedule.
func (m *Manager) StartScheduled(jobType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked(jobType)
}

// startLocked registers a new job; caller must hold m.mu.
func (m *Manager) startLocked(jobType string) error {
	if m.state != nil && m.state.Running {
		return ErrJobRunning
	}
	// Cancel any pending auto-dismiss timer from a previous completed job.
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	m.viewed = false
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.state = &models.JobState{
		Running:   true,
		JobType:   jobType,
		StartedAt: time.Now().UTC(),
		Message:   "Starting…",
	}
	return nil
}

// BeginSchedule reserves the manager for an in-progress scheduler run.
// While the reservation is held, user-facing Start() calls return
// ErrJobRunning so external triggers can't slip into a phase gap. Returns
// ErrJobRunning if a job is currently running or another scheduler run is
// already in progress. Pair with EndSchedule via defer.
func (m *Manager) BeginSchedule() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scheduleHeld {
		return ErrJobRunning
	}
	if m.state != nil && m.state.Running {
		return ErrJobRunning
	}
	m.scheduleHeld = true
	return nil
}

// EndSchedule releases the schedule reservation set by BeginSchedule.
func (m *Manager) EndSchedule() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scheduleHeld = false
}

// Context returns the cancellation context for the running job. Callers pass
// this into long-running work (tagger.RunWithTaggers, etc.) so the Cancel
// endpoint can interrupt it. Returns a background context when no job runs.
func (m *Manager) Context() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil {
		return context.Background()
	}
	return m.ctx
}

// Cancel signals the running job's context to abort. It is a no-op when no
// job is running; workers observe ctx.Done() and wrap up via Complete/Fail.
func (m *Manager) Cancel() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
}

// Update sets the processed count and message.
func (m *Manager) Update(processed, total int, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == nil {
		return
	}
	m.state.Processed = processed
	m.state.Total = total
	m.state.Message = message
}

// Complete marks the job as done with a summary.
func (m *Manager) Complete(summary string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == nil {
		return
	}
	now := time.Now().UTC()
	m.state.Running = false
	m.state.FinishedAt = &now
	m.state.Summary = summary
	m.state.Message = ""
	m.scheduleAutoDismiss()
}

// Fail marks the job as failed with an error message.
func (m *Manager) Fail(errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == nil {
		return
	}
	now := time.Now().UTC()
	m.state.Running = false
	m.state.FinishedAt = &now
	m.state.Error = errMsg
	m.scheduleAutoDismiss()
}

// Get returns a copy of the current job state (may be nil).
func (m *Manager) Get() *models.JobState {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == nil {
		return nil
	}
	copy := *m.state
	return &copy
}

// IsRunning returns true if a job is currently running or a scheduler run
// holds the manager. Callers that gate user-triggered work on the absence
// of a running job (gallery switch, settings tagger toggle, ...) get the
// same protection during scheduled maintenance.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scheduleHeld {
		return true
	}
	return m.state != nil && m.state.Running
}

// MarkViewed is called when a client renders the completed/failed job state.
// It shortens the auto-dismiss timer to a few seconds so the completion
// flash doesn't linger for the full 30s cap across page navigations once
// at least one client has seen it. The 30s cap stays in place for jobs
// that finish while no client is looking (e.g. an unattended browser).
func (m *Manager) MarkViewed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil || m.state.Running || m.viewed {
		return
	}
	m.viewed = true
	if m.timer != nil {
		m.timer.Stop()
	}
	m.timer = time.AfterFunc(6*time.Second, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.state == nil || m.state.Running {
			return
		}
		m.ctx, m.cancel = nil, nil
		m.state = nil
		m.timer = nil
		m.viewed = false
	})
}

// Dismiss clears the completed/failed job state so the status widget goes idle.
func (m *Manager) Dismiss() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil || m.state.Running {
		return
	}
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	m.ctx, m.cancel = nil, nil
	m.state = nil
	m.viewed = false
}

// SetWatcherMessage surfaces a transient watcher notification. When idle it
// becomes the status bar summary; when a job is running it only bumps the
// WatcherNotices counter so the client refreshes the gallery grid without
// overwriting the running job's progress line.
func (m *Manager) SetWatcherMessage(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != nil && m.state.Running {
		m.state.WatcherNotices++
		return
	}
	now := time.Now().UTC()
	m.state = &models.JobState{
		Running:    false,
		JobType:    "watcher",
		Summary:    msg,
		FinishedAt: &now,
	}
	m.scheduleAutoDismiss()
}

// scheduleAutoDismiss starts a 30-second timer that auto-dismisses the completed state.
// Must be called with m.mu held.
func (m *Manager) scheduleAutoDismiss() {
	if m.timer != nil {
		m.timer.Stop()
	}
	m.timer = time.AfterFunc(30*time.Second, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.state == nil || m.state.Running {
			return
		}
		m.ctx, m.cancel = nil, nil
		m.state = nil
		m.timer = nil
	})
}
