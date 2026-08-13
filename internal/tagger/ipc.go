//go:build tagger

package tagger

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/monbooru/monbooru/internal/logx"
)

// ipcMethod identifies the IPC call's intent so a single
// request/response struct covers Run, Status, ReleaseIdle, ReleaseAll
// without separate wire types per method.
type ipcMethod uint8

const (
	ipcMethodRun ipcMethod = iota + 1
	ipcMethodStatus
	ipcMethodReleaseIdle
	ipcMethodReleaseAll
	ipcMethodShutdown
)

// ipcRequest is the parent → child envelope. Only one of the
// method-specific fields is populated per call.
type ipcRequest struct {
	Method    ipcMethod
	Run       *RunRequest
	IdleAfter time.Duration
}

// ipcTokenEnv names the environment variable carrying the per-spawn
// handshake secret. The environment rather than argv: /proc/<pid>/environ
// is owner-only where /proc/<pid>/cmdline is world-readable, and the
// first frame the parent sends after the handshake is the whole config.
const ipcTokenEnv = "MONBOORU_TAGGER_IPC_TOKEN"

// ipcHandshakeTimeout bounds how long a connector may sit on the
// accepted socket before greeting, so a peer that never speaks cannot
// hold the listener.
const ipcHandshakeTimeout = 5 * time.Second

// ipcHello is the child's first frame. The listener is a loopback TCP
// port any local process can reach, so the parent drops every
// connection that cannot echo the secret it spawned the child with
// rather than handing the winner of a race the config.
type ipcHello struct{ Token string }

// ipcResponse is the child → parent envelope. Errors travel as a
// non-empty Err string so the response decode never fails on a normal
// failure. Stream=true marks a non-terminal progress frame that
// carries WorkerIdx + Msg; the receiver keeps reading until a frame
// with Stream=false arrives.
type ipcResponse struct {
	Stream    bool
	WorkerIdx int
	Msg       string
	Run       *RunResponse
	Status    *CacheStatus
	Released  bool
	Err       string
}

// writeFrame encodes payload as gob, prefixes a uint32 length, and
// writes both atomically. Returns the count of bytes the caller has
// successfully placed on the wire so partial-write errors can
// distinguish "the peer never saw this" from "the peer saw a partial
// payload".
func writeFrame(w io.Writer, payload any) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(payload); err != nil {
		return fmt.Errorf("gob encode: %w", err)
	}
	body := buf.Bytes()
	if len(body) > int(^uint32(0)) {
		return fmt.Errorf("ipc frame too large: %d bytes", len(body))
	}
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(body)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// maxFrameBytes caps the body length readFrame is willing to allocate
// in one go. The largest realistic payload is a batch response carrying
// merged tag maps, well under a megabyte; 64 MiB leaves headroom for
// future protocol additions while bounding the blast of a corrupted
// header that decodes as a multi-GB length.
const maxFrameBytes uint32 = 64 << 20

// readFrame reads a uint32 length followed by that many bytes of gob,
// decoding into dst. Returns io.EOF when the peer closed cleanly
// before sending another frame.
func readFrame(r io.Reader, dst any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrameBytes {
		return fmt.Errorf("frame body %d bytes exceeds cap %d", n, maxFrameBytes)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("read frame body: %w", err)
	}
	return gob.NewDecoder(bytes.NewReader(body)).Decode(dst)
}

// ipcBackend is the parent-side Backend that talks to a child
// monbooru process over a TCP loopback socket. It supervises the
// child: spawn on demand, terminate on ReleaseAll / ReleaseIdle, log
// abnormal exits, restart on the next Run.
//
// The child runs the same monbooru binary with the
// `tagger-worker --addr=<host:port>` argv tail; on exit (graceful
// shutdown or crash) the kernel reclaims its CUDA libraries and
// primary context, which is the whole point of the subprocess split.
type ipcBackend struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	conn net.Conn
	// inFlight gates Run vs supervisor teardown so a shutdown never
	// races a request in progress.
	inFlight atomic.Int32
	// childPID mirrors b.cmd.Process.Pid for callers that can't take
	// b.mu (Stats panel hits WorkerPID mid-batch; the IPC streaming
	// loop holds the mutex for the whole run).
	childPID atomic.Int32
	// runProvider records the in-flight Run's Provider so a Status
	// call mid-batch reports the right mode even when the cached
	// snapshot is stale (e.g. just after an execution_provider toggle
	// where the previous snapshot is from the CPU session).
	runProvider atomic.Value
	// lastStatus caches the most recent Status response so a Status
	// call during an in-flight Run can serve from cache instead of
	// queueing behind the long-running IPC frame loop.
	lastStatus atomic.Pointer[CacheStatus]
}

