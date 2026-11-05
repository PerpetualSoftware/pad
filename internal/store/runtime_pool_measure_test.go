package store

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestMeasure_CrossWorkspaceCopyAndRemap prints how long the cross-workspace
// copy (which holds both workspaces' advisory locks) and the attachment remap
// (which holds the workspace seq lock) take on a workspace shaped like the
// largest measured one: 4,079 items and about 20,000 comments (TASK-4278).
// It is a measurement, not an assertion, so it only runs on request:
//
//	PAD_MEASURE=1 PAD_TEST_POSTGRES_URL=... go test ./internal/store -run TestMeasure_ -v
func TestMeasure_CrossWorkspaceCopyAndRemap(t *testing.T) {
	if os.Getenv("PAD_MEASURE") != "1" {
		t.Skip("PAD_MEASURE=1 not set")
	}
	s := pgBounded(t, RuntimeLimits{Statement: 30 * time.Second, Lock: 10 * time.Second, IdleInTx: time.Minute})
	src := createTestWorkspace(t, s, "Measure Source")
	dst := createTestWorkspace(t, s, "Measure Dest")
	srcCol := createTestCollection(t, s, src.ID, "Tasks")
	dstCol := createTestCollection(t, s, dst.ID, "Tasks")

	const items, perItem = 4079, 5
	oldID, newID2 := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	var first *models.Item
	seedStart := time.Now()
	for i := 0; i < items; i++ {
		content := fmt.Sprintf("body of item %d, some prose to make the row non-trivial. ", i)
		if i%50 == 0 {
			content += "![img](pad-attachment:" + oldID + ")"
		}
		it := createTestItem(t, s, src.ID, srcCol.ID, fmt.Sprintf("Item %d", i), content)
		if first == nil {
			first = it
		}
	}
	// Comments in bulk, as one statement, to reach ~20k without 20k round trips.
	if _, err := s.db.Exec(`INSERT INTO comments (id, item_id, workspace_id, body, created_at, updated_at)
		SELECT gen_random_uuid()::text, i.id, i.workspace_id, 'comment ' || g, now()::text, now()::text
		FROM items i, generate_series(1, $2) g WHERE i.workspace_id = $1`, src.ID, perItem); err != nil {
		t.Fatalf("bulk comments: %v", err)
	}
	items0 := pgCount(t, s, `SELECT count(*) FROM items WHERE workspace_id = $1`, src.ID)
	comments0 := pgCount(t, s, `SELECT count(*) FROM comments WHERE workspace_id = $1`, src.ID)
	versions0 := pgCount(t, s, `SELECT count(*) FROM item_versions v JOIN items i ON i.id = v.item_id WHERE i.workspace_id = $1`, src.ID)
	t.Logf("MEASURE fixture: %d items, %d comments, %d item_versions rows, seeded in %s", items0, comments0, versions0, time.Since(seedStart))

	start := time.Now()
	if _, err := s.CopyItemAcrossWorkspaces(CrossWorkspaceCopyRequest{
		SourceItemID: first.ID, TargetWorkspaceID: dst.ID, TargetCollectionID: dstCol.ID, Actor: "measure",
	}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	t.Logf("MEASURE cross-workspace copy of one item (source workspace %d items): %s", items0, time.Since(start))

	start = time.Now()
	if err := s.RemapAttachmentReferencesInWorkspace(src.ID, map[string]string{oldID: newID2}); err != nil {
		t.Fatalf("remap: %v", err)
	}
	t.Logf("MEASURE RemapAttachmentReferencesInWorkspace over %d items / %d comments: %s", items0, comments0, time.Since(start))
}

// measureBulkWorkspace builds a Postgres workspace of n items and
// commentsPer comments each in a handful of statements. Every rewriteEvery-th
// item (0 = none) references oldID from its content, a comment and a version.
func measureBulkWorkspace(t *testing.T, s *Store, name string, n, commentsPer, rewriteEvery int, oldID string) (wsID, colID string) {
	t.Helper()
	ws := createTestWorkspace(t, s, name)
	col := createTestCollection(t, s, ws.ID, "Tasks")
	ref := ""
	if rewriteEvery > 0 {
		ref = remapRef(oldID)
	}
	every := rewriteEvery
	if every <= 0 {
		every = 1 << 30
	}
	if _, err := s.db.Exec(`INSERT INTO items (id, workspace_id, collection_id, title, slug, content, fields, item_number, created_at, updated_at)
		SELECT gen_random_uuid()::text, $1, $2, 'Item ' || g, 'item-' || g,
		  'body of item ' || g || ', some prose to make the row non-trivial. ' || CASE WHEN g % $4::int = 0 THEN $5::text ELSE '' END,
		  '{"status":"open"}'::jsonb, g, now()::text, now()::text FROM generate_series(1, $3::int) g`, ws.ID, col.ID, n, every, ref); err != nil {
		t.Fatalf("bulk items: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO comments (id, item_id, workspace_id, body, created_at, updated_at)
		SELECT gen_random_uuid()::text, i.id, i.workspace_id, 'comment ' || g, now()::text, now()::text
		FROM items i, generate_series(1, $2::int) g WHERE i.workspace_id = $1`, ws.ID, commentsPer); err != nil {
		t.Fatalf("bulk comments: %v", err)
	}
	if rewriteEvery > 0 {
		if _, err := s.db.Exec(`INSERT INTO comments (id, item_id, workspace_id, body, created_at, updated_at)
			SELECT gen_random_uuid()::text, id, workspace_id, 'ref ' || $2::text, now()::text, now()::text FROM items WHERE workspace_id = $1 AND content LIKE '%' || $3::text || '%'`, ws.ID, ref, oldID); err != nil {
			t.Fatalf("bulk ref comments: %v", err)
		}
		if _, err := s.db.Exec(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq)
			SELECT id || '-v1', id, 'older ' || $2::text, false, '2026-01-01T00:00:00Z', 1 FROM items WHERE workspace_id = $1 AND content LIKE '%' || $3::text || '%'`, ws.ID, ref, oldID); err != nil {
			t.Fatalf("bulk versions: %v", err)
		}
	}
	return ws.ID, col.ID
}

// (a) BackfillEffort's whole-table read on the 4,079-item shape and a
// 10x one, with an item writer running alongside to show what it costs a
// runtime writer.
//
//	PAD_MEASURE=1 PAD_TEST_POSTGRES_URL=... go test ./internal/store -run TestMeasure_BackfillEffortRead -v
func TestMeasure_BackfillEffortRead(t *testing.T) {
	if os.Getenv("PAD_MEASURE") != "1" {
		t.Skip("PAD_MEASURE=1 not set")
	}
	s := pgBounded(t, RuntimeLimits{Statement: 30 * time.Second, Lock: 10 * time.Second, IdleInTx: time.Minute})
	for _, n := range []int{4079, 40790} {
		wsID, colID := measureBulkWorkspace(t, s, fmt.Sprintf("Effort %d", n), n, 5, 0, "")
		if _, err := s.db.Exec(`INSERT INTO item_decisions (id, workspace_id, item_id, question_set, question_key, kind, answer, provider, model, state_hash, question_hash, item_seq, evaluated_at)
			SELECT gen_random_uuid()::text, workspace_id, id, 'attention', 'needs_human_decision', 'bool', 'no', 'p', 'm', 'h', 'q', 0, now()::text FROM items WHERE workspace_id = $1`, wsID); err != nil {
			t.Fatal(err)
		}
		probe := createTestItem(t, s, wsID, colID, "probe", "x")
		var maxWrite time.Duration
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; i < 20; i++ {
				title := fmt.Sprintf("probe %d", i)
				b := time.Now()
				if _, err := s.UpdateItem(probe.ID, models.ItemUpdate{Title: &title}); err != nil {
					t.Errorf("writer: %v", err)
				}
				if d := time.Since(b); d > maxWrite {
					maxWrite = d
				}
			}
		}()
		var worst time.Duration
		var got int
		for i := 0; i < 5; i++ {
			b := time.Now()
			c, err := s.ItemsMissingDecisionKey("attention", "needs_human_decision", "effort")
			if err != nil {
				t.Fatal(err)
			}
			got = len(c)
			if d := time.Since(b); d > worst {
				worst = d
			}
		}
		<-done
		t.Logf("MEASURE ItemsMissingDecisionKey over %d items (%d candidates): slowest of 5 reads %s; concurrent item writer max latency %s", n, got, worst, maxWrite)
	}
}

