package jobs

import (
	"sync"
	"testing"
)

func TestStart_ReturnsErrorIfRunning(t *testing.T) {
	m := NewManager()

	if err := m.Start("sync"); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	if err := m.Start("autotag"); err != ErrJobRunning {
		t.Errorf("expected ErrJobRunning, got %v", err)
	}
}

func TestUpdate_SetsFields(t *testing.T) {
	m := NewManager()
	m.Start("sync")
	m.Update(5, 10, "processing…")

	state := m.Get()
	if state.Processed != 5 {
		t.Errorf("Processed = %d, want 5", state.Processed)
	}
	if state.Total != 10 {
		t.Errorf("Total = %d, want 10", state.Total)
	}
	if state.Message != "processing…" {
		t.Errorf("Message = %q", state.Message)
	}
}

func TestComplete_SetsFinishedAt(t *testing.T) {
	m := NewManager()
	m.Start("sync")
	m.Complete("done: 5 added")

	state := m.Get()
	if state.Running {
		t.Error("Running should be false after Complete")
	}
	if state.FinishedAt == nil {
		t.Error("FinishedAt should be set")
	}
	if state.Summary != "done: 5 added" {
		t.Errorf("Summary = %q", state.Summary)
	}
}

func TestConcurrentUpdate_NoRace(t *testing.T) {
	m := NewManager()
	m.Start("autotag")

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			m.Update(i, 100, "working")
		}()
	}
	wg.Wait()
	m.Complete("done")
}

func TestFail_SetsError(t *testing.T) {
	m := NewManager()
	m.Start("sync")
	m.Fail("something went wrong")

	state := m.Get()
	if state.Running {
		t.Error("Running should be false after Fail")
	}
	if state.Error != "something went wrong" {
		t.Errorf("Error = %q", state.Error)
	}
}

func TestDismiss_ClearsState(t *testing.T) {
	m := NewManager()
	m.Start("sync")
	m.Complete("done")
	m.Dismiss()

	if m.Get() != nil {
		t.Error("state should be nil after Dismiss")
	}
}

func TestDismiss_NopWhenRunning(t *testing.T) {
	m := NewManager()
	m.Start("sync")
	m.Dismiss() // should not clear running job

	state := m.Get()
	if state == nil || !state.Running {
		t.Error("Dismiss should not clear a running job")
	}
}

func TestSetWatcherMessage(t *testing.T) {
	m := NewManager()
	m.SetWatcherMessage("added file.png")

	state := m.Get()
	if state == nil {
		t.Fatal("state should not be nil")
	}
	if state.JobType != "watcher" {
		t.Errorf("JobType = %q, want watcher", state.JobType)
	}
	if state.Summary != "added file.png" {
		t.Errorf("Summary = %q", state.Summary)
	}
}

func TestCancel_FiresContext(t *testing.T) {
	m := NewManager()
	m.Start("autotag")
	ctx := m.Context()
	if ctx.Err() != nil {
		t.Fatal("context should be live right after Start")
	}
	m.Cancel()
	if ctx.Err() == nil {
		t.Error("context should be cancelled after Cancel")
	}
}

func TestSetWatcherMessage_NopWhenRunning(t *testing.T) {
	m := NewManager()
	m.Start("sync")
	m.SetWatcherMessage("should be ignored")

	state := m.Get()
	if state.Summary == "should be ignored" {
		t.Error("watcher message should not override running job")
	}
}
