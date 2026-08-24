// Package plugins supervises the child processes monbooru launches for
// operator-installed plugins. Monbooru is a launcher here, not a package
// manager: it starts, restarts and stops a binary the operator put on disk
// and named in a folder manifest, and never downloads or installs
// anything.
//
// It holds no HTTP concern - the routes, the button rendering and the
// config reads stay in the web layer, which hands this the launch line and
// the address a child calls back on.
package plugins

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/logx"
)

// Launch is everything the supervisor needs to run one plugin: its name
// and the command line its manifest declared.
type Launch struct {
	Name    string
	Command string
	Args    []string
	Dir     string
}

// Supervisor owns the running children. CallbackURL answers the address a
// child is told to reach monbooru on, and Done closes at shutdown so a
// supervisor parked on its restart backoff lets go.
type Supervisor struct {
	CallbackURL func() string
	Done        <-chan struct{}

	mu      sync.Mutex
	managed map[string]*managedPlugin
}

// New returns a supervisor with nothing running yet.
func New(callbackURL func() string, done <-chan struct{}) *Supervisor {
	return &Supervisor{
		CallbackURL: callbackURL,
		Done:        done,
		managed:     map[string]*managedPlugin{},
	}
}

const (
	// managedStopGrace is how long a managed plugin gets to exit after its
	// stdin closes before it is killed. No signals: this has to work the
	// same on Windows.
	managedStopGrace = 5 * time.Second
	// managedHealthyAfter is how long a run must last to count as healthy,
	// clearing the restart counter so an occasional crash doesn't ratchet
	// the backoff up forever.
	managedHealthyAfter = 30 * time.Second
	// BackoffMin is the first restart delay after a crash; it doubles up
	// to managedBackoffMax. Exported so a caller waiting on a restart
	// knows how long that is.
	BackoffMin        = time.Second
	managedBackoffMax = time.Minute
	// LogLineMax caps one drained output line. A traceback carrying a
	// large repr or a JSON dump goes past the scanner's own 64 KiB default,
	// which ends the drain while the child is still writing. Exported
	// because it is a behavioural boundary, not a tuning knob: past it the
	// rest of the line is discarded rather than logged.
	LogLineMax = 1 << 20
)

// managedPlugin supervises one operator-launched plugin process.
type managedPlugin struct {
	name    string
	command string
	args    []string
	dir     string
	env     []string

	mu          sync.Mutex
	proc        *exec.Cmd
	stdin       io.WriteCloser
	running     bool
	supervising bool
	stopped     bool
	restarts    int
	stop        chan struct{}
}

// Start begins (or resumes) supervision of one plugin.
func (s *Supervisor) Start(p Launch) {
	s.mu.Lock()
	m, ok := s.managed[p.Name]
	if !ok {
		m = &managedPlugin{name: p.Name}
		s.managed[p.Name] = m
	}
	s.mu.Unlock()

	env := []string{"MONBOORU_URL=" + s.CallbackURL()}

	m.mu.Lock()
	m.command, m.args = p.Command, p.Args
	m.dir, m.env = p.Dir, env
	m.stopped, m.restarts = false, 0
	// A fresh channel even when a supervisor is already running: the stop
	// this Enable undoes closed the old one, and a supervisor still inside
	// run() would read that closed channel at its next backoff and quit,
	// leaving the plugin enabled with nothing running. It also keeps the
	// next Disable from closing an already-closed channel.
	m.stop = make(chan struct{})
	if m.supervising {
		m.mu.Unlock()
		return
	}
	m.supervising = true
	m.mu.Unlock()
	go s.supervise(m)
}

// Stop asks a plugin to exit and ends its supervision, so a stopped
// plugin stays stopped instead of being restarted by the crash handler.
func (s *Supervisor) Stop(name string) {
	s.mu.Lock()
	m := s.managed[name]
	s.mu.Unlock()
	if m == nil {
		return
	}
	m.mu.Lock()
	if !m.stopped {
		m.stopped = true
		close(m.stop)
	}
	m.mu.Unlock()
	m.terminate()
}

// StopAll tears down every managed plugin, for server shutdown.
func (s *Supervisor) StopAll() {
	s.mu.Lock()
	names := make([]string, 0, len(s.managed))
	for name := range s.managed {
		names = append(names, name)
	}
	s.mu.Unlock()
	for _, name := range names {
		s.Stop(name)
	}
}

