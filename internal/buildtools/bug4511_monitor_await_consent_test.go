package buildtools

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// BUG-4511: plugin/scripts/pad-monitor.sh is launched by the
// on-skill-invoke:connect monitor the moment /pad:connect is invoked, before
// the skill's own `pad session arm` has run. With --await-consent the script
// waits a bounded time for consent instead of exiting on the first no; the
// plugin mod's call (no flag) keeps the immediate gate.

const fakePadTemplate = `#!/bin/sh
d='@DIR@'
case "$1 $2" in
"watch --help") exit 0 ;;
"session register") exit 0 ;;
"session should-arm")
	echo should-arm >>"$d/calls"
	[ -f "$d/armed" ]
	exit $?
	;;
"watch --stream")
	echo stream >>"$d/calls"
	rm -f "$d/armed"
	: >"$d/streamed"
	exit 0
	;;
esac
exit 1
`

type monitorRun struct {
	dir      string
	stdout   string
	stderr   string
	err      error
	elapsed  time.Duration
	streamed bool
}

// runMonitor runs the real pad-monitor.sh against a fake pad on PATH.
// armAfter < 0 means consent never appears; 0 means armed before start.
func runMonitor(t *testing.T, args []string, env []string, armAfter time.Duration) monitorRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("pad-monitor.sh is a POSIX sh script")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "plugin", "scripts", "pad-monitor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := strings.ReplaceAll(fakePadTemplate, "@DIR@", dir)
	if err := os.WriteFile(filepath.Join(bin, "pad"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	armed := filepath.Join(dir, "armed")
	if armAfter == 0 {
		if err := os.WriteFile(armed, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{script}, args...)...)
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"CLAUDE_CODE_MESSAGING_SOCKET=bug4511-"+t.Name()+"-"+dir,
	)
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if armAfter > 0 {
		timer := time.AfterFunc(armAfter, func() {
			_ = os.WriteFile(armed, nil, 0o644)
		})
		defer timer.Stop()
	}
	runErr := cmd.Wait()
	elapsed := time.Since(start)
	if ctx.Err() != nil {
		t.Fatalf("pad-monitor.sh did not exit within the test timeout (%v)", elapsed)
	}
	_, statErr := os.Stat(filepath.Join(dir, "streamed"))
	return monitorRun{
		dir:      dir,
		stdout:   stdout.String(),
		stderr:   stderr.String(),
		err:      runErr,
		elapsed:  elapsed,
		streamed: statErr == nil,
	}
}

func (r monitorRun) calls(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(r.dir, "calls"))
	return string(b)
}

func TestBUG4511_AwaitConsent_StreamsWhenConsentArrivesLate(t *testing.T) {
	r := runMonitor(t, []string{"--await-consent"},
		[]string{"PAD_MONITOR_CONSENT_WAIT=10", "PAD_MONITOR_CONSENT_POLL=1"},
		1500*time.Millisecond)
	if r.err != nil {
		t.Fatalf("exit: %v (stderr %q)", r.err, r.stderr)
	}
	if !r.streamed {
		t.Fatalf("the connect monitor never streamed after consent arrived late; calls:\n%s", r.calls(t))
	}
	if r.stdout != "" || r.stderr != "" {
		t.Fatalf("script printed output: stdout %q stderr %q", r.stdout, r.stderr)
	}
}

func TestBUG4511_AwaitConsent_ExitsSilentlyWhenConsentNeverArrives(t *testing.T) {
	r := runMonitor(t, []string{"--await-consent"},
		[]string{"PAD_MONITOR_CONSENT_WAIT=2", "PAD_MONITOR_CONSENT_POLL=1"},
		-1)
	if r.err != nil {
		t.Fatalf("exit: %v (stderr %q)", r.err, r.stderr)
	}
	if r.streamed {
		t.Fatal("streamed without consent")
	}
	if r.stdout != "" || r.stderr != "" {
		t.Fatalf("script printed output: stdout %q stderr %q", r.stdout, r.stderr)
	}
	if r.elapsed < 1500*time.Millisecond || r.elapsed > 6*time.Second {
		t.Fatalf("bounded wait took %v, want ~2s", r.elapsed)
	}
	if n := strings.Count(r.calls(t), "should-arm"); n < 2 {
		t.Fatalf("expected the wait to poll should-arm more than once, got %d", n)
	}
}

