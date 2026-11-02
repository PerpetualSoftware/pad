package storetest

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

type pgTraceRecorder struct {
	testing.TB
	lines []string
}

func (*pgTraceRecorder) Helper() {}
func (r *pgTraceRecorder) Log(args ...any) {
	r.lines = append(r.lines, fmt.Sprint(args...))
}

func TestTracePGConnected(t *testing.T) {
	// Merely configuring Postgres must never enable tracing.
	t.Setenv("PAD_TEST_POSTGRES_URL", "configured-but-unused")
	for _, tc := range []struct {
		trace string
		want  string
	}{
		{"", ""},
		{"0", ""},
		{"true", ""},
		{"1", "PAD_PG_CONNECTED helper=probe"},
	} {
		t.Run("trace="+tc.trace, func(t *testing.T) {
			t.Setenv("PAD_TEST_PG_TRACE", tc.trace)
			if tc.trace == "" {
				if err := os.Unsetenv("PAD_TEST_PG_TRACE"); err != nil {
					t.Fatal(err)
				}
			}
			r := &pgTraceRecorder{}
			TracePGConnected(r, "probe")
			if got := strings.Join(r.lines, "\n"); got != tc.want {
				t.Fatalf("log = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewPostgres_TraceGate(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	if os.Getenv("PAD_PG_TRACE_TEST_CHILD") == "1" {
		_ = NewPostgres(t)
		return
	}
	for _, trace := range []string{"", "1"} {
		t.Run("trace="+trace, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestNewPostgres_TraceGate$", "-test.v")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "PAD_TEST_PG_TRACE=") && !strings.HasPrefix(entry, "PAD_PG_TRACE_TEST_CHILD=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "PAD_PG_TRACE_TEST_CHILD=1")
			if trace != "" {
				cmd.Env = append(cmd.Env, "PAD_TEST_PG_TRACE="+trace)
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				// Connection errors can contain credentials; do not echo output.
				t.Fatalf("Postgres trace probe failed: %v", err)
			}
			got := strings.Contains(string(output), "PAD_PG_CONNECTED")
			if got != (trace == "1") {
				t.Fatalf("marker present = %t for trace=%q", got, trace)
			}
			if trace == "1" && !strings.Contains(string(output), "PAD_PG_CONNECTED helper=storetest.NewPostgres\n") {
				t.Fatal("missing marker from the connected cloned store")
			}
		})
	}
}
