package store

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func copyLimitBody(n int) string {
	var body strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&body, "![file](pad-attachment:ref-%d)\n", i)
	}
	return body.String()
}

func copyLimitSnapshot(t *testing.T, s *Store) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range []string{"items", "attachments", "item_versions", "item_workspace_moves", "item_wiki_links"} {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts[table] = n
	}
	return counts
}

func TestCopyAttachmentLimitExactly1000(t *testing.T) {
	t.Parallel()
	f := newCopyFixture(t)
	var body strings.Builder
	a := f.attachIn(t, f.wsA.ID, "file.png", 1)
	body.WriteString(imageRef(a.ID))
	// Batch the remaining fixture rows in one transaction to avoid 999 fsyncs.
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 1; i < 1000; i++ {
		a.ID = ""
		if err := f.s.CreateAttachmentTx(tx, a); err != nil {
			t.Fatal(err)
		}
		body.WriteString(imageRef(a.ID))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "At the limit", body.String())
	req := f.req()
	req.SourceItemID = src.ID
	res := f.copy(t, req)
	if n := countAttachmentsIn(t, f.s, f.wsB.ID); n != 1000 {
		t.Fatalf("copied %d attachment rows, want 1000", n)
	}
	if res.Item.ID == src.ID {
		t.Fatal("copy reused source item")
	}
}

func TestCopyAttachmentLimitDistinctNotOccurrences(t *testing.T) {
	t.Parallel()
	f := newCopyFixture(t)
	refs := make([]string, 3)
	for i := range refs {
		refs[i] = imageRef(f.attachIn(t, f.wsA.ID, fmt.Sprintf("file-%d.png", i), 1).ID)
	}
	var body strings.Builder
	for i := 0; i < 1001; i++ {
		body.WriteString(refs[i%3])
	}
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Duplicates", body.String())
	req := f.req()
	req.SourceItemID = src.ID
	f.copy(t, req)
	if n := countAttachmentsIn(t, f.s, f.wsB.ID); n != 3 {
		t.Fatalf("copied %d attachments, want 3", n)
	}
}

func TestCopyAttachmentLimitRefusesBeforeTransaction(t *testing.T) {
	t.Parallel()
	f := newCopyFixture(t)
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Over the limit", copyLimitBody(1001)+"[[Other]]")
	before := copyLimitSnapshot(t, f.s)
	a, b := countItemsIn(t, f.s, f.wsA.ID), countItemsIn(t, f.s, f.wsB.ID)
	reached := false
	f.s.afterItemPreLockRead = func(string) { reached = true }
	req := f.req()
	req.SourceItemID = src.ID
	req.ArchiveSource = true
	req.EnforceStorageLimit = true
	req.PreCheck = func(*sql.Tx, *models.Item, *models.Collection) error { reached = true; return nil }
	_, err := f.s.CopyItemAcrossWorkspaces(req)
	if !errors.Is(err, ErrCopyTooManyAttachments) {
		t.Errorf("copy error = %v, want ErrCopyTooManyAttachments", err)
	}
	if reached {
		t.Error("oversized copy reached the pre-lock hook or transaction")
	}
	if got := copyLimitSnapshot(t, f.s); !reflect.DeepEqual(got, before) {
		t.Errorf("rows changed: before %v, after %v", before, got)
	}
	if countItemsIn(t, f.s, f.wsA.ID) != a || countItemsIn(t, f.s, f.wsB.ID) != b {
		t.Error("workspace item counts changed")
	}
	if !isExpectedCopyRejection(ErrCopyTooManyAttachments) {
		t.Error("cap refusal is not classified as pre-write rejection")
	}
}

