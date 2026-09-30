package materialize

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The Supervisor runs materializer jobs in a child process
// (`pad __materialize-worker`, RunWorker) and never in the server's own heap.
//
// Why a child: a crafted ~1 MB Yjs update drove an in-process runner past
// 5 GB, into Go's fatal, uncatchable out-of-memory — it would have taken the
// whole server down. In a child that is one dead worker. Why ONE long-lived
// child: loading the bundle costs ~2.6 s, so a process per job would spend
// more time starting than working. Why KILL on the deadline: goja's Interrupt
// cannot preempt a single long native call, so the soft deadline inside the
// child (timeout_ms) is not a bound; the process kill is.
//
// It is not wired to anything yet (TASK-2198 U4 does that). Callers are
// background workers: Materialize blocks for as long as the job, a respawn
// backoff, or waiting behind another job takes, bounded only by ctx.

var (
	// ErrDeadline: the job ran past its deadline. When the child's own
	// interrupt stopped it the child survives; otherwise the child was killed.
	ErrDeadline = errors.New("materialize: job deadline exceeded")
	// ErrChildDied: the worker process died, or was killed, while it held
	// the job. The job is NOT retried here: the input may be what killed it,
	// and retry budgets on untrusted input belong to the caller.
	ErrChildDied = errors.New("materialize: worker process died")
	// ErrMemoryLimit: the worker died on its memory cap. It wraps
	// ErrChildDied as well. Detected from the Go runtime's out-of-memory
	// report on the child's stderr (Linux, Windows) or from the RSS
	// watchdog's own kill (macOS); a death it cannot attribute is plain
	// ErrChildDied.
	ErrMemoryLimit = errors.New("materialize: worker exceeded its memory limit")
	// ErrProtocol: the worker answered with something that is not a valid
	// response to the request. The worker is killed.
	ErrProtocol = errors.New("materialize: worker protocol error")
	// ErrStart: the worker could not be started, capped or made ready.
	ErrStart = errors.New("materialize: worker failed to start")
	// ErrJobFailed: the worker answered the job with an error (a JavaScript
	// exception, a malformed row set). The worker is healthy.
	ErrJobFailed = errors.New("materialize: job failed")
	// ErrClosed: the Supervisor is closed.
	ErrClosed = errors.New("materialize: supervisor closed")
	// ErrJobTooLarge: the encoded request exceeds MaxFrameBytes. Nothing was
	// sent.
	ErrJobTooLarge = errors.New("materialize: job exceeds the worker frame limit")
)

// WorkerCommand is the argv[1] that runs the worker (cmd/pad/cmd_materialize.go).
const WorkerCommand = "__materialize-worker"

// SupervisorConfig configures a Supervisor. The zero value is usable.
type SupervisorConfig struct {
	// Timeout is the per-job hard deadline (0: DefaultTimeout), clamped to
	// [MinTimeout, MaxTimeout]. The child is asked to stop itself a little
	// earlier (softTimeout).
	Timeout time.Duration
	// MemLimit is the worker's memory cap in bytes (0: DefaultMemLimit),
	// clamped to [MinMemLimit, MaxMemLimit]. What it measures is per OS; see
	// memCapMechanism.
	MemLimit uint64
	// Command builds the worker's command. Nil runs this same binary with
	// WorkerCommand. Stdin, Stdout and Stderr must be left unset.
	Command func() (*exec.Cmd, error)
	// Logger receives the start, stop and stderr lines. Nil: slog.Default().
	Logger *slog.Logger
	// StartTimeout bounds spawn-to-ready (the bundle load). 0: 60s.
	StartTimeout time.Duration
	// Respawn backoff after a death: BackoffBase (0: 1s), doubling per
	// consecutive death, capped at BackoffMax (0: 5 min). A child that lived
	// HealthyStretch (0: 1 min) resets the streak when it dies.
	BackoffBase, BackoffMax, HealthyStretch time.Duration

	// Test seams.
	now           func() time.Time
	sleep         func(ctx context.Context, d time.Duration) error
	capOverride   *memCapImpl
	watchInterval time.Duration
	onSpawn       func(pid int)
}

