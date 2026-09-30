package materialize

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dop251/goja"

	pad "github.com/PerpetualSoftware/pad"
	"github.com/PerpetualSoftware/pad/internal/collab"
)

// sharedRunner is one Runner over the embedded bundle for the tests that do
// not disturb it. Loading costs about a second.
var (
	sharedOnce   sync.Once
	sharedRunner *Runner
	sharedErr    error
)

func runner(t *testing.T) *Runner {
	t.Helper()
	sharedOnce.Do(func() { sharedRunner, sharedErr = New(pad.MaterializerJS) })
	if sharedErr != nil {
		t.Fatalf("load embedded bundle: %v", sharedErr)
	}
	return sharedRunner
}

type corpusCase struct {
	Name          string      `json:"name"`
	Rows          []string    `json:"rows"`
	LinkIndex     []LinkEntry `json:"link_index"`
	WorkspaceSlug string      `json:"workspace_slug"`
	Expected      string      `json:"expected"`
}

func loadCorpus(t *testing.T) []corpusCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []corpusCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("empty corpus")
	}
	return cases
}

func (c corpusCase) job(t *testing.T, schema string) Job {
	t.Helper()
	j := Job{SchemaVersion: schema, LinkIndex: c.LinkIndex, WorkspaceSlug: c.WorkspaceSlug}
	for _, s := range c.Rows {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		j.Rows = append(j.Rows, b)
	}
	return j
}

// TestCorpusParity: the embedded bundle, in goja, reproduces the live editor's
// flush output byte for byte. The corpus's `expected` values were produced by
// a real Editor.svelte mount (materializerParity.svelte.test.ts, regenerate
// with PAD_GEN_MATERIALIZE_CORPUS=1), with prosemirror-markdown's UPSTREAM
// atBlank, so this also checks the vendored atBlank patch end to end.
func TestCorpusParity(t *testing.T) {
	r := runner(t)
	cases := loadCorpus(t)
	for _, c := range cases {
		got, err := r.Materialize(context.Background(), c.job(t, r.SchemaVersion()))
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		if got != c.Expected {
			t.Errorf("%s: output differs from the live editor's\n--- got ---\n%s\n--- want ---\n%s", c.Name, got, c.Expected)
		}
	}
	t.Logf("%d corpus cases", len(cases))
}

// The bundle's schema version is web/src/lib/collab/schemaVersion.ts's, and
// it must be the server's, or the materializer would accept rows the collab
// room would refuse (or the reverse).
func TestBundleSchemaVersionMatchesCollab(t *testing.T) {
	if got := runner(t).SchemaVersion(); got != collab.DefaultSchemaVersion {
		t.Fatalf("bundle schema version %q, collab.DefaultSchemaVersion %q", got, collab.DefaultSchemaVersion)
	}
}

func TestSchemaVersionMismatchRefused(t *testing.T) {
	r := runner(t)
	for _, v := range []string{"", r.SchemaVersion() + "0", "2"} {
		_, err := r.Materialize(context.Background(), Job{SchemaVersion: v})
		if !errors.Is(err, ErrSchemaVersion) {
			t.Errorf("schema %q: got %v, want ErrSchemaVersion", v, err)
		}
	}
	// And the matching version is accepted (an empty op-log is an empty doc).
	md, err := r.Materialize(context.Background(), Job{SchemaVersion: r.SchemaVersion()})
	if err != nil || md != "" {
		t.Fatalf("empty job: %q, %v", md, err)
	}
}

// TestAtBlankPatched guards the vendored prosemirror-markdown atBlank patch
// (web/src/lib/collab/materializer/atBlank.ts): without it serialization is
// quadratic under goja.
func TestAtBlankPatched(t *testing.T) {
	ok, err := runner(t).AtBlankPatched()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the embedded bundle's serializer does not run the vendored atBlank patch")
	}
}

// TestAtBlankAgreesWithUpstream: the patched atBlank and the saved upstream
// implementation (/(^|\n)$/.test(out)) agree on generated serializer outputs,
// with the characters that could trip an end-of-input test over-represented.
func TestAtBlankAgreesWithUpstream(t *testing.T) {
	r := runner(t)
	alphabet := []string{"a", " ", "\n", "\r", "\r\n", " ", " ", "\n\n", "$", "^", "\t", "é", "🎉", "\x00", "\\n", "�", "\v", "\f"}
	gen := rand.New(rand.NewSource(2198))
	outs := []string{"", "\n", "a", "a\n", "\n\n", "a\r", "\r", "a ", " "}
	for i := 0; i < 5000; i++ {
		var sb strings.Builder
		for n := gen.Intn(12); n > 0; n-- {
			sb.WriteString(alphabet[gen.Intn(len(alphabet))])
		}
		outs = append(outs, sb.String())
	}
	sawTrue, sawFalse := false, false
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, out := range outs {
		v, err := r.call("atBlankPair", out)
		if err != nil {
			t.Fatal(err)
		}
		var pair [2]bool
		if err := json.Unmarshal([]byte(v.String()), &pair); err != nil {
			t.Fatal(err)
		}
		if pair[0] != pair[1] {
			t.Errorf("out=%q: upstream %v, patched %v", out, pair[0], pair[1])
		}
		sawTrue = sawTrue || pair[0]
		sawFalse = sawFalse || !pair[0]
	}
	if !sawTrue || !sawFalse {
		t.Fatalf("generator reached only one answer (true %v, false %v)", sawTrue, sawFalse)
	}
}

