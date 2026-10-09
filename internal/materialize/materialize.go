// Package materialize turns an item's collaborative op-log into the markdown
// a live browser tab would store for it (TASK-2198).
//
// When a tab dies uncleanly its latest typing exists only in item_yjs_updates
// (raw y-protocols frames the server never parses), and items.content stays
// stale. This package runs the editor's REAL JavaScript — Yjs, y-tiptap, the
// Tiptap schema, tiptap-markdown's serializer and the tab's flush pipeline —
// in goja, from a bundle built by web/scripts/build-materializer.mjs and
// embedded as pad.MaterializerJS. Its result is byte-identical to what the
// tab would PATCH on flush; testdata/corpus.json holds cases whose expected
// output came from the live editor, and TestCorpusParity enforces it.
//
// A Runner is one goja runtime: not safe for concurrent use (Materialize
// serialises callers), and memory-hungry enough that the server runs it in a
// separate worker process (`pad __materialize-worker`, RunWorker) rather than
// in its own heap.
package materialize

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// LinkEntry is one item of the workspace link index the tab's flush pipeline
// converts internal links against (markdownToWikiLinks in
// web/src/lib/utils/markdown.ts). It carries exactly the four Item fields that
// function reads: slug, item_number and collection_prefix (which together form
// the ref) and title. cleanBrokenLinks, the pipeline's other step, reads no
// index.
//
// ORDER IS SIGNIFICANT: markdownToWikiLinks takes the FIRST entry that matches
// a link, so the index must be in the order the tab's localIndex.getAll(ws)
// would return. And the tab only runs markdownToWikiLinks when the index is
// NON-EMPTY; the bundle keeps that check (flushPipeline in entry.ts), so an
// empty or nil index skips the conversion entirely rather than running it over
// no items.
//
// A zero ItemNumber or empty CollectionPrefix is omitted, which the JS reads
// as falsy — the same as a tab's item with no ref.
type LinkEntry struct {
	Slug             string `json:"slug"`
	Title            string `json:"title"`
	ItemNumber       int    `json:"item_number,omitempty"`
	CollectionPrefix string `json:"collection_prefix,omitempty"`
}

// Job is one materialization.
type Job struct {
	// Rows are the item's CONTENT-BEARING op-log rows
	// (item_yjs_updates.content_bearing), oldest first, each one raw
	// y-protocols frame exactly as stored.
	Rows [][]byte
	// SchemaVersion is the collab schema version the rows were written under
	// (item_yjs_updates.schema_version). It must equal the bundle's, which is
	// web/src/lib/collab/schemaVersion.ts SCHEMA_VERSION; anything else is
	// refused with ErrSchemaVersion, because replaying another schema's ops is
	// exactly what the collab rebuild exists to prevent. An opaque string,
	// compared exactly, as the collab handler does.
	SchemaVersion string
	// LinkIndex is the workspace link index; see LinkEntry.
	LinkIndex []LinkEntry
	// WorkspaceSlug is the item's workspace. A tab builds attachment URLs
	// from its workspace, and they reach the stored markdown when an
	// attachment sits inside a block serialized as HTML. Empty gives the
	// tab's no-workspace form.
	WorkspaceSlug string
}

var (
	// ErrSchemaVersion refuses a job whose SchemaVersion is not the bundle's.
	ErrSchemaVersion = errors.New("materialize: schema version mismatch")
	// ErrInterrupted reports that the context ended while the job ran. It
	// wraps the context's error (context.DeadlineExceeded or
	// context.Canceled). The Runner stays usable.
	ErrInterrupted = errors.New("materialize: interrupted")
)

// testHookAfterCall runs between the bundle call returning and the watcher
// being stopped. Tests use it to make ctx end in exactly that window, the one
// where an Interrupt lands on a VM that is no longer running.
var testHookAfterCall func()

// Runner is a goja runtime with the materializer bundle loaded.
type Runner struct {
	mu            sync.Mutex
	vm            *goja.Runtime
	materialize   goja.Callable
	obj           *goja.Object
	schemaVersion string
}

// New loads the bundle into a fresh goja runtime and builds the headless
// editor once, so the cost of that lands here and not in the first job.
func New(js []byte) (*Runner, error) {
	vm := goja.New()
	if err := installGlobals(vm); err != nil {
		return nil, err
	}
	if _, err := vm.RunScript("materializer.js", string(js)); err != nil {
		return nil, fmt.Errorf("materialize: load bundle: %w", err)
	}
	mv := vm.Get("Materializer")
	if mv == nil || goja.IsUndefined(mv) || goja.IsNull(mv) {
		return nil, errors.New("materialize: bundle did not define Materializer (a placeholder? run `node scripts/build-materializer.mjs` in web/)")
	}
	r := &Runner{vm: vm, obj: mv.ToObject(vm)}
	fn, err := r.fn("materialize")
	if err != nil {
		return nil, err
	}
	r.materialize = fn
	v, err := r.call("schemaVersion")
	if err != nil {
		return nil, err
	}
	r.schemaVersion = v.String()
	// Warm-up: constructs the editor (schema, serializer).
	if _, err := r.call("schemaSpec"); err != nil {
		return nil, fmt.Errorf("materialize: build headless editor: %w", err)
	}
	return r, nil
}

