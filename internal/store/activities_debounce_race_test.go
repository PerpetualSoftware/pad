package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2770. CreateActivityDebounced merges by reading a row's metadata,
// combining it in Go, and writing the result back — two statements. Two
// writers that select the SAME row both read the pre-merge blob, each merges
// its own change into it, and the second UPDATE erases the first one's change
// entry with no error and no trace.
//
// These tests drive that interleaving through the Store's afterDebounceRead
// seam rather than racing goroutines: the defect needs the competing write to
// land strictly between one call's read and its write, and two real
// goroutines produce that ordering only sometimes. A test that reproduces a
// race sometimes is a detector with an unknown rate, and a 1-in-10 detector
// reads as coverage.
//
// What they assert is what the WRONG behaviour DOES — a change entry missing
// from the surviving row — not the end state a correct and a broken merge
// share (one row, still present, still recently stamped).

// debounceWriter builds one writer's activity, all fields but the change text
// held constant so every call selects the same row: same document, same
// account, same actor kind, same agent name (BUG-2763's narrowing).
func debounceWriter(wsID, docID, userID, changes string) models.Activity {
	return models.Activity{
		WorkspaceID: wsID,
		DocumentID:  docID,
		Action:      "updated",
		Actor:       "agent",
		Source:      "cli",
		UserID:      userID,
		Metadata:    `{"agent":"rook","changes":"` + changes + `"}`,
	}
}

// updatedRowsFor returns the "updated" activity rows on the document, as
// ListDocumentActivity reports them — which means a default page of 20,
// ordered by created_at with no tie-break, NOT "every row, newest first"
// (codex round 8). Every caller here first asserts the row COUNT it
// expects and then reads rows without depending on their order, so neither
// limitation is load-bearing; a future test that creates more than a
// handful of rows, or that cares which of two same-second rows comes
// first, must not use this helper as-is.
func updatedRowsFor(t *testing.T, s *Store, docID string) []models.Activity {
	t.Helper()
	rows, err := s.ListDocumentActivity(docID, models.ActivityListParams{Action: "updated"})
	if err != nil {
		t.Fatalf("list document activity: %v", err)
	}
	return rows
}

// changesOf reads the accumulated "changes" text out of an activity's
// metadata. Local to the test: the store writes this key through
// mergeActivityMeta and the timeline renders it, but nothing in models
// exposes a reader for it.
func changesOf(t *testing.T, a models.Activity) string {
	t.Helper()
	if a.Metadata == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(a.Metadata), &m); err != nil {
		t.Fatalf("metadata is not JSON: %q (%v)", a.Metadata, err)
	}
	changes, _ := m["changes"].(string)
	return changes
}

