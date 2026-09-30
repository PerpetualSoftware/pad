package materialize

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pad "github.com/PerpetualSoftware/pad"
)

// The supervisor tests spawn THIS test binary as the worker: TestMain sees
// helperEnv and runs a helper instead of the tests.
//
//   - "worker": the real RunWorker over the embedded bundle.
//   - "script": answers the readiness probe, then does what each job's
//     SchemaVersion says (see runScript), so one child can behave well on one
//     job and badly on the next.
const helperEnv = "PAD_MATERIALIZE_TEST_HELPER"

// inflateEnv makes the script helper inflate its baseline by that many MiB
// before it answers the readiness probe (inflateBaseline).
const inflateEnv = "PAD_MATERIALIZE_TEST_INFLATE_MIB"

// journalEnv names a file the script helper appends every JOB it receives to
// (the readiness probe excluded), so a test can prove what reached a worker.
const journalEnv = "PAD_MATERIALIZE_TEST_JOURNAL"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(runHelper(mode))
	}
	os.Exit(m.Run())
}

func runHelper(mode string) int {
	if mode == "parent" {
		return runHelperParent()
	}
	// As the real worker command does (cmd/pad/cmd_materialize.go).
	ExitWhenOrphaned()
	if mode == "worker" {
		if err := RunWorker(os.Stdin, os.Stdout, pad.MaterializerJS); err != nil {
			fmt.Fprintln(os.Stderr, "helper worker:", err)
			return 1
		}
		return 0
	}
	if mib, _ := strconv.Atoi(os.Getenv(inflateEnv)); mib > 0 {
		if err := inflateBaseline(mib); err != nil {
			fmt.Fprintln(os.Stderr, "helper: inflate:", err)
			return 8
		}
	}
	warmThreads() // as RunWorker does before readiness
	r := bufio.NewReader(os.Stdin)
	w := bufio.NewWriter(os.Stdout)
	for {
		p, err := readFrame(r)
		if err != nil {
			return 0
		}
		var req WorkerRequest
		if err := json.Unmarshal(p, &req); err != nil {
			return 9
		}
		if req.SchemaVersion == "" { // the readiness probe
			_ = writeFrame(w, WorkerResponse{ID: req.ID, Error: "materialize: schema version mismatch: probe"})
			continue
		}
		if j := os.Getenv(journalEnv); j != "" {
			f, err := os.OpenFile(j, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				fmt.Fprintln(f, req.SchemaVersion)
				f.Close()
			}
		}
		if !runScript(w, req) {
			return 0
		}
	}
}

func runScript(w *bufio.Writer, req WorkerRequest) bool {
	ok := func(md string) { _ = writeFrame(w, WorkerResponse{ID: req.ID, Markdown: &md}) }
	cmd, arg, _ := strings.Cut(req.SchemaVersion, ":")
	switch cmd {
	case "echo":
		ok("echo:" + arg)
	case "timeout": // echoes the soft deadline it was handed
		ok(strconv.FormatInt(req.TimeoutMs, 10))
	case "hang": // a native call nothing can interrupt
		time.Sleep(time.Hour)
	case "exit":
		fmt.Fprintln(os.Stderr, "helper: exiting on purpose")
		os.Exit(3)
	case "alloc": // allocate arg MiB, every byte written with noise
		// Noise, not a byte per page: macOS compresses idle pages, and a
		// page of zeros and one 1 would compress away out of the resident
		// set the watchdog reads.
		mib, _ := strconv.Atoi(arg)
		b := make([]byte, mib<<20)
		x := uint64(0x9E3779B97F4A7C15)
		for i := 0; i+8 <= len(b); i += 8 {
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			binary.LittleEndian.PutUint64(b[i:], x)
		}
		ok(strconv.Itoa(len(b)))
	case "soft": // what the worker answers when its own interrupt fires
		_ = writeFrame(w, WorkerResponse{ID: req.ID, Error: "materialize: interrupted: context deadline exceeded"})
	case "jserror":
		_ = writeFrame(w, WorkerResponse{ID: req.ID, Error: "materialize: TypeError: boom"})
	case "badjson":
		body := []byte("{nope")
		_, _ = w.Write([]byte{0, 0, 0, byte(len(body))})
		_, _ = w.Write(body)
		_ = w.Flush()
	case "wrongid":
		md := "x"
		_ = writeFrame(w, WorkerResponse{ID: req.ID + 1000, Markdown: &md})
	case "closeout": // close stdout but keep running
		os.Stdout.Close()
		time.Sleep(time.Hour)
	default:
		return false
	}
	return true
}