// (b) The largest cross-workspace copy the UI allows. The copy is ONE item (no
// children, comments, versions or links carry); what scales is the item's
// body (the API caps a JSON body at 2 MiB) and the attachments it references,
// which are cloned with their variants. No cap on the count exists, so this
// measures a copy of a 2 MiB item referencing K attachments with one variant
// each, and reports the per-attachment rate.
func TestMeasure_LargestCopy(t *testing.T) {
	if os.Getenv("PAD_MEASURE") != "1" {
		t.Skip("PAD_MEASURE=1 not set")
	}
	s := pgBounded(t, RuntimeLimits{Statement: 30 * time.Second, Lock: 10 * time.Second, IdleInTx: time.Minute})
	for _, k := range []int{0, 100, 1000} {
		src := createTestWorkspace(t, s, fmt.Sprintf("Copy Src %d", k))
		dst := createTestWorkspace(t, s, fmt.Sprintf("Copy Dst %d", k))
		srcCol := createTestCollection(t, s, src.ID, "Tasks")
		dstCol := createTestCollection(t, s, dst.ID, "Tasks")
		var b strings.Builder
		for i := 0; i < k; i++ {
			id := createCloneAttachment(t, s, src.ID)
			orig := "original"
			th := "thumb"
			parent := id
			if err := s.CreateAttachment(&models.Attachment{ID: newID(), WorkspaceID: src.ID, StorageKey: "fs:" + newID(), ContentHash: "h" + newID(), Filename: "t.png", MimeType: "image/png", SizeBytes: 1, Variant: &th, ParentID: &parent}); err != nil {
				t.Fatal(err)
			}
			_ = orig
			b.WriteString(remapRef(id) + "\n")
		}
		for b.Len() < (2<<20)-1024 {
			b.WriteString("filler text to reach the 2 MiB request body cap of an item write.\n")
		}
		it := createTestItem(t, s, src.ID, srcCol.ID, "Largest", b.String())
		start := time.Now()
		if _, err := s.CopyItemAcrossWorkspaces(CrossWorkspaceCopyRequest{SourceItemID: it.ID, TargetWorkspaceID: dst.ID, TargetCollectionID: dstCol.ID, Actor: "measure"}); err != nil {
			t.Fatalf("copy: %v", err)
		}
		d := time.Since(start)
		t.Logf("MEASURE copy of one ~2 MiB item referencing %d attachments (+%d variants): %s (%s per attachment)", k, k, d, perN(d, k))
	}
}

