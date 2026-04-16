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
}

// NewManager returns a new Manager with no active job.
func NewManager() *Manager {
	return &Manager{}
}

// Start begins a new job. Returns ErrJobRunning if a job is already active.
func (m *Manager) Start(jobType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state != nil && m.state.Running {
		return ErrJobRunning
	}

	// Cancel any pending auto-dismiss timer from a previous completed job.
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}

	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.state = &models.JobState{
		Running:   true,
		JobType:   jobType,
		StartedAt: time.Now().UTC(),
		Message:   "Starting…",
	}
	return nil
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

// IsRunning returns true if a job is currently running.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state != nil && m.state.Running
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
}

// SetWatcherMessage sets a transient watcher notification as the current summary,
// only when no job is running. Used by the filesystem watcher to surface events.
func (m *Manager) SetWatcherMessage(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != nil && m.state.Running {
		return // don't interrupt a running job
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