// runHelperParent is a supervising process for TestSupervisorOrphanedWorkerExits:
// it spawns a script worker, prints its pid, then hangs it in a job forever.
func runHelperParent() int {
	f := &helperFactory{mode: "script"}
	s := NewSupervisor(SupervisorConfig{
		Command:     f.cmd,
		Timeout:     MaxTimeout,
		capOverride: testCap(),
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		onSpawn:     func(pid int) { fmt.Printf("worker %d\n", pid) },
	})
	if _, err := s.Materialize(context.Background(), script("echo:warm")); err != nil {
		fmt.Fprintln(os.Stderr, "helper parent:", err)
		return 2
	}
	fmt.Println("hanging")
	_, err := s.Materialize(context.Background(), script("hang"))
	fmt.Fprintln(os.Stderr, "helper parent: hang returned:", err)
	return 3
}

// ---------------------------------------------------------------------------
// fixtures

type helperFactory struct {
	mode  string
	calls atomic.Int32
}

func (h *helperFactory) cmd() (*exec.Cmd, error) {
	h.calls.Add(1)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"="+h.mode)
	return cmd, nil
}

// logSink records every slog record as "msg k=v k=v".
type logSink struct {
	mu   sync.Mutex
	recs []string
}

func (l *logSink) Enabled(context.Context, slog.Level) bool { return true }
func (l *logSink) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String() + " ")
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	l.mu.Lock()
	l.recs = append(l.recs, b.String())
	l.mu.Unlock()
	return nil
}
func (l *logSink) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logSink) WithGroup(string) slog.Handler      { return l }

// find returns the records containing every one of subs.
func (l *logSink) find(subs ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
next:
	for _, r := range l.recs {
		for _, s := range subs {
			if !strings.Contains(r, s) {
				continue next
			}
		}
		out = append(out, r)
	}
	return out
}

func (l *logSink) warns() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, r := range l.recs {
		if strings.HasPrefix(r, "WARN ") {
			out = append(out, r)
		}
	}
	return out
}

func (l *logSink) dump() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.recs, "\n")
}

type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_000_000, 0)} }

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

func (f *fakeClock) sleep(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	f.sleeps = append(f.sleeps, d)
	f.t = f.t.Add(d)
	f.mu.Unlock()
	return nil
}

func (f *fakeClock) slept() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.sleeps...)
}

// fakeCap reports a fixed baseline and caps nothing: for tests whose subject
// is not the cap, and for race builds, whose runtime reserves terabytes of
// address space (a baseline no plausibility check accepts).
var fakeCap = &memCapImpl{
	mechanism: "fake(test)",
	attach: func(*exec.Cmd, func()) (*capHandle, error) {
		return &capHandle{
			baseline: func() (uint64, error) { return 100 << 20, nil },
			set:      func(uint64) error { return nil },
		}, nil
	},
}

// testCap is the cap for tests that are not about the cap: the platform's,
// except under the race detector.
func testCap() *memCapImpl {
	if raceEnabled {
		return fakeCap
	}
	return nil
}

type harness struct {
	s    *Supervisor
	f    *helperFactory
	log  *logSink
	clk  *fakeClock
	pids chan int
}

func newHarness(t *testing.T, mode string, tweak func(*SupervisorConfig)) *harness {
	t.Helper()
	h := &harness{f: &helperFactory{mode: mode}, log: &logSink{}, clk: newFakeClock(), pids: make(chan int, 64)}
	cfg := SupervisorConfig{
		Command:     h.f.cmd,
		Logger:      slog.New(h.log),
		Timeout:     10 * time.Second,
		now:         h.clk.now,
		sleep:       h.clk.sleep,
		capOverride: testCap(),
		onSpawn:     func(pid int) { h.pids <- pid },
	}
	if tweak != nil {
		tweak(&cfg)
	}
	h.s = NewSupervisor(cfg)
	t.Cleanup(func() {
		_ = h.s.Close()
		if t.Failed() {
			t.Logf("supervisor log:\n%s", h.log.dump())
		}
	})
	return h
}

