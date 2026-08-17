package store

// BUG-2614 — the orphan GC's reference scan and the writer-side reference
// stamp both ignored the legacy v1 documents surface, so an attachment
// referenced ONLY from a document body was reclaimable while genuinely
// referenced. BUG-2615 — the bundle import's attachment remap rewrote item
// content and fields but not comment bodies, so imported comments kept the
// source workspace's ids.
//
// Both defects have the same shape: a content surface that carries
// `pad-attachment:` references was missing from a walk that is supposed to
// cover every such surface.

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func (f *gcClaimFixture) createDocument(t *testing.T, content string) *models.Document {
	t.Helper()
	doc, err := f.s.CreateDocument(f.wsID, models.DocumentCreate{
		Title:   "Ref carrier",
		Content: content,
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if doc == nil {
		t.Fatal("CreateDocument returned nil")
	}
	return doc
}

// The headline: an attachment referenced only from a document body must read
// as referenced. Before the fix the scan covered items and comments only, so
// this returned false and the GC was free to reclaim a live reference.
func TestAttachmentReferenced_SeesDocumentContent(t *testing.T) {
	f := newGCClaimFixture(t)
	a := f.seedNeverAttached(t)

	// Control first: nothing references it yet, on any surface.
	referenced, err := f.s.AttachmentReferenced(f.wsID, a.ID)
	if err != nil {
		t.Fatalf("AttachmentReferenced (pre): %v", err)
	}
	if referenced {
		t.Fatal("attachment read as referenced before anything referenced it — " +
			"the assertion below would be vacuous")
	}

	f.createDocument(t, "before ![img](pad-attachment:"+a.ID+") after")

	referenced, err = f.s.AttachmentReferenced(f.wsID, a.ID)
	if err != nil {
		t.Fatalf("AttachmentReferenced (post): %v", err)
	}
	if !referenced {
		t.Error("attachment referenced from a document body read as UNreferenced — " +
			"the orphan GC would reclaim it while the reference is live")
	}
}

// A soft-deleted document must NOT hold a reference, mirroring items. Without
// this the scan would pin blobs behind deleted content forever.
func TestAttachmentReferenced_IgnoresDeletedDocument(t *testing.T) {
	f := newGCClaimFixture(t)
	a := f.seedNeverAttached(t)
	doc := f.createDocument(t, "![img](pad-attachment:"+a.ID+")")

	// Armed: live document, reference visible. Without this leg a broken
	// scan that never sees documents at all would satisfy the assertion below.
	referenced, err := f.s.AttachmentReferenced(f.wsID, a.ID)
	if err != nil {
		t.Fatalf("AttachmentReferenced (live): %v", err)
	}
	if !referenced {
		t.Fatal("fixture never armed: live document's reference was not seen")
	}

	if err := f.s.DeleteDocument(doc.ID); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}

	referenced, err = f.s.AttachmentReferenced(f.wsID, a.ID)
	if err != nil {
		t.Fatalf("AttachmentReferenced (deleted): %v", err)
	}
	if referenced {
		t.Error("a soft-deleted document still held the attachment reference; " +
			"items are scanned live-only and documents must match")
	}
}

// A document in ANOTHER workspace must not hold the reference — the scan is
// workspace-scoped, and a pasted foreign id must not pin someone else's rows.
func TestAttachmentReferenced_DocumentScanIsWorkspaceScoped(t *testing.T) {
	f := newGCClaimFixture(t)
	a := f.seedNeverAttached(t)

	other, err := f.s.CreateWorkspace(models.WorkspaceCreate{Name: "Other WS"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if _, err := f.s.CreateDocument(other.ID, models.DocumentCreate{
		Title:   "Foreign carrier",
		Content: "![img](pad-attachment:" + a.ID + ")",
	}); err != nil {
		t.Fatalf("CreateDocument (other ws): %v", err)
	}

	referenced, err := f.s.AttachmentReferenced(f.wsID, a.ID)
	if err != nil {
		t.Fatalf("AttachmentReferenced: %v", err)
	}
	if referenced {
		t.Error("a document in another workspace held the reference — the scan " +
			"must stay workspace-scoped")
	}
}

// The write half. The scan protects references that exist when the sweep runs;
// the stamp is what covers a reference landing DURING a sweep, and it only
// works if the document writers call it. Both write paths are covered because
// they were differently shaped: UpdateDocument already had a transaction,
// CreateDocument had none and needed one.
func TestDocumentWrites_StampAttachmentRefs(t *testing.T) {
	t.Run("create stamps", func(t *testing.T) {
		f := newGCClaimFixture(t)
		a := f.seedNeverAttached(t)

		if stamp := f.lastReferencedAt(t, a.ID); stamp != nil {
			t.Fatalf("attachment already stamped before any write: %v", *stamp)
		}

		f.createDocument(t, "![img](pad-attachment:"+a.ID+")")

		if stamp := f.lastReferencedAt(t, a.ID); stamp == nil {
			t.Error("CreateDocument did not stamp the reference it wrote — a claim " +
				"racing the insert cannot observe it")
		}
	})

	t.Run("update stamps", func(t *testing.T) {
		f := newGCClaimFixture(t)
		a := f.seedNeverAttached(t)

		// Create with NO reference, so the stamp under test can only come
		// from the update.
		doc := f.createDocument(t, "nothing here yet")
		if stamp := f.lastReferencedAt(t, a.ID); stamp != nil {
			t.Fatalf("stamped by a document that never referenced it: %v", *stamp)
		}

		body := "![img](pad-attachment:" + a.ID + ")"
		if _, err := f.s.UpdateDocument(doc.ID, models.DocumentUpdate{Content: &body}); err != nil {
			t.Fatalf("UpdateDocument: %v", err)
		}

		if stamp := f.lastReferencedAt(t, a.ID); stamp == nil {
			t.Error("UpdateDocument did not stamp the reference it wrote")
		}
	})

	t.Run("a metadata-only update stamps nothing", func(t *testing.T) {
		f := newGCClaimFixture(t)
		a := f.seedNeverAttached(t)
		doc := f.createDocument(t, "![img](pad-attachment:"+a.ID+")")

		// Clear the create-time stamp so the assertion is about THIS update.
		if _, err := f.s.db.Exec(f.s.q(
			`UPDATE attachments SET last_referenced_at = NULL WHERE id = ?`), a.ID); err != nil {
			t.Fatalf("clear stamp: %v", err)
		}

		title := "Renamed, content untouched"
		if _, err := f.s.UpdateDocument(doc.ID, models.DocumentUpdate{Title: &title}); err != nil {
			t.Fatalf("UpdateDocument: %v", err)
		}

		if stamp := f.lastReferencedAt(t, a.ID); stamp != nil {
			t.Errorf("a metadata-only update refreshed the reference stamp (%v) — it "+
				"neither adds nor keeps a reference and must not vouch for one", *stamp)
		}
	})
}

// BUG-2615. The remap is what makes an imported bundle's references point at
// the destination's cloned attachment rows. It walked items only, so a
// reference living in a COMMENT kept the source workspace's id: broken in the
// destination, and the clone it should have pointed at ends up referenced by
// nothing.
func TestRemapAttachmentReferences_RewritesCommentBodies(t *testing.T) {
	f := newGCClaimFixture(t)
	oldID, newID := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"

	item, err := f.s.CreateItem(f.wsID, f.collID, models.ItemCreate{Title: "Carrier"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	comment, err := f.s.CreateComment(f.wsID, item.ID, "", models.CommentCreate{
		Author: "alice",
		Body:   "see ![img](pad-attachment:" + oldID + ") above",
	})
	if err != nil {
		t.Fatalf("CreateComment: %v", err)
	}

	if err := f.s.RemapAttachmentReferencesInWorkspace(f.wsID, map[string]string{oldID: newID}); err != nil {
		t.Fatalf("RemapAttachmentReferencesInWorkspace: %v", err)
	}

	var body string
	if err := f.s.db.QueryRow(f.s.q(`SELECT body FROM comments WHERE id = ?`), comment.ID).Scan(&body); err != nil {
		t.Fatalf("read comment body: %v", err)
	}
	if want := "pad-attachment:" + newID; !contains(body, want) {
		t.Errorf("comment body was not remapped: %q — an imported comment keeps the "+
			"SOURCE workspace's id, which resolves to nothing here", body)
	}
	if stale := "pad-attachment:" + oldID; contains(body, stale) {
		t.Errorf("comment body still carries the source id: %q", body)
	}
}

// The remap must leave the destination's stamps correct, not just its text.
// The import stamps each comment body at insert, but the body still holds the
// SOURCE ids then — so the clone the rewrite now points at carries no stamp
// unless the remap adds one, which is exactly the never-attached shape the GC
// claims.
func TestRemapAttachmentReferences_StampsTheRewrittenTarget(t *testing.T) {
	f := newGCClaimFixture(t)
	target := f.seedNeverAttached(t)
	oldID := "33333333-3333-4333-8333-333333333333"

	item, err := f.s.CreateItem(f.wsID, f.collID, models.ItemCreate{Title: "Carrier"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := f.s.CreateComment(f.wsID, item.ID, "", models.CommentCreate{
		Author: "alice",
		Body:   "![img](pad-attachment:" + oldID + ")",
	}); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	// The comment referenced the SOURCE id, so nothing stamped the clone.
	if stamp := f.lastReferencedAt(t, target.ID); stamp != nil {
		t.Fatalf("fixture never armed: clone was already stamped (%v)", *stamp)
	}

	if err := f.s.RemapAttachmentReferencesInWorkspace(f.wsID, map[string]string{oldID: target.ID}); err != nil {
		t.Fatalf("RemapAttachmentReferencesInWorkspace: %v", err)
	}

	if stamp := f.lastReferencedAt(t, target.ID); stamp == nil {
		t.Error("the remap rewrote a reference onto the clone without stamping it — " +
			"a GC sweep between the import and the next write reclaims it")
	}
}

// A clone that nothing ends up referencing must NOT be stamped. Stamping the
// whole id map would vouch for rows no text points at, keeping genuinely
// unreferenced clones alive for an extra GC window.
func TestRemapAttachmentReferences_DoesNotStampUnreferencedClones(t *testing.T) {
	f := newGCClaimFixture(t)
	unreferenced := f.seedNeverAttached(t)

	if err := f.s.RemapAttachmentReferencesInWorkspace(f.wsID, map[string]string{
		"44444444-4444-4444-8444-444444444444": unreferenced.ID,
	}); err != nil {
		t.Fatalf("RemapAttachmentReferencesInWorkspace: %v", err)
	}

	if stamp := f.lastReferencedAt(t, unreferenced.ID); stamp != nil {
		t.Errorf("stamped a clone that no content references (%v)", *stamp)
	}
}

// strings.Contains by another name would be simpler; kept local only because
// the package already imports strings for other purposes and a helper name
// collision is cheaper to avoid than to debug.
func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