// memCapImpl is one OS's way of capping the child; memcap_<os>.go defines
// platformMemCap.
type memCapImpl struct {
	// mechanism names it in the start log line.
	mechanism string
	// prepare adjusts the command before Start (nil: nothing).
	prepare func(cmd *exec.Cmd)
	// apply caps the started child before it is sent anything. Its release
	// runs after the child is reaped. Nil: no cap.
	apply func(cmd *exec.Cmd, limit uint64) (release func(), err error)
	// rss, when set, makes the parent poll the child's resident set every
	// watchInterval and kill it above the limit.
	rss func(pid int) (uint64, error)
}

// Supervisor owns at most one worker process and runs jobs through it one at
// a time. Safe for concurrent use.
type Supervisor struct {
	cfg      SupervisorConfig
	log      *slog.Logger
	timeout  time.Duration
	memLimit uint64
	cap      memCapImpl

	slot chan struct{} // held for a whole job: the child is serial

	mu        sync.Mutex
	closed    bool
	child     *child
	nextID    uint64
	streak    int       // consecutive deaths
	nextSpawn time.Time // no spawn before this
}

// NewSupervisor builds a Supervisor. It starts no process: the first job does.
func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Command == nil {
		cfg.Command = defaultWorkerCommand
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 60 * time.Second
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 5 * time.Minute
	}
	if cfg.HealthyStretch <= 0 {
		cfg.HealthyStretch = time.Minute
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.sleep == nil {
		cfg.sleep = sleepCtx
	}
	if cfg.watchInterval <= 0 {
		cfg.watchInterval = 100 * time.Millisecond
	}
	s := &Supervisor{cfg: cfg, log: cfg.Logger, slot: make(chan struct{}, 1), cap: platformMemCap}
	if cfg.capOverride != nil {
		s.cap = *cfg.capOverride
	}
	var clamped bool
	if s.timeout, clamped = effectiveTimeout(cfg.Timeout); clamped {
		s.log.Warn("materialize: "+EnvTimeout+" out of range; clamped",
			"requested", cfg.Timeout.String(), "effective", s.timeout.String(),
			"min", MinTimeout.String(), "max", MaxTimeout.String())
	}
	if s.memLimit, clamped = effectiveMemLimit(cfg.MemLimit); clamped {
		s.log.Warn("materialize: "+EnvMemLimit+" out of range; clamped",
			"requested", formatBytes(cfg.MemLimit), "effective", formatBytes(s.memLimit),
			"min", formatBytes(MinMemLimit), "max", formatBytes(MaxMemLimit))
	}
	return s
}

func defaultWorkerCommand() (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(exe, WorkerCommand), nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// softTimeout is the deadline the child enforces on itself with goja's
// Interrupt: a tenth short of the hard one (at least 50 ms short), so a job
// the interrupt CAN stop fails without costing a respawn.
func softTimeout(hard time.Duration) time.Duration {
	margin := hard / 10
	if margin < 50*time.Millisecond {
		margin = 50 * time.Millisecond
	}
	return hard - margin
}

// Materialize runs job in the worker and returns its markdown. It starts the
// worker if none is running (waiting out any respawn backoff first), and
// waits behind a job already running. The job is never retried: an error
// from a dead or killed worker is returned as is (see the Err values).
//
// When ctx ends while the job runs, the worker is killed (it cannot be told
// to drop a job) and ctx's error is returned, wrapped.
func (s *Supervisor) Materialize(ctx context.Context, job Job) (string, error) {
	select {
	case s.slot <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.slot }()

	c, err := s.ensureChild(ctx)
	if err != nil {
		return "", err
	}
	return s.run(ctx, c, job)
}

// Close kills the worker, if any, and refuses every later job with ErrClosed.
// Idempotent. A job running when Close is called fails with ErrClosed.
func (s *Supervisor) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	c := s.child
	s.child = nil
	s.mu.Unlock()
	if c != nil {
		c.kill("closed")
		c.awaitExit()
		s.logStop(c, "closed", 0)
	}
	return nil
}

// ---------------------------------------------------------------------------
// one worker process

type child struct {
	cmd     *exec.Cmd
	pid     int
	stdin   *os.File
	started time.Time
	jobs    atomic.Int64

	frames  chan []byte   // responses, in order
	readErr error         // set before readEnd closes
	readEnd chan struct{} // reader stopped (EOF, error, or quit)
	exited  chan struct{} // reaped; waitErr set
	waitErr error
	quit    chan struct{} // closed by kill

	killOnce   sync.Once
	killReason atomic.Value // string: why WE killed it
	stderr     *stderrLog
}