// TestCreateActivityDebounced_ConcurrentMergeKeepsBothChanges is the
// regression test for the lost update itself. A competing writer merges into
// the chosen row inside the read-write window; the call under test must
// notice its compare-and-set lost, re-read, and merge into what the winner
// left — so the surviving row names BOTH changes.
//
// Against the unfixed code this fails on the competitor's change: the
// second UPDATE overwrites the blob the competitor wrote.
//
// WHAT IT CANNOT DISCRIMINATE (codex round 7): an implementation that
// re-read the row and re-merged immediately before an unconditional UPDATE
// would also pass, because the seam fires before that re-read. The residual
// window — a competitor landing between such a re-read and the write — is
// not reachable without a second seam, and is the window the CAS exists to
// close. That the token must be the merged-FROM blob rather than a fresh
// re-read is pinned instead by the mutation matrix (M5) and by
// TestMergeIntoUnlinkedActivity_RefusesMovedRow at the statement level.
func TestCreateActivityDebounced_ConcurrentMergeKeepsBothChanges(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Race")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")
	const userID = ""

	// The run this writer will try to extend.
	if _, err := s.CreateActivityDebounced(debounceWriter(ws.ID, doc.ID, userID, "title: a → b")); err != nil {
		t.Fatalf("seed activity: %v", err)
	}

	// One competing merge, fired between the read and the write of the call
	// below. It runs once: the retry must then find a clear field and
	// succeed, which is what makes the assertion about content rather than
	// about how many times we looped.
	fired := 0
	var hook func()
	hook = func() {
		if fired > 0 {
			return
		}
		fired++
		s.afterDebounceRead = nil
		defer func() { s.afterDebounceRead = hook }()
		if _, err := s.CreateActivityDebounced(debounceWriter(ws.ID, doc.ID, userID, "status: open → done")); err != nil {
			t.Errorf("competing write: %v", err)
		}
	}
	s.afterDebounceRead = hook

	if _, err := s.CreateActivityDebounced(debounceWriter(ws.ID, doc.ID, userID, "priority: low → high")); err != nil {
		t.Fatalf("contended write: %v", err)
	}
	s.afterDebounceRead = nil

	if fired != 1 {
		t.Fatalf("seam fired %d times, want 1 — the test did not exercise the read-write window", fired)
	}

	rows := updatedRowsFor(t, s, doc.ID)
	if len(rows) != 1 {
		t.Fatalf("want the run coalesced into 1 row, got %d — a contended merge must retry into the winner's row, not start a new run", len(rows))
	}
	changes := changesOf(t, rows[0])
	for _, want := range []string{"title", "status", "priority"} {
		if !strings.Contains(changes, want) {
			t.Errorf("surviving row lost the %q change: changes = %q", want, changes)
		}
	}
}

// TestCreateActivityDebounced_ContentionPastTheBoundKeepsTheChange pins the
// give-up path. A competitor that wins EVERY attempt exhausts
// debounceMergeAttempts; the call must then write its own row rather than
// loop forever or drop the change text. Presentation degrades (an extra
// timeline entry), content does not.
func TestCreateActivityDebounced_ContentionPastTheBoundKeepsTheChange(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Race")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")
	const userID = ""

	if _, err := s.CreateActivityDebounced(debounceWriter(ws.ID, doc.ID, userID, "title: a → b")); err != nil {
		t.Fatalf("seed activity: %v", err)
	}

	fired := 0
	var hook func()
	hook = func() {
		fired++
		s.afterDebounceRead = nil
		defer func() { s.afterDebounceRead = hook }()
		// A different change text every time, so every attempt's
		// compare-and-set finds a blob it has not seen.
		if _, err := s.CreateActivityDebounced(debounceWriter(ws.ID, doc.ID, userID, "field"+string(rune('0'+fired))+": x → y")); err != nil {
			t.Errorf("competing write %d: %v", fired, err)
		}
	}
	s.afterDebounceRead = hook

	if _, err := s.CreateActivityDebounced(debounceWriter(ws.ID, doc.ID, userID, "priority: low → high")); err != nil {
		t.Fatalf("contended write: %v", err)
	}
	s.afterDebounceRead = nil

	if fired != debounceMergeAttempts {
		t.Errorf("seam fired %d times, want %d (one per attempt) — the loop is not bounded where it claims to be", fired, debounceMergeAttempts)
	}

	rows := updatedRowsFor(t, s, doc.ID)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows after giving up (the contended run plus a fresh one), got %d", len(rows))
	}
	all := changesOf(t, rows[0]) + "; " + changesOf(t, rows[1])
	if !strings.Contains(all, "priority") {
		t.Error("giving up dropped the caller's change entirely; it must land on a new row")
	}
	// The competitors' changes must survive too. Without this, an
	// accumulator that discarded everything it merged over would still pass
	// on the `priority` assertion alone (codex round 7).
	for i := 1; i <= debounceMergeAttempts; i++ {
		want := "field" + string(rune('0'+i))
		if !strings.Contains(all, want) {
			t.Errorf("contention lost the competing %s change; changes across both rows = %q", want, all)
		}
	}
}

