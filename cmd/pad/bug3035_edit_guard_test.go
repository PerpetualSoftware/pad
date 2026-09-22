package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3035: `pad item edit` is a read-modify-write over the whole body. It now
// (1) refuses to open the editor on a stale-body read unless --force, (2) sends
// the seq that seeded the editor as expected_seq, (3) re-reads before saving and
// refuses if the body went stale meanwhile, and (4) preserves the edited text in
// a recovery file whenever the save does not land.

const editSentinel = "EDITOR-RAN-HERE"

// editFixture is a REAL item's shape — ref parts, seq, updated_at and fields —
// because a stub missing what real data always has hides the branch real use
// takes (a field-less, ref-less stub already hid one bug on this surface, #1416).
func editFixture(content, contentState string, seq int64) map[string]any {
	it := map[string]any{
		"id": "i1", "slug": "fix-the-thing", "title": "Fix the thing",
		"collection_slug": "tasks", "collection_prefix": "TASK", "item_number": 5,
		"ref": "TASK-5", "fields": `{"status":"open","priority":"high"}`,
		"content": content, "seq": seq, "updated_at": "2026-09-21T22:53:52Z",
	}
	if contentState != "" {
		it["content_state"] = contentState
	}
	return it
}

type editStub struct {
	mu      sync.Mutex
	gets    []map[string]any // one per GET, last one repeats
	nGet    int
	patches []map[string]any
	// patchStatus: 0/200 answers the item; 409 answers a seq conflict; other
	// codes answer a plain error envelope.
	patchStatus int
}

func (s *editStub) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == http.MethodPatch {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.patches = append(s.patches, body)
		switch s.patchStatus {
		case 0, http.StatusOK:
			c, _ := body["content"].(string)
			_ = json.NewEncoder(w).Encode(editFixture(c, "", 42))
		case http.StatusConflict:
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": "update_conflict", "message": "Item was modified by another writer",
				"details": map[string]any{"ref": "TASK-5", "expected_seq": 41, "actual_seq": 43, "conflict_type": "seq"},
			}})
		default:
			w.WriteHeader(s.patchStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": "internal_error", "message": "boom"}})
		}
		return
	}
	i := s.nGet
	if i >= len(s.gets) {
		i = len(s.gets) - 1
	}
	s.nGet++
	_ = json.NewEncoder(w).Encode(s.gets[i])
}

// runEdit runs `pad item edit TASK-5 [extra...]` against the stub with a fake
// editor that announces itself on stderr and appends a line. It returns stdout,
// stderr, the command error, and the directory recovery files land in.
func runEdit(t *testing.T, stub *editStub, extra ...string) (string, string, error, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(srv.Close)
	setTempHomeMain(t)
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_testtoken")

	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	editorPath := filepath.Join(t.TempDir(), "fake-editor")
	script := "#!/bin/sh\necho " + editSentinel + " >&2\necho edited >> \"$1\"\n"
	if err := os.WriteFile(editorPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake editor: %v", err)
	}
	t.Setenv("EDITOR", editorPath)

	origWS, origFormat := workspaceFlag, formatFlag
	t.Cleanup(func() { workspaceFlag, formatFlag = origWS, origFormat })
	workspaceFlag, formatFlag = "ws", ""

	cmd := editCmd()
	cmd.SetArgs(append([]string{"TASK-5"}, extra...))
	var execErr error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { execErr = cmd.Execute() })
	})
	return stdout, stderr, execErr, tmp
}

func recoveryFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "pad-edit-*.md"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestEditRefusesStaleSeed(t *testing.T) {
	stub := &editStub{gets: []map[string]any{
		editFixture("the previous content", models.ContentOutcomeAppliedPendingFlush, 41)}}
	_, stderr, err, _ := runEdit(t, stub)
	if err == nil {
		t.Fatal("a stale-body read must refuse without --force")
	}
	if strings.Contains(stderr, editSentinel) {
		t.Fatal("the editor opened on a body the server reported stale; the refusal must come BEFORE it")
	}
	if len(stub.patches) != 0 {
		t.Fatalf("a refused edit sent %d PATCH(es)", len(stub.patches))
	}
	for _, want := range []string{"TASK-5", "browser tab", "--force", "after the tab that caused it has closed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q: %v", want, err)
		}
	}
}