func (c *child) kill(reason string) {
	c.killOnce.Do(func() {
		c.killReason.Store(reason)
		close(c.quit)
		_ = c.cmd.Process.Kill()
	})
}

func (c *child) killedFor() string {
	r, _ := c.killReason.Load().(string)
	return r
}

// awaitExit waits for the child to be reaped. Kill is SIGKILL /
// TerminateProcess, so this is prompt; the bound covers a wedged kernel.
func (c *child) awaitExit() bool {
	return c.awaitExitFor(10 * time.Second)
}

func (c *child) awaitExitFor(d time.Duration) bool {
	select {
	case <-c.exited:
		return true
	case <-time.After(d):
		return false
	}
}

// exitString describes how the child ended, once it has been reaped.
func (c *child) exitString() string {
	select {
	case <-c.exited:
		return exitDetail(c.waitErr)
	default:
		return "not reaped"
	}
}

func (s *Supervisor) ensureChild(ctx context.Context) (*child, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	if c := s.child; c != nil {
		s.mu.Unlock()
		return c, nil
	}
	wait := s.nextSpawn.Sub(s.cfg.now())
	s.mu.Unlock()

	if wait > 0 {
		if err := s.cfg.sleep(ctx, wait); err != nil {
			return nil, fmt.Errorf("materialize: waiting out worker respawn backoff: %w", err)
		}
	}
	return s.spawn(ctx)
}

// spawn starts, caps and readies a worker.
//
// The cap is applied after Start returns and BEFORE the parent writes the
// first byte to the child's stdin (the readiness probe). The child cannot do
// anything with untrusted input before it holds a request, so the only work it
// does uncapped is its own startup; see memcap_<os>.go for each OS's window.
func (s *Supervisor) spawn(ctx context.Context) (*child, error) {
	cmd, err := s.cfg.Command()
	if err != nil {
		return nil, s.startFailed(nil, fmt.Errorf("build command: %w", err))
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, s.startFailed(nil, err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, s.startFailed(nil, err)
	}
	c := &child{
		cmd: cmd, stdin: inW,
		frames: make(chan []byte), readEnd: make(chan struct{}),
		exited: make(chan struct{}), quit: make(chan struct{}),
	}
	c.stderr = &stderrLog{log: s.log}
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = c.stderr
	// Wait returns this long after exit even if something still holds the
	// stderr pipe open.
	cmd.WaitDelay = 2 * time.Second
	if s.cap.prepare != nil {
		s.cap.prepare(cmd)
	}
	if err := cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, s.startFailed(nil, err)
	}
	inR.Close()
	outW.Close()
	c.pid = cmd.Process.Pid
	c.started = s.cfg.now()
	c.stderr.setPid(c.pid) // the stderr copier is already running

	var release func()
	if s.cap.apply != nil {
		release, err = s.cap.apply(cmd, s.memLimit)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			inW.Close()
			outR.Close()
			return nil, s.startFailed(c, fmt.Errorf("apply memory cap (%s): %w", s.cap.mechanism, err))
		}
	}

	go func() {
		c.waitErr = cmd.Wait()
		if release != nil {
			release()
		}
		c.stderr.flush()
		inW.Close()
		close(c.exited)
	}()
	go func() {
		defer close(c.readEnd)
		defer outR.Close()
		for {
			p, err := readFrame(outR)
			if err != nil {
				c.readErr = err
				return
			}
			select {
			case c.frames <- p:
			case <-c.quit:
				return
			}
		}
	}()
	if s.cap.rss != nil {
		go s.watchRSS(c)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		c.kill("closed")
		c.awaitExit()
		return nil, ErrClosed
	}
	s.child = c
	s.mu.Unlock()
	if s.cfg.onSpawn != nil {
		s.cfg.onSpawn(c.pid)
	}

	// Readiness: the worker reads nothing until its bundle is loaded, so the
	// first answer marks the end of the load. The probe carries an empty
	// schema version, which the worker refuses (ErrSchemaVersion) without
	// doing any work; the refusal IS the ready signal. Doing this apart from
	// the first job keeps the ~2.6 s load out of that job's deadline.
	id := s.newID()
	probe, err := encodeFrame(WorkerRequest{ID: id})
	if err != nil {
		return nil, s.fail(c, "start", fmt.Errorf("%w: encode probe: %v", ErrStart, err))
	}
	timer := time.NewTimer(s.cfg.StartTimeout)
	defer timer.Stop()
	resp, err := s.exchange(ctx, c, probe, timer.C, "start",
		fmt.Errorf("%w: not ready within %s; worker killed", ErrStart, s.cfg.StartTimeout))
	if err != nil {
		if errors.Is(err, ErrChildDied) && !errors.Is(err, ErrMemoryLimit) {
			err = fmt.Errorf("%w: %w", ErrStart, err)
		}
		return nil, err
	}
	if resp.ID != id || resp.Error == "" {
		return nil, s.fail(c, "protocol", fmt.Errorf("%w: unexpected answer to the readiness probe: %+v", ErrProtocol, resp))
	}

	s.log.Info("materialize worker started",
		"pid", c.pid,
		"ready_ms", s.cfg.now().Sub(c.started).Milliseconds(),
		"timeout", s.timeout.String(),
		"soft_timeout", softTimeout(s.timeout).String(),
		"mem_limit", formatBytes(s.memLimit),
		"mem_limit_bytes", s.memLimit,
		"mem_cap", s.cap.mechanism)
	return c, nil
}