func TestCopyAttachmentLimitGrownBeforeLock(t *testing.T) {
	t.Parallel()
	f := newCopyFixture(t)
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Grows", copyLimitBody(1000))
	grew := false
	var before map[string]int
	f.s.afterItemPreLockRead = func(id string) {
		f.s.afterItemPreLockRead = nil
		content := copyLimitBody(1001) + "[[Other]]"
		if _, err := f.s.UpdateItem(id, models.ItemUpdate{Content: &content}); err != nil {
			t.Fatal(err)
		}
		grew = true
		before = copyLimitSnapshot(t, f.s)
	}
	req := f.req()
	req.SourceItemID = src.ID
	req.ArchiveSource = true
	_, err := f.s.CopyItemAcrossWorkspaces(req)
	if !grew {
		t.Fatal("between-read hook did not run")
	}
	if !errors.Is(err, ErrCopyTooManyAttachments) {
		t.Errorf("copy error = %v, want ErrCopyTooManyAttachments", err)
	}
	if got := copyLimitSnapshot(t, f.s); !reflect.DeepEqual(got, before) {
		t.Errorf("copy wrote after growth: before %v, after %v", before, got)
	}
	if live, err := f.s.GetItem(src.ID); err != nil || live == nil {
		t.Fatalf("source was archived: %v", err)
	}
}

func TestCountAttachmentRefsMatchesPlan(t *testing.T) {
	t.Parallel()
	f := newPlanFixture(t)
	a := f.attach(t, f.wsA, "image.png", 1)
	b := f.attach(t, f.wsA, "file.txt", 1)
	c := f.attach(t, f.wsA, "field.png", 1)
	content := imageRef(a.ID) + " [file](pad-attachment:" + b.ID + ") " + imageRef(a.ID) + " pad-attachment:"
	fields := map[string]any{"nested": []any{map[string]any{"ref": "pad-attachment:" + c.ID}, "pad-attachment:" + a.ID}}
	n, err := CountAttachmentRefs(content, fields)
	if err != nil {
		t.Fatal(err)
	}
	plan := f.plan(t, f.req(content, fields))
	if n != 3 || n != len(sourceIDs(plan)) {
		t.Fatalf("count = %d, plan source IDs = %v", n, sourceIDs(plan))
	}
	if _, err := CountAttachmentRefs("", map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("unencodable fields accepted")
	}
}

func TestCopyAttachmentLimitUsesFinalFields(t *testing.T) {
	t.Parallel()
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("override=%t", override), func(t *testing.T) {
			f := newCopyFixture(t)
			schema := `{"fields":[{"key":"refs","type":"text"}]}`
			if _, err := f.s.UpdateCollection(f.colA.ID, models.CollectionUpdate{Schema: &schema}); err != nil {
				t.Fatal(err)
			}
			if override {
				if _, err := f.s.UpdateCollection(f.colB.ID, models.CollectionUpdate{Schema: &schema}); err != nil {
					t.Fatal(err)
				}
			}
			fields := fmt.Sprintf(`{"refs":%q}`, copyLimitBody(1001))
			src, err := f.s.CreateItem(f.wsA.ID, f.colA.ID, models.ItemCreate{Title: "Fields", Fields: fields})
			if err != nil {
				t.Fatal(err)
			}
			req := f.req()
			req.SourceItemID = src.ID
			if override {
				req.FieldOverrides = map[string]any{"refs": "pad-attachment:only-one"}
			}
			f.copy(t, req) // Dropped or replaced source references must not trip the advisory check.
			if override {
				req.FieldOverrides = map[string]any{"refs": copyLimitBody(1001)}
				_, err := f.s.CopyItemAcrossWorkspaces(req)
				if !errors.Is(err, ErrCopyTooManyAttachments) {
					t.Fatalf("oversized final fields: %v", err)
				}
			}
		})
	}
}

// The advisory cap check can only refuse. A request that fails field
// migration (an override naming a key the destination does not declare) must
// reach the transaction and get PreCheck's answer, as it did before the cap.
func TestCopyAttachmentLimitAdvisoryErrorsDoNotPreemptPreCheck(t *testing.T) {
	t.Parallel()
	f := newCopyFixture(t)
	src := createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Under the limit", "plain body")
	denied := errors.New("pre-check denied")
	req := f.req()
	req.SourceItemID = src.ID
	req.FieldOverrides = map[string]any{"no_such_destination_key": "x"}
	req.PreCheck = func(*sql.Tx, *models.Item, *models.Collection) error { return denied }
	_, err := f.s.CopyItemAcrossWorkspaces(req)
	if !errors.Is(err, denied) {
		t.Fatalf("copy error = %v, want the PreCheck denial", err)
	}
}