// newIPCBackend constructs the parent-side IPC backend. The child is
// spawned lazily on the first Run; constructor errors are limited to
// process-environment problems (cannot determine own path).
func newIPCBackend() (*ipcBackend, error) {
	return &ipcBackend{}, nil
}

// ensureRunning starts the child if it isn't alive yet. Caller must
// hold b.mu.
func (b *ipcBackend) ensureRunning() error {
	if b.cmd != nil && b.conn != nil {
		return nil
	}
	token, err := newIPCToken()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen tcp: %w", err)
	}
	defer func() { _ = listener.Close() }()
	addr := listener.Addr().String()

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable: %w", err)
	}
	cmd := exec.Command(exe, "tagger-worker", "--addr="+addr)
	cmd.Env = append(os.Environ(), ipcTokenEnv+"="+token)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn tagger-worker: %w", err)
	}
	logx.Infof("tagger-worker: spawned pid=%d addr=%s", cmd.Process.Pid, addr)

	// Accept the child's connection with a bounded wait so a child
	// that crashes before connecting (e.g. missing libonnxruntime)
	// surfaces a clear error instead of hanging the parent.
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				accepted <- acceptResult{nil, err}
				return
			}
			if !ipcHandshakeOK(c, token) {
				logx.Warnf("tagger-worker: dropped a connection that failed the handshake")
				_ = c.Close()
				continue
			}
			accepted <- acceptResult{c, nil}
			return
		}
	}()

	select {
	case res := <-accepted:
		if res.err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return fmt.Errorf("accept tagger-worker connection: %w", res.err)
		}
		b.cmd = cmd
		b.conn = res.conn
		b.childPID.Store(int32(cmd.Process.Pid))
		return nil
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return errors.New("tagger-worker did not connect within 15s")
	}
}

// newIPCToken mints the per-spawn handshake secret.
func newIPCToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("tagger ipc token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ipcHandshakeOK reads the greeting frame and reports whether it
// carries the expected secret.
func ipcHandshakeOK(c net.Conn, want string) bool {
	_ = c.SetReadDeadline(time.Now().Add(ipcHandshakeTimeout))
	var hello ipcHello
	if err := readFrame(c, &hello); err != nil {
		return false
	}
	_ = c.SetReadDeadline(time.Time{})
	return subtle.ConstantTimeCompare([]byte(hello.Token), []byte(want)) == 1
}

// terminate forcibly stops the child for abnormal or crash paths. It
// sends SIGTERM, waits 5s, then SIGKILL if necessary. Caller must hold
// b.mu.
func (b *ipcBackend) terminate() {
	if b.cmd == nil {
		return
	}
	if b.cmd.Process != nil {
		_ = b.cmd.Process.Signal(syscall.SIGTERM)
	}
	b.reapLocked(true, "terminated")
}

// reapLocked waits for the child to exit, killing it after 5s, then clears
// the handles it left behind. dropConnFirst closes the socket before the
// wait, which is what an unresponsive child needs; a graceful shutdown keeps
// it open until the child has acknowledged and exited. verb names the outcome
// in the log line. Caller must hold b.mu.
func (b *ipcBackend) reapLocked(dropConnFirst bool, verb string) {
	if dropConnFirst && b.conn != nil {
		_ = b.conn.Close()
		b.conn = nil
	}
	done := make(chan struct{})
	go func() {
		if b.cmd != nil {
			_ = b.cmd.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if b.cmd != nil && b.cmd.Process != nil {
			_ = b.cmd.Process.Kill()
		}
		<-done
	}
	if b.conn != nil {
		_ = b.conn.Close()
		b.conn = nil
	}
	pid := 0
	if b.cmd != nil && b.cmd.ProcessState != nil {
		pid = b.cmd.ProcessState.Pid()
	}
	b.cmd = nil
	b.childPID.Store(0)
	b.lastStatus.Store(nil)
	logx.Infof("tagger-worker: %s pid=%d", verb, pid)
}

// call sends a request and reads the terminal response. Caller must
// hold b.mu. A watcher goroutine closes b.conn when ctx fires so a
// wedged worker can't hang the parent indefinitely; the resulting
// wire error runs terminate() which SIGTERMs the child. The caller
// gets ctx.Err on cancellation rather than the wrapped read error.
// Intermediate Stream=true frames are forwarded to onProgress (when
// non-nil); the loop returns on the first Stream=false frame.
func (b *ipcBackend) call(ctx context.Context, req ipcRequest, onProgress func(int, string)) (ipcResponse, error) {
	// Capture the handle under the caller's lock so the watcher never reads
	// b.conn while terminate() (also under b.mu) sets it to nil.
	conn := b.conn
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			if conn != nil {
				_ = conn.Close()
			}
		case <-done:
		}
	}()

	if err := writeFrame(conn, req); err != nil {
		b.terminate()
		if ctx.Err() != nil {
			return ipcResponse{}, ctx.Err()
		}
		return ipcResponse{}, fmt.Errorf("ipc write: %w", err)
	}
	for {
		var resp ipcResponse
		if err := readFrame(conn, &resp); err != nil {
			b.terminate()
			if ctx.Err() != nil {
				return ipcResponse{}, ctx.Err()
			}
			return ipcResponse{}, fmt.Errorf("ipc read: %w", err)
		}
		if resp.Stream {
			if onProgress != nil {
				onProgress(resp.WorkerIdx, resp.Msg)
			}
			continue
		}
		return resp, nil
	}
}