// SchemaVersion is the collab schema version the bundle was built with.
func (r *Runner) SchemaVersion() string { return r.schemaVersion }

// SchemaSpec returns the headless editor's schema description as JSON.
func (r *Runner) SchemaSpec() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, err := r.call("schemaSpec")
	if err != nil {
		return "", err
	}
	return v.String(), nil
}

// AtBlankPatched reports whether the bundle's serializer runs the vendored
// atBlank patch (see web/src/lib/collab/materializer/atBlank.ts).
func (r *Runner) AtBlankPatched() (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, err := r.call("atBlankPatched")
	if err != nil {
		return false, err
	}
	return v.ToBoolean(), nil
}

type jsJob struct {
	Rows          []string    `json:"rows"`
	LinkIndex     []LinkEntry `json:"link_index"`
	WorkspaceSlug string      `json:"workspace_slug"`
}

// Materialize replays job.Rows and returns the markdown a tab would flush.
//
// When ctx ends while the JavaScript runs, the runtime is interrupted and the
// call returns an error wrapping ErrInterrupted and ctx.Err(); the Runner is
// usable afterwards. A JavaScript exception is returned as an error.
func (r *Runner) Materialize(ctx context.Context, job Job) (string, error) {
	return r.runJob(ctx, r.materialize, "materialize", job)
}

// Snapshot is one op-log compacted into a single frame (TASK-3531).
type Snapshot struct {
	// Markdown is what the rows (and the frame) materialize to.
	Markdown string
	// Frame is a y-protocols Update frame carrying the whole replayed
	// document's state: one op-log row that replays to the same document.
	Frame []byte
}

// Snapshot compacts job's rows into one frame (TASK-3531). The bundle refuses
// (an error) rather than return a frame that could lose anything: a row that
// failed to replay, structs left pending, or a frame that does not replay to
// the same document and markdown.
func (r *Runner) Snapshot(ctx context.Context, job Job) (Snapshot, error) {
	fn, err := r.fn("snapshot")
	if err != nil {
		return Snapshot{}, err
	}
	out, err := r.runJob(ctx, fn, "snapshot", job)
	if err != nil {
		return Snapshot{}, err
	}
	var res struct {
		Markdown string `json:"markdown"`
		Frame    string `json:"frame"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot: decode result: %w", err)
	}
	frame, err := base64.StdEncoding.DecodeString(res.Frame)
	if err != nil || len(frame) == 0 {
		return Snapshot{}, fmt.Errorf("snapshot: result has no frame")
	}
	return Snapshot{Markdown: res.Markdown, Frame: frame}, nil
}

// runJob calls one job function of the bundle (materialize or snapshot) with
// job's JSON, under ctx's interrupt.
func (r *Runner) runJob(ctx context.Context, fn goja.Callable, what string, job Job) (string, error) {
	if job.SchemaVersion != r.schemaVersion {
		return "", fmt.Errorf("%w: job %q, bundle %q", ErrSchemaVersion, job.SchemaVersion, r.schemaVersion)
	}
	rows := make([]string, len(job.Rows))
	for i, b := range job.Rows {
		rows[i] = base64.StdEncoding.EncodeToString(b)
	}
	index := job.LinkIndex
	if index == nil {
		index = []LinkEntry{}
	}
	payload, err := json.Marshal(jsJob{Rows: rows, LinkIndex: index, WorkspaceSlug: job.WorkspaceSlug})
	if err != nil {
		return "", fmt.Errorf("materialize: encode job: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInterrupted, err)
	}

	// The watcher interrupts the VM when ctx ends (a deadline is a timer
	// inside ctx). It must have EXITED before the interrupt flag is cleared,
	// or a late Interrupt could land after ClearInterrupt and kill the next
	// job the moment it starts.
	stop := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		select {
		case <-ctx.Done():
			r.vm.Interrupt(ctx.Err())
		case <-stop:
		}
	}()
	v, err := fn(goja.Undefined(), r.vm.ToValue(string(payload)))
	if testHookAfterCall != nil {
		testHookAfterCall()
	}
	close(stop)
	<-exited
	r.vm.ClearInterrupt()

	if err != nil {
		var ie *goja.InterruptedError
		if errors.As(err, &ie) {
			cause := ctx.Err()
			if cause == nil {
				cause = context.Canceled
			}
			return "", fmt.Errorf("%w: %w", ErrInterrupted, cause)
		}
		return "", fmt.Errorf("%s: %w", what, err)
	}
	return v.String(), nil
}

func (r *Runner) fn(name string) (goja.Callable, error) {
	f, ok := goja.AssertFunction(r.obj.Get(name))
	if !ok {
		return nil, fmt.Errorf("materialize: bundle has no Materializer.%s", name)
	}
	return f, nil
}

// call invokes a bundle function. Callers hold r.mu, except New.
func (r *Runner) call(name string, args ...any) (goja.Value, error) {
	f, err := r.fn(name)
	if err != nil {
		return nil, err
	}
	vals := make([]goja.Value, len(args))
	for i, a := range args {
		vals[i] = r.vm.ToValue(a)
	}
	return f(goja.Undefined(), vals...)
}

// elapsedMs is a millisecond duration for the worker protocol.
func elapsedMs(since time.Time) float64 {
	return float64(time.Since(since).Microseconds()) / 1000
}