// TestMergeIntoUnlinkedActivity_RefusesMovedRow drives the compare-and-set
// arm directly, on a row NO comment links — so the only thing that can refuse
// the write is the stale expectation. The control leg (a matching
// expectation, same row, same call) is what makes the refusal evidence of a
// predicate rather than of a merge that no longer works at all.
func TestMergeIntoUnlinkedActivity_RefusesMovedRow(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "CAS")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	id, err := s.CreateActivity(models.Activity{
		WorkspaceID: ws.ID, DocumentID: doc.ID,
		Action: "updated", Actor: "agent", Source: "cli",
		Metadata: `{"agent":"rook","changes":"title: a → b"}`,
	})
	if err != nil {
		t.Fatalf("create activity: %v", err)
	}
	metaOf := func() string {
		var m string
		if err := s.db.QueryRow(s.q(`SELECT metadata FROM activities WHERE id = ?`), id).Scan(&m); err != nil {
			t.Fatalf("read metadata: %v", err)
		}
		return m
	}
	current := metaOf()

	written, err := s.mergeIntoUnlinkedActivity(id, `{"agent":"rook","changes":"someone else's read"}`, `{"agent":"rook","changes":"clobber"}`, now())
	if err != nil {
		t.Fatalf("merge with stale expectation: %v", err)
	}
	if written {
		t.Error("merge reported written despite a stale expectation")
	}
	if got := metaOf(); got != current {
		t.Errorf("refused merge still wrote: metadata = %q, want %q", got, current)
	}

	written, err = s.mergeIntoUnlinkedActivity(id, current, `{"agent":"rook","changes":"title: a → c"}`, now())
	if err != nil {
		t.Fatalf("control merge: %v", err)
	}
	if !written {
		t.Fatal("control: a matching expectation on an unlinked row was refused")
	}
	if got := changesOf(t, models.Activity{Metadata: metaOf()}); !strings.Contains(got, "a → c") {
		t.Errorf("control: row not updated, changes = %q", got)
	}
}

// TestDebounceRowUnchanged covers the classifier that tells the merge
// UPDATE's two refusal arms apart. Its answer decides between two OPPOSITE
// dispositions — retry, or start a fresh row — so each of its three inputs
// gets a leg.
func TestDebounceRowUnchanged(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Classify")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	id, err := s.CreateActivity(models.Activity{
		WorkspaceID: ws.ID, DocumentID: doc.ID,
		Action: "updated", Actor: "agent", Source: "cli",
		Metadata: `{"agent":"rook","changes":"title: a → b"}`,
	})
	if err != nil {
		t.Fatalf("create activity: %v", err)
	}
	var current string
	if err := s.db.QueryRow(s.q(`SELECT metadata FROM activities WHERE id = ?`), id).Scan(&current); err != nil {
		t.Fatalf("read metadata: %v", err)
	}

	unchanged, err := s.debounceRowUnchanged(id, current)
	if err != nil {
		t.Fatalf("unchanged row: %v", err)
	}
	if !unchanged {
		t.Error("row carrying the exact blob reported as changed — a frozen row would be retried instead of starting a fresh run")
	}

	unchanged, err = s.debounceRowUnchanged(id, `{"agent":"rook","changes":"something else"}`)
	if err != nil {
		t.Fatalf("moved row: %v", err)
	}
	if unchanged {
		t.Error("row carrying a different blob reported as unchanged — a contended merge would be abandoned instead of retried")
	}

	unchanged, err = s.debounceRowUnchanged("no-such-activity-id", current)
	if err != nil {
		t.Fatalf("missing row: %v", err)
	}
	if unchanged {
		t.Error("missing row reported as unchanged")
	}

	// A store ERROR must not read as an answer. Without this leg, a
	// classifier that swallowed errors into "unchanged" would pass every
	// other leg here (codex round 7) — and "unchanged" is the answer that
	// makes the caller abandon a merge it could have retried.
	broken := testStore(t)
	if err := broken.DB().Close(); err != nil {
		t.Fatalf("close store db: %v", err)
	}
	unchanged, err = broken.debounceRowUnchanged(id, current)
	if err == nil {
		t.Error("a closed database produced no error from the classifier")
	}
	if unchanged {
		t.Error("a failed probe reported the row as unchanged")
	}
}