func (h *harness) lastPid(t *testing.T) int {
	t.Helper()
	select {
	case p := <-h.pids:
		return p
	default:
		t.Fatal("no worker was spawned")
		return 0
	}
}

func script(cmd string) Job { return Job{SchemaVersion: cmd} }

// ---------------------------------------------------------------------------
// 1. lazy start

func TestSupervisorLazyStart(t *testing.T) {
	h := newHarness(t, "script", nil)
	time.Sleep(200 * time.Millisecond)
	if n := h.f.calls.Load(); n != 0 {
		t.Fatalf("NewSupervisor spawned %d processes; want none before the first job", n)
	}
	h.s.mu.Lock()
	c := h.s.child
	h.s.mu.Unlock()
	if c != nil {
		t.Fatal("a child exists before the first job")
	}
	for i := range 3 {
		md, err := h.s.Materialize(context.Background(), script("echo:"+strconv.Itoa(i)))
		if err != nil || md != "echo:"+strconv.Itoa(i) {
			t.Fatalf("job %d: %q, %v", i, md, err)
		}
	}
	if n := h.f.calls.Load(); n != 1 {
		t.Fatalf("%d processes for three jobs; want one long-lived child", n)
	}
}

// ---------------------------------------------------------------------------
// 2. a REAL worker reproduces the in-process Runner

func TestSupervisorRealWorkerParity(t *testing.T) {
	h := newHarness(t, "worker", func(c *SupervisorConfig) { c.Timeout = 30 * time.Second })
	r := runner(t)
	cases := loadCorpus(t)
	if len(cases) > 4 {
		cases = cases[:4]
	}
	for _, tc := range cases {
		job := tc.job(t, r.SchemaVersion())
		want, err := r.Materialize(context.Background(), job)
		if err != nil {
			t.Fatalf("%s in-process: %v", tc.Name, err)
		}
		got, err := h.s.Materialize(context.Background(), job)
		if err != nil {
			t.Fatalf("%s through the supervisor: %v", tc.Name, err)
		}
		if got != want || got != tc.Expected {
			t.Errorf("%s: supervisor %q, in-process %q, corpus %q", tc.Name, got, want, tc.Expected)
		}
	}
	// The real worker's refusal comes back typed.
	if _, err := h.s.Materialize(context.Background(), Job{SchemaVersion: "999"}); !errors.Is(err, ErrSchemaVersion) {
		t.Errorf("schema mismatch: got %v, want ErrSchemaVersion", err)
	}
	if n := h.f.calls.Load(); n != 1 {
		t.Errorf("%d spawns; want 1", n)
	}
	if got := h.log.find("materialize worker started", "mem_cap="); len(got) != 1 {
		t.Errorf("start lines: %q", got)
	}
}

// ---------------------------------------------------------------------------
// 3. deadline: the child is killed, the next job gets a fresh one

func TestSupervisorDeadlineKillsChild(t *testing.T) {
	h := newHarness(t, "script", func(c *SupervisorConfig) { c.Timeout = MinTimeout })
	if _, err := h.s.Materialize(context.Background(), script("echo:warm")); err != nil {
		t.Fatal(err)
	}
	pid := h.lastPid(t)

	start := time.Now()
	_, err := h.s.Materialize(context.Background(), script("hang"))
	took := time.Since(start)
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("got %v, want ErrDeadline", err)
	}
	if took > MinTimeout+time.Second {
		t.Fatalf("deadline %s enforced after %s", MinTimeout, took)
	}
	if err := processGone(pid); err != nil {
		t.Fatalf("the hung worker survived its deadline: %v", err)
	}
	if got := h.log.find("materialize worker stopped", "reason=deadline", "pid="+strconv.Itoa(pid)); len(got) != 1 {
		t.Fatalf("stop line: %q", got)
	}

	md, err := h.s.Materialize(context.Background(), script("echo:after"))
	if err != nil || md != "echo:after" {
		t.Fatalf("job after the kill: %q, %v", md, err)
	}
	if pid2 := h.lastPid(t); pid2 == pid {
		t.Fatalf("the job after a kill ran on pid %d again", pid)
	}
	t.Logf("deadline %s enforced in %s", MinTimeout, took.Round(time.Millisecond))
}

