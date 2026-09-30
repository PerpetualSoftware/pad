package materialize

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// procStatusKB reads one "Name:   N kB" line of /proc/<pid>/status.
func procStatusKB(t *testing.T, pid int, name string) uint64 {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, name+":") {
			n, _ := strconv.ParseUint(strings.Fields(l)[1], 10, 64)
			return n << 10
		}
	}
	t.Fatalf("no %s", name)
	return 0
}

// TestSupervisorBaselineMeasurements records, for a REAL loaded worker, its
// address-space baseline and how much address space and resident memory the
// corpus and a moderate adversarial document add — the evidence behind
// MinMemLimit and MaxPlausibleBaseline. Run with PAD_MATERIALIZE_MEASURE=1.
func TestSupervisorBaselineMeasurements(t *testing.T) {
	if os.Getenv("PAD_MATERIALIZE_MEASURE") == "" {
		t.Skip("measurement; set PAD_MATERIALIZE_MEASURE=1")
	}
	r := runner(t)
	cases := loadCorpus(t)
	for _, procs := range []string{"1", "2", "8", "64"} {
		var pid int
		h := newHarness(t, "worker", func(c *SupervisorConfig) {
			c.Timeout = 60 * time.Second
			c.capOverride = fakeCap
			c.extraEnv = []string{"GOMAXPROCS=" + procs}
			c.onSpawn = func(p int) { pid = p }
		})
		if _, err := h.s.Materialize(context.Background(), Job{SchemaVersion: r.SchemaVersion()}); err != nil {
			t.Fatal(err)
		}
		vsz0, rss0 := procStatusKB(t, pid, "VmSize"), procStatusKB(t, pid, "VmRSS")
		for _, tc := range cases {
			if _, err := h.s.Materialize(context.Background(), tc.job(t, r.SchemaVersion())); err != nil {
				t.Fatal(err)
			}
		}
		vszC, rssC := procStatusKB(t, pid, "VmPeak"), procStatusKB(t, pid, "VmHWM")
		line := "GOMAXPROCS=" + procs + ": baseline VmSize " + formatBytes(vsz0) + " (" + strconv.FormatUint(vsz0>>20, 10) + " MiB), VmRSS " + strconv.FormatUint(rss0>>20, 10) + " MiB; after " + strconv.Itoa(len(cases)) + " corpus cases: +" + strconv.FormatUint((vszC-vsz0)>>20, 10) + " MiB address space (peak), +" + strconv.FormatUint((rssC-rss0)>>20, 10) + " MiB resident (peak)"
		for _, depth := range []int{1000, 2000, 4000} {
			_, err := h.s.Materialize(context.Background(), Job{Rows: [][]byte{deepNestFrame(depth, "blockquote")}, SchemaVersion: r.SchemaVersion()})
			if err != nil {
				line += "; depth " + strconv.Itoa(depth) + ": " + err.Error()
				break
			}
			line += "; depth " + strconv.Itoa(depth) + ": +" + strconv.FormatUint((procStatusKB(t, pid, "VmPeak")-vsz0)>>20, 10) + " MiB AS, +" + strconv.FormatUint((procStatusKB(t, pid, "VmHWM")-rss0)>>20, 10) + " MiB RSS"
		}
		t.Log(line)
		_ = h.s.Close()
	}
}