// CONTROL for the refusal: a current row opens the editor and saves. Without
// this leg a command that refused EVERYTHING would pass the test above.
func TestEditCurrentRowSavesWithTheSeedSeq(t *testing.T) {
	stub := &editStub{gets: []map[string]any{editFixture("body", "", 41)}}
	stdout, stderr, err, tmp := runEdit(t, stub)
	if err != nil {
		t.Fatalf("edit: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, editSentinel) {
		t.Fatal("the editor never ran")
	}
	if len(stub.patches) != 1 {
		t.Fatalf("want exactly one PATCH, got %d", len(stub.patches))
	}
	p := stub.patches[0]
	if p["content"] != "body"+"edited\n" {
		t.Fatalf("PATCH content = %q", p["content"])
	}
	if p["expected_seq"] != float64(41) {
		t.Fatalf("the save must carry the seq that SEEDED the editor (41); got %v", p["expected_seq"])
	}
	if _, has := p["expected_updated_at"]; has {
		t.Fatalf("with a seq available the weak token must not be sent too: %v", p)
	}
	if stub.nGet != 2 {
		t.Fatalf("want the seed read plus one pre-save re-read (2 GETs), got %d", stub.nGet)
	}
	if !strings.Contains(stdout, `Updated TASK-5 "Fix the thing"`) {
		t.Fatalf("success line: %q", stdout)
	}
	if f := recoveryFiles(t, tmp); len(f) != 0 {
		t.Fatalf("a successful save must not leave a recovery file: %v", f)
	}
}

func TestEditFallsBackToUpdatedAtWhenNoSeq(t *testing.T) {
	stub := &editStub{gets: []map[string]any{editFixture("body", "", 0)}}
	if _, stderr, err, _ := runEdit(t, stub); err != nil {
		t.Fatalf("edit: %v\n%s", err, stderr)
	}
	p := stub.patches[0]
	if _, has := p["expected_seq"]; has {
		t.Fatalf("a seq the server never issued (0) must not be sent: %v", p)
	}
	if p["expected_updated_at"] != "2026-09-21T22:53:52Z" {
		t.Fatalf("with no seq the save must still be guarded by updated_at; got %v", p["expected_updated_at"])
	}
}

func TestEditForceOpensAStaleSeedAndStillSendsTheToken(t *testing.T) {
	stub := &editStub{gets: []map[string]any{
		editFixture("the previous content", models.ContentOutcomeAppliedPendingFlush, 41)}}
	_, stderr, err, _ := runEdit(t, stub, "--force")
	if err != nil {
		t.Fatalf("edit --force: %v\n%s", err, stderr)
	}
	if len(stub.patches) != 1 || stub.patches[0]["expected_seq"] != float64(41) {
		t.Fatalf("--force skips the stale refusal, not the concurrency guard: %v", stub.patches)
	}
}

func TestEditConflictPreservesTheEditedText(t *testing.T) {
	stub := &editStub{gets: []map[string]any{editFixture("body", "", 41)}, patchStatus: http.StatusConflict}
	_, stderr, err, tmp := runEdit(t, stub)
	if err == nil {
		t.Fatal("a 409 must fail the command")
	}
	files := recoveryFiles(t, tmp)
	if len(files) != 1 {
		t.Fatalf("want one recovery file, got %v (stderr: %s)", files, stderr)
	}
	got, _ := os.ReadFile(files[0])
	if string(got) != "body"+"edited\n" {
		t.Fatalf("the recovery file must hold the EDITED text byte for byte; got %q", got)
	}
	if !strings.Contains(err.Error(), files[0]) {
		t.Errorf("the returned error must carry the path, for a caller that discards stderr: %v", err)
	}
	pathAt := strings.Index(stderr, files[0])
	markerAt := strings.Index(stderr, "pad-structured-error/v1:")
	if pathAt < 0 || markerAt < 0 {
		t.Fatalf("stderr must carry both the path and the structured conflict:\n%s", stderr)
	}
	if pathAt > markerAt {
		t.Errorf("the recovery path must print BEFORE the conflict block:\n%s", stderr)
	}
}

func TestEditAnyFailedSavePreservesTheEditedText(t *testing.T) {
	stub := &editStub{gets: []map[string]any{editFixture("body", "", 41)}, patchStatus: http.StatusInternalServerError}
	_, _, err, tmp := runEdit(t, stub)
	if err == nil {
		t.Fatal("a 500 must fail the command")
	}
	if files := recoveryFiles(t, tmp); len(files) != 1 {
		t.Fatalf("a non-conflict failure loses the same text; want one recovery file, got %v", files)
	}
}

// A tab starts typing while $EDITOR is open: the op-log moves, the row's seq
// does not, so only the re-read can see it.
func TestEditRefusesWhenTheBodyWentStaleWhileEditing(t *testing.T) {
	stub := &editStub{gets: []map[string]any{
		editFixture("body", "", 41),
		editFixture("body", models.ContentOutcomeAppliedPendingFlush, 41),
	}}
	_, stderr, err, tmp := runEdit(t, stub)
	if err == nil {
		t.Fatal("a body that went stale during editing must refuse the save")
	}
	if !strings.Contains(stderr, editSentinel) {
		t.Fatal("premise: the editor must have run (the seed read was current)")
	}
	if len(stub.patches) != 0 {
		t.Fatalf("the save went out anyway: %v", stub.patches)
	}
	if files := recoveryFiles(t, tmp); len(files) != 1 {
		t.Fatalf("the refused save must preserve the edit; got %v", files)
	}
}

func TestEditNoChangesSendsNothing(t *testing.T) {
	stub := &editStub{gets: []map[string]any{editFixture("body", "", 41)}}
	// An editor that saves the file untouched.
	noop := filepath.Join(t.TempDir(), "noop-editor")
	if err := os.WriteFile(noop, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(srv.Close)
	setTempHomeMain(t)
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_testtoken")
	t.Setenv("EDITOR", noop)
	origWS := workspaceFlag
	t.Cleanup(func() { workspaceFlag = origWS })
	workspaceFlag = "ws"
	cmd := editCmd()
	cmd.SetArgs([]string{"TASK-5"})
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	if err != nil || !strings.Contains(out, "No changes.") || len(stub.patches) != 0 {
		t.Fatalf("an unchanged edit must say so and send nothing: err=%v out=%q patches=%d", err, out, len(stub.patches))
	}
}