// The child is asked to stop itself a little before the hard deadline, and a
// job its interrupt stops does not cost the worker.
func TestSupervisorSoftDeadlineKeepsChild(t *testing.T) {
	h := newHarness(t, "script", func(c *SupervisorConfig) { c.Timeout = 2 * time.Second })
	md, err := h.s.Materialize(context.Background(), script("timeout"))
	if err != nil || md != "1800" {
		t.Fatalf("timeout_ms for a 2s deadline: %q, %v; want 1800", md, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	md, err = h.s.Materialize(ctx, script("timeout"))
	if n, _ := strconv.Atoi(md); err != nil || n <= 0 || n >= 1000 {
		t.Fatalf("timeout_ms under a 1s ctx: %q, %v; want below the ctx", md, err)
	}
	if _, err := h.s.Materialize(context.Background(), script("soft")); !errors.Is(err, ErrDeadline) {
		t.Fatalf("got %v, want ErrDeadline", err)
	}
	if _, err := h.s.Materialize(context.Background(), script("echo:x")); err != nil {
		t.Fatal(err)
	}
	if n := h.f.calls.Load(); n != 1 {
		t.Fatalf("a soft deadline respawned the worker (%d spawns)", n)
	}
}

// ---------------------------------------------------------------------------
// 4. crash, and the respawn backoff

func TestSupervisorCrashAndBackoff(t *testing.T) {
	h := newHarness(t, "script", func(c *SupervisorConfig) {
		c.BackoffBase = time.Second
		c.BackoffMax = 8 * time.Second
		c.HealthyStretch = time.Minute
	})
	var pids []int
	for i := range 6 {
		_, err := h.s.Materialize(context.Background(), script("exit"))
		if !errors.Is(err, ErrChildDied) || errors.Is(err, ErrMemoryLimit) {
			t.Fatalf("crash %d: got %v, want ErrChildDied", i, err)
		}
		if !strings.Contains(err.Error(), "exit status 3") {
			t.Fatalf("crash %d: error does not name the exit status: %v", i, err)
		}
		pids = append(pids, h.lastPid(t))
	}
	// Every death is logged with its reason, and the child's stderr reached
	// the log, attributed.
	for _, pid := range pids {
		p := "pid=" + strconv.Itoa(pid)
		if got := h.log.find("materialize worker stopped", "reason=exit", "exit status 3", p); len(got) != 1 {
			t.Fatalf("stop line for %s: %q", p, got)
		}
		if got := h.log.find("materialize worker stderr: helper: exiting on purpose", p); len(got) != 1 {
			t.Fatalf("stderr line for %s: %q", p, got)
		}
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	if got := h.clk.slept(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("respawn waits %v, want %v", got, want)
	}

	// The next job respawns (after the capped wait) and succeeds.
	if md, err := h.s.Materialize(context.Background(), script("echo:ok")); err != nil || md != "echo:ok" {
		t.Fatalf("job after crashes: %q, %v", md, err)
	}
	// A child that lived a healthy stretch resets the streak: its death
	// costs the base wait, not the cap.
	h.clk.advance(2 * time.Minute)
	if _, err := h.s.Materialize(context.Background(), script("exit")); !errors.Is(err, ErrChildDied) {
		t.Fatal(err)
	}
	if _, err := h.s.Materialize(context.Background(), script("echo:ok")); err != nil {
		t.Fatal(err)
	}
	slept := h.clk.slept()
	if got := slept[len(slept)-1]; got != time.Second {
		t.Fatalf("wait after a healthy stretch: %v, want 1s (all: %v)", got, slept)
	}
}

func TestSupervisorProtocolErrors(t *testing.T) {
	h := newHarness(t, "script", nil)
	if _, err := h.s.Materialize(context.Background(), script("echo:warm")); err != nil {
		t.Fatal(err)
	}
	pid := h.lastPid(t)
	for _, bad := range []string{"badjson", "wrongid"} {
		if _, err := h.s.Materialize(context.Background(), script(bad)); !errors.Is(err, ErrProtocol) {
			t.Fatalf("%s: got %v, want ErrProtocol", bad, err)
		}
		if err := processGone(pid); err != nil {
			t.Fatalf("%s: worker not killed: %v", bad, err)
		}
		if got := h.log.find("materialize worker stopped", "reason=protocol", "pid="+strconv.Itoa(pid)); len(got) != 1 {
			t.Fatalf("%s: stop line %q", bad, got)
		}
		if md, err := h.s.Materialize(context.Background(), script("echo:ok")); err != nil || md != "echo:ok" {
			t.Fatalf("after %s: %q, %v", bad, md, err)
		}
		pid = h.lastPid(t)
	}
	if _, err := h.s.Materialize(context.Background(), script("jserror")); !errors.Is(err, ErrJobFailed) {
		t.Fatalf("jserror: %v", err)
	}
	if _, err := h.s.Materialize(context.Background(), script("closeout")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("closeout: got %v, want ErrProtocol", err)
	}
}

func TestSupervisorCtxCancelKillsWithoutBackoff(t *testing.T) {
	h := newHarness(t, "script", nil)
	if _, err := h.s.Materialize(context.Background(), script("echo:warm")); err != nil {
		t.Fatal(err)
	}
	pid := h.lastPid(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := h.s.Materialize(ctx, script("hang"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if err := processGone(pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Materialize(context.Background(), script("echo:ok")); err != nil {
		t.Fatal(err)
	}
	if s := h.clk.slept(); len(s) != 0 {
		t.Fatalf("a caller's cancellation cost a backoff: %v", s)
	}
}

// ---------------------------------------------------------------------------
// 5. memory cap

// TestSupervisorMemoryCapPlatform runs the cap this OS ships with, which is
// RELATIVE to the loaded worker: a job may grow it by the limit. It runs
// twice: as is, and with the worker's baseline inflated before readiness
// (inflateBaseline: ~1.5 GiB of never-touched address space on Linux), which
// stands in for a host whose runtime reserves more than this one — the case
// the absolute cap this replaced failed on CI.
func TestSupervisorMemoryCapPlatform(t *testing.T) {
	if raceEnabled {
		skipCapTest(t, "the race runtime cannot start under an address-space cap")
	}
	if platformMemCap.unsupported {
		skipCapTest(t, "no memory cap on this OS ("+platformMemCap.mechanism+")")
	}
	const limit = 512 << 20
	for _, inflate := range []int{0, inflateMiB} {
		t.Run("baseline+"+strconv.Itoa(inflate)+"MiB", func(t *testing.T) {
			h := newHarness(t, "script", func(c *SupervisorConfig) {
				c.MemLimit = limit
				c.capOverride = nil
				c.extraEnv = []string{inflateEnv + "=" + strconv.Itoa(inflate)}
			})
			if _, err := h.s.Materialize(context.Background(), script("echo:warm")); err != nil {
				t.Fatal(err)
			}
			pid := h.lastPid(t)
			started := h.log.find("materialize worker started", "pid="+strconv.Itoa(pid))
			if len(started) != 1 || !strings.Contains(started[0], "mem_limit=512MiB") || !strings.Contains(started[0], "mem_baseline=") {
				t.Fatalf("start line: %q", started)
			}
			t.Log(started[0])
			if inflate > 0 {
				h.s.mu.Lock()
				b := h.s.child.baseline
				h.s.mu.Unlock()
				if b < uint64(inflate)<<20 {
					t.Fatalf("baseline %d does not include the %d MiB inflation", b, inflate)
				}
			}
			// Comfortably under baseline + limit: fine.
			if md, err := h.s.Materialize(context.Background(), script("alloc:128")); err != nil || md != strconv.Itoa(128<<20) {
				t.Fatalf("128 MiB of growth under a 512 MiB limit: %q, %v", md, err)
			}
			// Over it: killed on the cap.
			md, err := h.s.Materialize(context.Background(), script("alloc:1024"))
			if !errors.Is(err, ErrMemoryLimit) || !errors.Is(err, ErrChildDied) {
				t.Fatalf("1 GiB of growth under a 512 MiB limit: %q, %v; want ErrMemoryLimit", md, err)
			}
			if got := h.log.find("materialize worker stopped", "reason=memory", "pid="+strconv.Itoa(pid)); len(got) != 1 {
				t.Fatalf("stop line: %q", got)
			}
			if md, err := h.s.Materialize(context.Background(), script("echo:ok")); err != nil || md != "echo:ok" {
				t.Fatalf("after the memory kill: %q, %v", md, err)
			}
			t.Logf("over-cap error: %v", err)
		})
	}
}

// TestSupervisorRSSWatchdog drives the macOS mechanism (a parent-side RSS
// poll and kill) wherever a sampler exists, so it runs on Linux too.
func TestSupervisorRSSWatchdog(t *testing.T) {
	if testRSSSampler == nil {
		skipCapTest(t, "no RSS sampler for tests on this OS")
	}
	h := newHarness(t, "script", func(c *SupervisorConfig) {
		c.MemLimit = 512 << 20
		c.capOverride = &memCapImpl{mechanism: "rss_watchdog(test)", rss: testRSSSampler}
		c.watchInterval = 100 * time.Millisecond
	})
	if _, err := h.s.Materialize(context.Background(), script("echo:warm")); err != nil {
		t.Fatal(err)
	}
	pid := h.lastPid(t)
	md, err := h.s.Materialize(context.Background(), script("alloc:1024"))
	if !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("1 GiB resident under a baseline + 512 MiB watchdog: %q, %v", md, err)
	}
	if !strings.Contains(err.Error(), "watchdog") {
		t.Fatalf("not attributed to the watchdog: %v", err)
	}
	if err := processGone(pid); err != nil {
		t.Fatal(err)
	}
	if got := h.log.find("materialize worker stopped", "reason=memory"); len(got) != 1 {
		t.Fatalf("stop line: %q", got)
	}
	if _, err := h.s.Materialize(context.Background(), script("echo:ok")); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// 6. env knobs

func TestParseMemLimit(t *testing.T) {
	for in, want := range map[string]uint64{
		"2048MiB":    2 << 30,
		"2GiB":       2 << 30,
		"1610612736": 1536 << 20,
		"512KiB":     512 << 10,
		" 3GiB ":     3 << 30,
	} {
		if got, err := ParseMemLimit(in); err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "2GB", "2 GiB", "2gib", "1.5GiB", "-1", "GiB", "99999999999999999999", "17179869184GiB"} {
		if got, err := ParseMemLimit(in); err == nil {
			t.Errorf("%q parsed as %d; want an error", in, got)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	sink := &logSink{}
	lg := slog.New(sink)

	cfg := ConfigFromEnv(env(nil), lg)
	if cfg.Timeout != 0 || cfg.MemLimit != 0 {
		t.Fatalf("unset: %+v", cfg)
	}
	cfg = ConfigFromEnv(env(map[string]string{EnvTimeout: "5s", EnvMemLimit: "3GiB"}), lg)
	if cfg.Timeout != 5*time.Second || cfg.MemLimit != 3<<30 {
		t.Fatalf("set: %v %d", cfg.Timeout, cfg.MemLimit)
	}
	if len(sink.find("materialize")) != 0 {
		t.Fatalf("valid values logged: %s", sink.dump())
	}
	cfg = ConfigFromEnv(env(map[string]string{EnvTimeout: "soon", EnvMemLimit: "2GB"}), lg)
	if cfg.Timeout != 0 || cfg.MemLimit != 0 {
		t.Fatalf("invalid values must fall back to the default: %+v", cfg)
	}
	if len(sink.find("ignoring invalid "+EnvTimeout, "value=soon")) != 1 || len(sink.find("ignoring invalid "+EnvMemLimit, "value=2GB")) != 1 {
		t.Fatalf("invalid values not logged: %s", sink.dump())
	}
}

func TestSupervisorClampAndEffectiveLog(t *testing.T) {
	cases := []struct {
		timeout   time.Duration
		mem       uint64
		wantT     string
		wantM     string
		clampLogs int
	}{
		{0, 0, "2s", "2GiB", 0},
		{time.Millisecond, 128 << 20, "250ms", "256MiB", 2},
		{10 * time.Minute, 64 << 30, "1m0s", "16GiB", 2},
		{5 * time.Second, 4 << 30, "5s", "4GiB", 0},
	}
	for _, tc := range cases {
		h := newHarness(t, "script", func(c *SupervisorConfig) {
			c.Timeout = tc.timeout
			c.MemLimit = tc.mem
			c.capOverride = fakeCap // the value is under test, not the cap
		})
		if got := len(h.log.find("out of range; clamped")); got != tc.clampLogs {
			t.Errorf("%v/%d: %d clamp warnings, want %d: %s", tc.timeout, tc.mem, got, tc.clampLogs, h.log.dump())
		}
		if _, err := h.s.Materialize(context.Background(), script("echo:x")); err != nil {
			t.Fatal(err)
		}
		got := h.log.find("materialize worker started", "timeout="+tc.wantT+" ", "mem_limit="+tc.wantM+" ", "mem_cap=fake(test)")
		if len(got) != 1 {
			t.Errorf("%v/%d: effective-values line missing: %s", tc.timeout, tc.mem, h.log.dump())
		}
	}
	// The production default names the platform mechanism.
	h := newHarness(t, "script", func(c *SupervisorConfig) { c.capOverride = nil })
	if h.s.cap.mechanism != platformMemCap.mechanism {
		t.Fatalf("mechanism %q", h.s.cap.mechanism)
	}
}

// ---------------------------------------------------------------------------
// the supervising process dies mid-job

// TestSupervisorOrphanedWorkerExits: when the SUPERVISING process is killed
// while its worker is inside a job, the worker must not run on without its
// deadline kill and memory watchdog. Process tree: this test -> a helper
// parent running a Supervisor -> its worker, hung in a job. SIGKILL the helper
// parent; the worker must be gone within a second (Linux: Pdeathsig, and the
// ppid watch; macOS: the ppid watch; Windows: KILL_ON_JOB_CLOSE).
func TestSupervisorOrphanedWorkerExits(t *testing.T) {
	parent := exec.Command(os.Args[0], "-test.run=^$")
	parent.Env = append(os.Environ(), helperEnv+"=parent")
	var stderr lockedBuffer
	parent.Stderr = &stderr
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Process.Kill(); _ = parent.Wait() })

	lines := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	next := func() string {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("helper parent ended early; stderr:\n%s", stderr.String())
			}
			return l
		case <-time.After(60 * time.Second):
			t.Fatalf("helper parent silent; stderr:\n%s", stderr.String())
		}
		return ""
	}
	var worker int
	if _, err := fmt.Sscanf(next(), "worker %d", &worker); err != nil || worker <= 0 {
		t.Fatalf("no worker pid: %v", err)
	}
	t.Cleanup(func() {
		if processGone(worker) != nil {
			if p, err := os.FindProcess(worker); err == nil {
				_ = p.Kill()
			}
		}
	})
	if l := next(); l != "hanging" {
		t.Fatalf("got %q", l)
	}
	time.Sleep(300 * time.Millisecond) // the worker is now inside the hung job
	if processGone(worker) == nil {
		t.Fatal("premise: the worker must be alive before its parent dies")
	}

	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()
	killed := time.Now()
	for processGone(worker) != nil {
		if time.Since(killed) > 5*time.Second {
			t.Fatalf("worker %d outlived its supervising process by 5s", worker)
		}
		time.Sleep(10 * time.Millisecond)
	}
	took := time.Since(killed)
	t.Logf("worker %d gone %s after its parent was killed", worker, took.Round(time.Millisecond))
	if took > time.Second {
		t.Fatalf("worker took %s to exit after its parent died; want under 1s", took)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// ---------------------------------------------------------------------------
// 7. Close

func TestSupervisorClose(t *testing.T) {
	h := newHarness(t, "script", nil)
	if _, err := h.s.Materialize(context.Background(), script("echo:x")); err != nil {
		t.Fatal(err)
	}
	pid := h.lastPid(t)
	if err := h.s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := processGone(pid); err != nil {
		t.Fatalf("Close left the worker running: %v", err)
	}
	if err := h.s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := h.s.Materialize(context.Background(), script("echo:x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("after Close: %v", err)
	}
	if n := h.f.calls.Load(); n != 1 {
		t.Fatalf("a job after Close spawned (%d spawns)", n)
	}
}

func TestSupervisorCloseDuringJob(t *testing.T) {
	h := newHarness(t, "script", nil)
	if _, err := h.s.Materialize(context.Background(), script("echo:x")); err != nil {
		t.Fatal(err)
	}
	pid := h.lastPid(t)
	done := make(chan error, 1)
	go func() {
		_, err := h.s.Materialize(context.Background(), script("hang"))
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	_ = h.s.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("running job: %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the running job did not end on Close")
	}
	if err := processGone(pid); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorCloseBeforeFirstJob(t *testing.T) {
	h := newHarness(t, "script", nil)
	_ = h.s.Close()
	if _, err := h.s.Materialize(context.Background(), script("echo:x")); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if n := h.f.calls.Load(); n != 0 {
		t.Fatalf("%d spawns", n)
	}
}