// shortCallTimeout bounds Status / ReleaseIdle / ReleaseAll: long
// enough that a healthy worker's response (microseconds for Status,
// up to a few seconds for a teardown that includes mallocTrim) never
// trips it, short enough that a wedged worker doesn't hang the
// reclaim ticker or server shutdown.
const shortCallTimeout = 30 * time.Second

// Run sends the batch to the child, forwards every Stream=true
// progress frame it emits through req.OnProgress, and returns the
// terminal response. ctx cancellation unwedges the IPC reader and
// returns ctx.Err to the caller; the next Run respawns.
func (b *ipcBackend) Run(ctx context.Context, req RunRequest) (RunResponse, error) {
	// gob can't encode the OnProgress func, so we strip it from the
	// wire payload and forward progress over the response stream
	// instead.
	wire := req
	wire.OnProgress = nil
	b.inFlight.Add(1)
	b.runProvider.Store(req.Provider)
	defer b.inFlight.Add(-1)

	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureRunning(); err != nil {
		return RunResponse{}, err
	}

	resp, err := b.call(ctx, ipcRequest{Method: ipcMethodRun, Run: &wire}, req.OnProgress)
	if err != nil {
		return RunResponse{}, err
	}
	if resp.Err != "" {
		return RunResponse{}, errors.New(resp.Err)
	}
	if resp.Run == nil {
		return RunResponse{}, errors.New("tagger-worker returned empty response")
	}
	return *resp.Run, nil
}

// Status returns the child's cache state. While a Run is in flight,
// b.mu is held by the streaming call loop and an IPC round-trip would
// queue behind the whole batch. Short-circuit on inFlight > 0 with the
// last cached snapshot, overlaying InUse=true, so the Stats panel
// renders without waiting on the worker.
func (b *ipcBackend) Status() CacheStatus {
	if b.inFlight.Load() > 0 {
		provider, _ := b.runProvider.Load().(string)
		provider = cmp.Or(provider, "cpu")
		if cached := b.lastStatus.Load(); cached != nil {
			snap := *cached
			snap.InUse = true
			snap.Provider = provider
			return snap
		}
		return CacheStatus{Loaded: true, InUse: true, Provider: provider}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return CacheStatus{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), shortCallTimeout)
	defer cancel()
	resp, err := b.call(ctx, ipcRequest{Method: ipcMethodStatus}, nil)
	if err != nil {
		return CacheStatus{}
	}
	if resp.Status == nil {
		return CacheStatus{}
	}
	snap := *resp.Status
	b.lastStatus.Store(&snap)
	return snap
}

// ReleaseIdle asks the child to teardown if it has been idle long
// enough. When the child reports it tore down, parent SIGTERMs to
// reclaim the CUDA libraries it loaded; the next Run respawns it.
func (b *ipcBackend) ReleaseIdle(after time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), shortCallTimeout)
	defer cancel()
	resp, err := b.call(ctx, ipcRequest{Method: ipcMethodReleaseIdle, IdleAfter: after}, nil)
	if err != nil {
		return false
	}
	if resp.Released {
		b.terminate()
	}
	return resp.Released
}

// ReleaseAll asks the child to shut down gracefully, waits up to 5s
// for the process to exit, and kills it if it is still alive. The
// next Run respawns.
func (b *ipcBackend) ReleaseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), shortCallTimeout)
	defer cancel()
	resp, err := b.call(ctx, ipcRequest{Method: ipcMethodShutdown}, nil)
	if err != nil {
		// call already invoked terminate() on wire failure.
		return
	}
	if resp.Err != "" {
		b.terminate()
		return
	}

	// The child keeps the socket until it has acknowledged the shutdown and
	// exited, so the drop happens after the wait.
	b.reapLocked(false, "released")
}

// WorkerPID returns the live child's PID, or (0, false) when no
// worker is currently running. Reads childPID atomically so the Stats
// panel can sample /proc/<pid>/smaps even while an autotag batch
// holds b.mu in the streaming Run loop.
func (b *ipcBackend) WorkerPID() (int, bool) {
	pid := b.childPID.Load()
	if pid == 0 {
		return 0, false
	}
	return int(pid), true
}