// Overrides other than a plain decimal of one to four digits with no leading
// zero fall back to the default: a zero poll never advances the counter, a
// leading zero reads as octal in shell arithmetic (08 is an error), and an
// over-long number overflows the test builtin. Each must stay silent and
// bounded. POLL takes its default (2s) here, so WAIT=2 ends in about 2s; a
// bad WAIT takes its default (60s), so those cases arm at 1s and must stream.
func TestBUG4511_AwaitConsent_MalformedOverridesFallBack(t *testing.T) {
	for _, poll := range []string{"00", "0", "08", "010", "99999999999999999999999"} {
		t.Run("poll="+poll, func(t *testing.T) {
			r := runMonitor(t, []string{"--await-consent"},
				[]string{"PAD_MONITOR_CONSENT_WAIT=2", "PAD_MONITOR_CONSENT_POLL=" + poll},
				-1)
			if r.err != nil {
				t.Fatalf("exit: %v (stderr %q, took %v)", r.err, r.stderr, r.elapsed)
			}
			if r.streamed {
				t.Fatal("streamed without consent")
			}
			if r.stdout != "" || r.stderr != "" {
				t.Fatalf("script printed output: stdout %q stderr %q", r.stdout, r.stderr)
			}
			if r.elapsed > 6*time.Second {
				t.Fatalf("wait took %v, want it bounded near 2s", r.elapsed)
			}
		})
	}
	for _, wait := range []string{"08", "99999999999999999999999"} {
		t.Run("wait="+wait, func(t *testing.T) {
			r := runMonitor(t, []string{"--await-consent"},
				[]string{"PAD_MONITOR_CONSENT_WAIT=" + wait, "PAD_MONITOR_CONSENT_POLL=1"},
				time.Second)
			if r.err != nil {
				t.Fatalf("exit: %v (stderr %q, took %v)", r.err, r.stderr, r.elapsed)
			}
			if !r.streamed {
				t.Fatalf("never streamed after consent arrived; calls:\n%s", r.calls(t))
			}
			if r.stdout != "" || r.stderr != "" {
				t.Fatalf("script printed output: stdout %q stderr %q", r.stdout, r.stderr)
			}
		})
	}
}

func TestBUG4511_NoFlag_ExitsPromptlyWithoutWaiting(t *testing.T) {
	// Consent lands at 3s and the script must be gone by 2.5s: wide enough
	// margins that a loaded machine does not turn startup time into a fail.
	r := runMonitor(t, nil, nil, 3*time.Second)
	if r.err != nil {
		t.Fatalf("exit: %v (stderr %q)", r.err, r.stderr)
	}
	if r.streamed {
		t.Fatalf("the mod's call streamed after a late consent; it must gate immediately (took %v)", r.elapsed)
	}
	if r.elapsed > 2500*time.Millisecond {
		t.Fatalf("no-flag run took %v; it must not wait for consent", r.elapsed)
	}
	if r.stdout != "" || r.stderr != "" {
		t.Fatalf("script printed output: stdout %q stderr %q", r.stdout, r.stderr)
	}
}

func TestBUG4511_NoFlag_AlreadyArmedStreamsOnce(t *testing.T) {
	r := runMonitor(t, nil, nil, 0)
	if r.err != nil {
		t.Fatalf("exit: %v (stderr %q)", r.err, r.stderr)
	}
	if !r.streamed {
		t.Fatal("an armed session did not stream")
	}
	n := 0
	for _, line := range strings.Split(r.calls(t), "\n") {
		if line == "stream" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("streamed %d times, want 1; calls:\n%s", n, r.calls(t))
	}
	if r.stdout != "" || r.stderr != "" {
		t.Fatalf("script printed output: stdout %q stderr %q", r.stdout, r.stderr)
	}
}