func (s *Supervisor) newID() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return s.nextID
}

// startFailed records a failed start as a death (it counts toward backoff).
func (s *Supervisor) startFailed(c *child, err error) error {
	err = fmt.Errorf("%w: %v", ErrStart, err)
	s.mu.Lock()
	delay := s.recordDeathLocked(c)
	s.mu.Unlock()
	pid := 0
	if c != nil {
		pid = c.pid
	}
	s.log.Warn("materialize worker stopped", "pid", pid, "reason", "start", "detail", err.Error(),
		"respawn_after", delay.String())
	return err
}

// run sends one job and waits for its answer.
func (s *Supervisor) run(ctx context.Context, c *child, job Job) (string, error) {
	hard := s.timeout
	soft := softTimeout(hard)
	// When ctx ends first, have the child stop itself first too, so a job
	// its interrupt can reach does not cost the worker.
	if dl, ok := ctx.Deadline(); ok {
		if rem := dl.Sub(time.Now()); rem < hard {
			soft = softTimeout(rem)
		}
	}
	if soft < time.Millisecond {
		soft = time.Millisecond
	}
	req := WorkerRequest{
		ID:            s.newID(),
		Rows:          make([]string, len(job.Rows)),
		SchemaVersion: job.SchemaVersion,
		LinkIndex:     job.LinkIndex,
		WorkspaceSlug: job.WorkspaceSlug,
		TimeoutMs:     soft.Milliseconds(),
	}
	if req.LinkIndex == nil {
		req.LinkIndex = []LinkEntry{}
	}
	for i, b := range job.Rows {
		req.Rows[i] = base64.StdEncoding.EncodeToString(b)
	}
	frame, err := encodeFrame(req)
	if err != nil {
		return "", err
	}
	c.jobs.Add(1)

	timer := time.NewTimer(hard)
	defer timer.Stop()
	resp, err := s.exchange(ctx, c, frame, timer.C, "deadline",
		fmt.Errorf("%w: %s passed; worker killed", ErrDeadline, hard))
	if err != nil {
		return "", err
	}
	if resp.ID != req.ID {
		return "", s.fail(c, "protocol", fmt.Errorf("%w: response id %d for request %d", ErrProtocol, resp.ID, req.ID))
	}
	switch {
	case resp.Error != "":
		switch {
		case strings.Contains(resp.Error, ErrSchemaVersion.Error()):
			return "", fmt.Errorf("%w (worker: %s)", ErrSchemaVersion, resp.Error)
		case strings.Contains(resp.Error, ErrInterrupted.Error()):
			// The child's own interrupt stopped it: soft deadline, child kept.
			return "", fmt.Errorf("%w after %s (stopped by the worker's interrupt): %s", ErrDeadline, soft, resp.Error)
		}
		return "", fmt.Errorf("%w: %s", ErrJobFailed, resp.Error)
	case resp.Markdown == nil:
		return "", s.fail(c, "protocol", fmt.Errorf("%w: response %d has neither markdown nor error", ErrProtocol, resp.ID))
	}
	return *resp.Markdown, nil
}