// TestCreateActivityDebounced_FrozenRowIsNotRetried pins the OTHER half of
// the refusal classifier. A comment-linked row refuses the merge for a reason
// that will not change no matter how many times it is asked, so the call must
// start a fresh run on the FIRST refusal rather than spending the retry
// budget rediscovering it.
//
// The end state — a new row — is the same either way, which is precisely why
// this asserts the seam's fire COUNT: it is the only observable that
// distinguishes "recognised the freeze" from "retried until it gave up".
//
// NOT a counterfactual for BUG-2770: the pre-fix code also made exactly one
// attempt, so this test passes against it. Its target is the classifier —
// it fails when the classifier stops distinguishing the two refusal arms
// (matrix M3) — and it is listed here so nobody reads it as evidence for
// the CAS.
func TestCreateActivityDebounced_FrozenRowIsNotRetried(t *testing.T) {
	s, item := agentNameFixture(t)

	writer := models.Activity{
		WorkspaceID: item.WorkspaceID,
		DocumentID:  item.ID,
		Action:      "updated",
		Actor:       "agent",
		Source:      "cli",
		Metadata:    `{"agent":"rook","changes":"title: a → b"}`,
	}
	first, err := s.CreateActivityDebounced(writer)
	if err != nil {
		t.Fatalf("seed activity: %v", err)
	}
	if _, err := s.CreateComment(item.WorkspaceID, item.ID, "", models.CommentCreate{
		Body: "pinned to that entry", CreatedBy: "agent", Source: "cli", ActivityID: first,
	}); err != nil {
		t.Fatalf("link comment: %v", err)
	}

	attempts := 0
	s.afterDebounceRead = func() { attempts++ }

	next := writer
	next.Metadata = `{"agent":"rook","changes":"status: open → done"}`
	second, err := s.CreateActivityDebounced(next)
	if err != nil {
		t.Fatalf("update after a linked row: %v", err)
	}
	s.afterDebounceRead = nil

	if second == first {
		t.Fatal("merged into a comment-linked row; it must be frozen")
	}
	if attempts != 1 {
		t.Errorf("seam fired %d times against a comment-linked row, want 1 — a freeze is not contention and must not consume the retry budget", attempts)
	}
}

// TestMergeActivityMeta_NullBlobDoesNotPanic covers the blob that is valid
// JSON, valid JSONB, storable in both dialects' columns — and unmarshals to a
// nil map. The overlay writes into that map, and assignment to a nil map
// panics, so this is a crash on a write path rather than a bad merge.
//
// Both orders are exercised because only ONE of them is the defect: a nil
// INCOMING map is merely ranged over (safe), while a nil EXISTING map is
// assigned into. A test that only passed `null` as the incoming side would
// go green against the unguarded code and say nothing.
func TestMergeActivityMeta_NullBlobDoesNotPanic(t *testing.T) {
	got := mergeActivityMeta("null", `{"agent":"rook","changes":"status: open → done"}`)
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("merged blob is not JSON: %q (%v)", got, err)
	}
	if m["changes"] != "status: open → done" {
		t.Errorf("null existing blob lost the incoming change: %q", got)
	}
	if m["agent"] != "rook" {
		t.Errorf("null existing blob lost the incoming agent: %q", got)
	}

	// The guard must produce a REAL merge, not a null-shaped special case:
	// same-field runs still collapse through it (codex round 7).
	got = mergeActivityMeta("null", `{"changes":"title: a → b; title: b → c"}`)
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("merged blob is not JSON: %q (%v)", got, err)
	}
	if m["changes"] != "title: a → c" {
		t.Errorf("collapsing did not run through the null-existing path: %q", got)
	}

	got = mergeActivityMeta(`{"agent":"rook","changes":"title: a → b"}`, "null")
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("merged blob is not JSON: %q (%v)", got, err)
	}
	if m["changes"] != "title: a → b" {
		t.Errorf("null incoming blob dropped the existing change: %q", got)
	}
}