// replaceMaterialize swaps the bundle function Materialize calls, to drive the
// interrupt machinery with a job that never ends on its own.
func replaceMaterialize(t *testing.T, r *Runner, src string) func() {
	t.Helper()
	v, err := r.vm.RunString(src)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := goja.AssertFunction(v)
	if !ok {
		t.Fatal("not a function")
	}
	orig := r.materialize
	r.materialize = f
	return func() { r.materialize = orig }
}

func TestInterruptOnDeadlineAndReuse(t *testing.T) {
	r, err := New(pad.MaterializerJS)
	if err != nil {
		t.Fatal(err)
	}
	c := loadCorpus(t)[0]
	job := c.job(t, r.SchemaVersion())

	restore := replaceMaterialize(t, r, `(function () { for (;;) {} })`)
	const deadline = 50 * time.Millisecond
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		start := time.Now()
		done := make(chan error, 1)
		go func() { _, err := r.Materialize(ctx, job); done <- err }()
		var err error
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Materialize did not return 10s after a 50ms deadline: the runtime was not interrupted")
		}
		took := time.Since(start)
		cancel()
		if !errors.Is(err, ErrInterrupted) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("busy loop: got %v, want ErrInterrupted wrapping DeadlineExceeded", err)
		}
		if took > deadline+time.Second {
			t.Fatalf("interrupt took %v after a %v deadline", took, deadline)
		}
		t.Logf("interrupted after %v (deadline %v)", took, deadline)
	}
	// Cancellation, not only a deadline.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, err := r.Materialize(ctx, job); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: got %v", err)
	}
	restore()

	// The Runner is usable afterwards and still exact.
	got, err := r.Materialize(context.Background(), job)
	if err != nil {
		t.Fatalf("after interrupt: %v", err)
	}
	if got != c.Expected {
		t.Fatalf("after interrupt: output differs from corpus %s", c.Name)
	}
}

// A context ending AFTER the job's JavaScript returned, but before the
// watcher is stopped, makes the watcher Interrupt a VM that is no longer
// running. goja keeps that flag set, so without ClearInterrupt the NEXT job
// would be killed on its first instruction. The hook puts the cancel in
// exactly that window.
func TestNoInterruptLeaksIntoNextJob(t *testing.T) {
	r, err := New(pad.MaterializerJS)
	if err != nil {
		t.Fatal(err)
	}
	c := loadCorpus(t)[0]
	job := c.job(t, r.SchemaVersion())
	ctx, cancel := context.WithCancel(context.Background())
	testHookAfterCall = func() {
		cancel()
		time.Sleep(50 * time.Millisecond) // let the watcher take ctx.Done
	}
	got, err := r.Materialize(ctx, job)
	testHookAfterCall = nil
	if err != nil || got != c.Expected {
		t.Fatalf("the job itself completed before the cancel: %v", err)
	}
	got, err = r.Materialize(context.Background(), job)
	if err != nil {
		t.Fatalf("next job: %v", err)
	}
	if got != c.Expected {
		t.Fatal("next job output differs")
	}
}

func TestExpiredContextDoesNotRun(t *testing.T) {
	r := runner(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Materialize(ctx, Job{SchemaVersion: r.SchemaVersion()}); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("got %v", err)
	}
}

func TestPlaceholderBundleRefused(t *testing.T) {
	if _, err := New([]byte("placeholder")); err == nil {
		t.Fatal("a non-bundle loaded without error")
	}
	if _, err := New([]byte("var x = 1;")); err == nil || !strings.Contains(err.Error(), "Materializer") {
		t.Fatalf("got %v", err)
	}
}

// A malformed frame is skipped and the rest still apply, as in a tab.
func TestMalformedRowSkipped(t *testing.T) {
	r := runner(t)
	c := loadCorpus(t)[1]
	job := c.job(t, r.SchemaVersion())
	junk := [][]byte{{}, {0}, {0, 7, 1, 0}, {0, 2, 200}, {1, 2, 3}, {0xff, 0xff, 0xff, 0xff, 0xff}}
	job.Rows = append(append(append([][]byte{}, junk...), job.Rows[:len(job.Rows)/2]...), append(junk, job.Rows[len(job.Rows)/2:]...)...)
	got, err := r.Materialize(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if got != c.Expected {
		t.Fatalf("junk frames changed the output of %s", c.Name)
	}
}