func perN(d time.Duration, n int) time.Duration {
	if n == 0 {
		return d
	}
	return d / time.Duration(n)
}

// (c) Remap at 1,000 rewrites on the 4,079-item / ~20k-comment shape: total
// time and the longest an item writer waited, through the public API only, so
// the SAME test runs against the pre-chunking code (copy this file into a
// checkout of 6d1a7f178) and the chunked code.
func TestMeasure_RemapBeforeAfter(t *testing.T) {
	if os.Getenv("PAD_MEASURE") != "1" {
		t.Skip("PAD_MEASURE=1 not set")
	}
	s := pgBounded(t, RuntimeLimits{Statement: 30 * time.Second, Lock: 5 * time.Minute, IdleInTx: time.Minute})
	oldID, newID2 := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	wsID, colID := measureBulkWorkspace(t, s, "Remap Measure", 4079, 5, 4, oldID)
	probe := createTestItem(t, s, wsID, colID, "probe", "x")
	rewrites := pgCount(t, s, `SELECT count(*) FROM items WHERE workspace_id = $1 AND content LIKE '%' || $2 || '%'`, wsID, oldID)
	var maxWait time.Duration
	var mu sync.Mutex
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			title := fmt.Sprintf("probe %d", i)
			b := time.Now()
			if _, err := s.UpdateItem(probe.ID, models.ItemUpdate{Title: &title}); err != nil {
				t.Errorf("writer: %v", err)
			}
			mu.Lock()
			if d := time.Since(b); d > maxWait {
				maxWait = d
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
		}
	}()
	start := time.Now()
	err := s.RemapAttachmentReferencesInWorkspace(wsID, map[string]string{oldID: newID2})
	total := time.Since(start)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	t.Logf("MEASURE Remap, %d rewrites in a 4079-item workspace: total %s; longest item-writer wait during it (= max lock hold seen) %s", rewrites, total, maxWait)
	mu.Unlock()
}