// exchange writes one frame and returns the next response. On the timer it
// kills the child for timerReason; on ctx, for "canceled". Every failure
// that ends the child goes through fail.
func (s *Supervisor) exchange(ctx context.Context, c *child, frame []byte, timer <-chan time.Time, timerReason string, timerErr error) (WorkerResponse, error) {
	// The write runs apart from the wait: a child that stops reading would
	// otherwise block it for good, and a Windows pipe takes no deadline. A
	// kill closes the pipe's far end and ends the write.
	wrote := make(chan error, 1)
	go func() {
		_, err := c.stdin.Write(frame)
		wrote <- err
	}()
	for {
		select {
		case err := <-wrote:
			wrote = nil
			if err != nil {
				return WorkerResponse{}, s.died(c)
			}
		case p := <-c.frames:
			var resp WorkerResponse
			if err := json.Unmarshal(p, &resp); err != nil {
				return resp, s.fail(c, "protocol", fmt.Errorf("%w: response is not JSON: %v", ErrProtocol, err))
			}
			return resp, nil
		case <-c.readEnd:
			if c.readErr != nil && !errors.Is(c.readErr, io.EOF) && c.killedFor() == "" {
				// A truncated or oversized frame from a live child.
				select {
				case <-c.exited:
				case <-time.After(200 * time.Millisecond):
					return WorkerResponse{}, s.fail(c, "protocol", fmt.Errorf("%w: %v", ErrProtocol, c.readErr))
				}
			}
			return WorkerResponse{}, s.died(c)
		case <-c.exited:
			return WorkerResponse{}, s.died(c)
		case <-timer:
			return WorkerResponse{}, s.fail(c, timerReason, timerErr)
		case <-ctx.Done():
			return WorkerResponse{}, s.fail(c, "canceled", fmt.Errorf("materialize: %w (worker killed)", ctx.Err()))
		}
	}
}

// fail kills the child for reason and returns err. A kill already made for
// another reason (the watchdog, Close) wins: its classification is returned.
func (s *Supervisor) fail(c *child, reason string, err error) error {
	c.kill(reason)
	if c.killedFor() != reason {
		return s.died(c)
	}
	c.awaitExit()
	s.stopped(c, reason, err)
	return err
}

// died classifies a child that is gone (or going) and returns the job's error.
func (s *Supervisor) died(c *child) error {
	if c.killedFor() == "" && !c.awaitExitFor(2*time.Second) {
		// Its stdout ended (or its stdin broke) but it is still running.
		return s.fail(c, "protocol", fmt.Errorf("%w: worker closed its output but did not exit", ErrProtocol))
	}
	c.awaitExit()
	var reason string
	var err error
	switch k := c.killedFor(); {
	case k == "closed":
		reason, err = "closed", ErrClosed
	case k == "memory":
		reason = "memory"
		err = fmt.Errorf("%w (%w): resident set above %s; killed by the watchdog", ErrMemoryLimit, ErrChildDied, formatBytes(s.memLimit))
	case k != "":
		// Killed by us for a reason whose own path returns; not reached in
		// practice, but never report such a death as a crash.
		reason, err = k, fmt.Errorf("%w: killed (%s)", ErrChildDied, k)
	case c.stderr.oom.Load():
		reason = "memory"
		err = fmt.Errorf("%w (%w): %s; the Go runtime reported out of memory under a %s cap",
			ErrMemoryLimit, ErrChildDied, c.exitString(), formatBytes(s.memLimit))
	default:
		reason = "exit"
		err = fmt.Errorf("%w: %s", ErrChildDied, c.exitString())
	}
	s.stopped(c, reason, err)
	return err
}

func exitDetail(err error) string {
	if err == nil {
		return "exited with status 0"
	}
	return err.Error()
}

// stopped forgets the child, schedules the respawn and logs the stop. Called
// once per child: only the job holding the slot (or Close, which has already
// forgotten it) reaches here.
func (s *Supervisor) stopped(c *child, reason string, err error) {
	s.mu.Lock()
	if s.child != c {
		// Close got there first and logged it.
		s.mu.Unlock()
		return
	}
	s.child = nil
	var delay time.Duration
	if reason != "canceled" && reason != "closed" {
		delay = s.recordDeathLocked(c)
	}
	s.mu.Unlock()
	s.logStop(c, reason, delay, "detail", err.Error())
}