// TestCreateActivityDebounced_NullMetadataRowSurvives drives the same blob
// through the store, since a panic in mergeActivityMeta is only interesting
// because a stored row can reach it. A row carrying `null` is what an older
// or hand-written caller can leave behind: CreateActivity only substitutes
// "{}" for the EMPTY string.
func TestCreateActivityDebounced_NullMetadataRowSurvives(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Null")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	id, err := s.CreateActivity(models.Activity{
		WorkspaceID: ws.ID, DocumentID: doc.ID,
		Action: "updated", Actor: "user", Source: "web",
		Metadata: "null",
	})
	if err != nil {
		t.Fatalf("create activity with null metadata: %v", err)
	}

	if _, err := s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID, DocumentID: doc.ID,
		Action: "updated", Actor: "user", Source: "web",
		Metadata: `{"changes":"status: open → done"}`,
	}); err != nil {
		t.Fatalf("debounced update over a null-metadata row: %v", err)
	}

	rows := updatedRowsFor(t, s, doc.ID)
	if len(rows) != 1 {
		t.Fatalf("want the null row extended, got %d rows", len(rows))
	}
	if rows[0].ID != id {
		t.Errorf("merged into %s, want the existing null-metadata row %s", rows[0].ID, id)
	}
	if got := changesOf(t, rows[0]); got != "status: open → done" {
		t.Errorf("changes on the merged row = %q", got)
	}
}

// TestCreateActivityDebounced_RetryStampsTheRetryTime pins that the merge's
// timestamp is taken per ATTEMPT. A retry that reused the instant captured
// before the loop would backdate a row whose newest change is younger than
// that stamp — created_at is what the coalescing window, the timeline's
// ordering and the status-transition backfill all read.
//
// The competing write holds the second boundary open (RFC3339 stores whole
// seconds, so a sub-second difference is unobservable). That sleep is the
// same device TestCreateActivityDebounced_TimestampBumped already uses, and
// The sleep runs inside the hook, so the assertion below is only meaningful
// if the hook actually ran — which is why `fired` is checked rather than
// assumed (codex round 8).
//
// Boundary (codex round 7): an implementation with no CAS at all that
// simply called now() at write time would also pass, so this is evidence
// about WHERE the timestamp is taken, not that a retry happened. The retry
// itself is pinned by the tests above; this one dies to the matrix mutation
// that hoists ts back out of the loop (M7).
func TestCreateActivityDebounced_RetryStampsTheRetryTime(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Stamp")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")
	const userID = ""

	if _, err := s.CreateActivityDebounced(debounceWriter(ws.ID, doc.ID, userID, "title: a → b")); err != nil {
		t.Fatalf("seed activity: %v", err)
	}
	beforeLoop := time.Now().UTC()

	fired := 0
	var hook func()
	hook = func() {
		if fired > 0 {
			return
		}
		fired++
		s.afterDebounceRead = nil
		defer func() { s.afterDebounceRead = hook }()
		if _, err := s.CreateActivityDebounced(debounceWriter(ws.ID, doc.ID, userID, "status: open → done")); err != nil {
			t.Errorf("competing write: %v", err)
		}
		// Push the retry into a later whole second than the one the call
		// started in.
		time.Sleep(1100 * time.Millisecond)
	}
	s.afterDebounceRead = hook

	if _, err := s.CreateActivityDebounced(debounceWriter(ws.ID, doc.ID, userID, "priority: low → high")); err != nil {
		t.Fatalf("contended write: %v", err)
	}
	s.afterDebounceRead = nil

	if fired != 1 {
		t.Fatalf("seam fired %d times, want 1 — without it the sleep never ran and the timestamp assertion below proves nothing", fired)
	}
	rows := updatedRowsFor(t, s, doc.ID)
	if len(rows) != 1 {
		t.Fatalf("want 1 coalesced row, got %d", len(rows))
	}
	if !rows[0].CreatedAt.After(beforeLoop) {
		t.Errorf("merged row stamped %v, which is not after the instant the contended call began (%v) — the retry reused a stale timestamp", rows[0].CreatedAt, beforeLoop)
	}
}