// supervise runs one plugin until the operator stops it or the server
// shuts down, restarting it with capped backoff after a crash.
func (s *Supervisor) supervise(m *managedPlugin) {
	defer func() {
		m.mu.Lock()
		m.supervising = false
		m.mu.Unlock()
	}()
	for {
		// The backoff select can lose a stop: both arms can be ready at once,
		// and an Enable swaps in a channel a later Disable closes instead of
		// the one this loop is parked on. The flag is the authority, so it is
		// read again here rather than only after a run.
		m.mu.Lock()
		stopped := m.stopped
		m.mu.Unlock()
		if stopped {
			return
		}
		started := time.Now()
		if err := m.run(); err != nil {
			logx.Errorf("plugin %s: %v", m.name, err)
		}
		m.mu.Lock()
		if m.stopped {
			m.mu.Unlock()
			return
		}
		// A run that lasted counts as healthy, so an occasional crash doesn't
		// ratchet the backoff up forever.
		if time.Since(started) >= managedHealthyAfter {
			m.restarts = 0
		}
		m.restarts++
		stop := m.stop
		delay := min(BackoffMin<<min(m.restarts-1, 6), managedBackoffMax)
		m.mu.Unlock()
		logx.Warnf("plugin %s: exited, restarting in %s", m.name, delay)
		select {
		case <-time.After(delay):
		case <-stop:
			// The channel closes on a stop, but the operator may have
			// switched the plugin back on before this read; the flag is
			// what says which, so an Enable inside the stop grace resumes
			// rather than ending supervision.
			m.mu.Lock()
			stopped := m.stopped
			m.mu.Unlock()
			if stopped {
				return
			}
		case <-s.Done:
			return
		}
	}
}

// run starts the process and blocks until it exits, folding its output into
// monbooru's log under the plugin's name.
func (m *managedPlugin) run() error {
	m.mu.Lock()
	cmd := exec.Command(m.command, m.args...)
	cmd.Dir = m.dir
	cmd.Env = append(os.Environ(), m.env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return err
	}
	m.proc, m.stdin, m.running = cmd, stdin, true
	m.mu.Unlock()
	logx.Infof("plugin %s: started (pid %d)", m.name, cmd.Process.Pid)

	// Drain before Wait: it closes the pipe out from under a live read.
	scanner := bufio.NewScanner(out)
	scanner.Buffer(nil, LogLineMax)
	var last string
	for scanner.Scan() {
		last = scanner.Text()
		logx.Infof("plugin %s: %s", m.name, last)
	}
	if scanner.Err() != nil {
		// A line past the cap stops the scan with the child still writing.
		// Wait would then block on a process blocked on a full pipe, and the
		// supervisor would never come back to restart it.
		logx.Warnf("plugin %s: output dropped: %v", m.name, scanner.Err())
		_, _ = io.Copy(io.Discard, out)
	}
	err = cmd.Wait()

	m.mu.Lock()
	m.proc, m.stdin, m.running = nil, nil, false
	m.mu.Unlock()
	// The plugin's own output is info-level, which the default threshold
	// drops; without its last line a crash is an exit status and no reason.
	if err != nil && last != "" {
		return fmt.Errorf("%w: %s", err, last)
	}
	return err
}

// terminate closes the plugin's stdin - the exit signal a managed plugin has
// to honour - and kills it if it is still around after the grace period.
func (m *managedPlugin) terminate() {
	m.mu.Lock()
	cmd, stdin := m.proc, m.stdin
	m.mu.Unlock()
	if cmd == nil {
		return
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	deadline := time.Now().Add(managedStopGrace)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		running := m.running
		m.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
		logx.Warnf("plugin %s: killed after %s", m.name, managedStopGrace)
	}
}

// State reports how a managed plugin's settings row should read.
func (s *Supervisor) State(name string) string {
	s.mu.Lock()
	m := s.managed[name]
	s.mu.Unlock()
	if m == nil {
		return "stopped"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.running:
		return "running"
	case m.stopped || !m.supervising:
		return "stopped"
	case m.restarts > 0:
		return "restarting (" + strconv.Itoa(m.restarts) + ")"
	default:
		return "starting"
	}
}