func (s *Supervisor) logStop(c *child, reason string, delay time.Duration, extra ...any) {
	attrs := []any{"pid", c.pid, "reason", reason, "exit", c.exitString(),
		"uptime", s.cfg.now().Sub(c.started).Round(time.Millisecond).String(), "jobs", c.jobs.Load(),
		"respawn_after", delay.String()}
	s.log.Warn("materialize worker stopped", append(attrs, extra...)...)
}

// recordDeathLocked bumps the death streak and sets the next spawn time. A
// child that lived HealthyStretch resets the streak first. Holds s.mu.
func (s *Supervisor) recordDeathLocked(c *child) time.Duration {
	now := s.cfg.now()
	if c != nil && now.Sub(c.started) >= s.cfg.HealthyStretch {
		s.streak = 0
	}
	s.streak++
	delay := s.cfg.BackoffBase
	for i := 1; i < s.streak && delay < s.cfg.BackoffMax; i++ {
		delay *= 2
	}
	if delay > s.cfg.BackoffMax {
		delay = s.cfg.BackoffMax
	}
	s.nextSpawn = now.Add(delay)
	return delay
}

// watchRSS is the parent-side memory cap for platforms that do not enforce
// one on the child (macOS).
func (s *Supervisor) watchRSS(c *child) {
	t := time.NewTicker(s.cfg.watchInterval)
	defer t.Stop()
	for {
		select {
		case <-c.exited:
			return
		case <-t.C:
			rss, err := s.cap.rss(c.pid)
			if err != nil {
				continue // gone, or not yet visible; exit is seen above
			}
			if rss > s.memLimit {
				c.kill("memory")
				return
			}
		}
	}
}

func encodeFrame(req WorkerRequest) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("materialize: encode request: %w", err)
	}
	if len(body) > MaxFrameBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrJobTooLarge, len(body))
	}
	out := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(body)))
	return append(out, body...), nil
}

// ---------------------------------------------------------------------------
// the child's stderr

const (
	stderrMaxLine  = 4 << 10
	stderrMaxLines = 200
)

// stderrLog is the child's stderr: every line goes to the log, prefixed by
// the worker's pid, and never near the protocol (stdout). It always drains —
// a child blocked on a full stderr pipe would read as hung — but logs at most
// stderrMaxLines lines per child (a Go crash dump is thousands), each cut at
// stderrMaxLine bytes, and reports how many it dropped.
//
// It also watches for the Go runtime's out-of-memory report, which is how a
// death on the RLIMIT_AS / Job Object cap is told from any other.
type stderrLog struct {
	log *slog.Logger
	pid int

	mu      sync.Mutex
	partial []byte
	lines   int
	dropped int
	oom     atomic.Bool
}

// oomMarkers are the Go runtime's words for a failed allocation: "fatal
// error: runtime: out of memory" / "fatal error: out of memory", and the mmap
// failure ("cannot allocate memory", ENOMEM). On Windows a VirtualAlloc
// failure is followed by the same "fatal error: out of memory".
var oomMarkers = []string{"out of memory", "cannot allocate memory"}

func (l *stderrLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			room := stderrMaxLine - len(l.partial)
			if room > 0 {
				take := min(room, len(p))
				l.partial = append(l.partial, p[:take]...)
			}
			break
		}
		room := stderrMaxLine - len(l.partial)
		if room > 0 {
			l.partial = append(l.partial, p[:min(room, i)]...)
		}
		l.emitLocked()
		p = p[i+1:]
	}
	return n, nil
}

func (l *stderrLog) emitLocked() {
	line := string(l.partial)
	l.partial = l.partial[:0]
	for _, m := range oomMarkers {
		if strings.Contains(line, m) {
			l.oom.Store(true)
		}
	}
	if l.lines >= stderrMaxLines {
		l.dropped++
		return
	}
	l.lines++
	l.log.Info("materialize worker stderr: "+line, "pid", l.pid)
}

func (l *stderrLog) setPid(pid int) {
	l.mu.Lock()
	l.pid = pid
	l.mu.Unlock()
}

func (l *stderrLog) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.partial) > 0 {
		l.emitLocked()
	}
	if l.dropped > 0 {
		l.log.Info("materialize worker stderr: lines dropped", "pid", l.pid, "dropped", l.dropped)
	}
}
