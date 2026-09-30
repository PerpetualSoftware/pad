package materialize

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// probeCap is a cap whose baseline probe answers what the test says.
func probeCap(probe func() (uint64, error)) *memCapImpl {
	return &memCapImpl{
		mechanism: "probe(test)",
		attach: func(*exec.Cmd, func()) (*capHandle, error) {
			return &capHandle{baseline: probe, set: func(uint64) error { return nil }}, nil
		},
	}
}

func journal(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// A worker whose memory baseline cannot be established is killed WITHOUT
// being sent a job, and the job fails with ErrNoMemoryCap. One WARN names the
// cause across any number of such refusals; each refused job's successor
// starts a new worker and probes again, and a working probe ends the refusals.
func TestSupervisorBaselineProbeFailureRefuses(t *testing.T) {
	for name, bad := range map[string]func() (uint64, error){
		"unreadable":  func() (uint64, error) { return 0, errors.New("probe exploded") },
		"zero":        func() (uint64, error) { return 0, nil },
		"implausible": func() (uint64, error) { return MaxPlausibleBaseline + 1, nil },
	} {
		t.Run(name, func(t *testing.T) {
			var broken atomic.Bool
			broken.Store(true)
			jpath := filepath.Join(t.TempDir(), "journal")
			h := newHarness(t, "script", func(c *SupervisorConfig) {
				c.capOverride = probeCap(func() (uint64, error) {
					if broken.Load() {
						return bad()
					}
					return 100 << 20, nil
				})
				c.extraEnv = []string{journalEnv + "=" + jpath}
			})
			const n = 4
			for i := range n {
				_, err := h.s.Materialize(context.Background(), script("echo:refused"))
				if got := journal(t, jpath); len(got) != 0 {
					t.Fatalf("job %d reached an uncapped worker: %q", i, got)
				}
				if !errors.Is(err, ErrNoMemoryCap) {
					t.Fatalf("job %d: got %v, want ErrNoMemoryCap", i, err)
				}
				if err := processGone(h.lastPid(t)); err != nil {
					t.Fatalf("job %d: the uncapped worker was left running: %v", i, err)
				}
			}
			if got := h.f.calls.Load(); got != n {
				t.Fatalf("%d spawns for %d refused jobs; each must start and probe a new worker", got, n)
			}
			if got := journal(t, jpath); len(got) != 0 {
				t.Fatalf("jobs reached an uncapped worker: %q", got)
			}
			warns := h.log.warns()
			if len(warns) != 1 || !strings.Contains(warns[0], "could not establish") || !strings.Contains(warns[0], "cause=") {
				t.Fatalf("want exactly one WARN naming the cause across %d refusals, got %d:\n%s", n, len(warns), strings.Join(warns, "\n"))
			}
			t.Log(warns[0])

			// The probe works again: the next start re-probes and the job runs.
			broken.Store(false)
			if md, err := h.s.Materialize(context.Background(), script("echo:ok")); err != nil || md != "echo:ok" {
				t.Fatalf("after the probe recovered: %q, %v", md, err)
			}
			if got := journal(t, jpath); len(got) != 1 || got[0] != "echo:ok" {
				t.Fatalf("journal %q, want only the job after recovery", got)
			}
			// A failure after a success is news again: one more WARN.
			broken.Store(true)
			h.s.mu.Lock()
			c := h.s.child
			h.s.mu.Unlock()
			if c != nil {
				c.kill("test")
				c.awaitExit()
				h.s.stoppedQuiet(c)
			}
			if _, err := h.s.Materialize(context.Background(), script("echo:refused")); !errors.Is(err, ErrNoMemoryCap) {
				t.Fatalf("after re-breaking: %v", err)
			}
			if got := len(h.log.warns()); got != 2 {
				t.Fatalf("%d WARNs; a failure after a success must warn again", got)
			}
		})
	}
}

// On an OS with no cap mechanism, no worker is ever started and every job is
// refused, with one WARN naming the OS.
func TestSupervisorUnsupportedOSRefuses(t *testing.T) {
	h := newHarness(t, "script", func(c *SupervisorConfig) {
		c.capOverride = &memCapImpl{mechanism: "none", unsupported: true}
	})
	for i := range 3 {
		if _, err := h.s.Materialize(context.Background(), script("echo:x")); !errors.Is(err, ErrNoMemoryCap) {
			t.Fatalf("job %d: %v", i, err)
		}
	}
	if n := h.f.calls.Load(); n != 0 {
		t.Fatalf("%d workers started on an OS with no cap", n)
	}
	warns := h.log.warns()
	if len(warns) != 1 || !strings.Contains(warns[0], "goos="+runtime.GOOS) {
		t.Fatalf("want one WARN naming the OS, got %q", warns)
	}
}

func TestParseStatVsize(t *testing.T) {
	line := "1234 (a (weird) name) S 1 1234 1234 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 7 0 555 1423151104 18000 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0"
	if n, err := parseStatVsize(line); err != nil || n != 1423151104 {
		t.Fatalf("%d %v", n, err)
	}
	for _, bad := range []string{"", "1234 no parens", "1 (x) S 1 2"} {
		if _, err := parseStatVsize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestStderrOOMMarkers(t *testing.T) {
	for line, want := range map[string]bool{
		"fatal error: runtime: out of memory":                                  true,
		"runtime: mmap: cannot allocate memory":                                true,
		"runtime/cgo: pthread_create failed: Resource temporarily unavailable": true,
		"helper: exiting on purpose":                                           false,
		"panic: boom":                                                          false,
	} {
		l := &stderrLog{log: slog.New(&logSink{})}
		_, _ = l.Write([]byte(line + "\n"))
		if l.oom.Load() != want {
			t.Errorf("%q: oom=%v, want %v", line, l.oom.Load(), want)
		}
	}
}
