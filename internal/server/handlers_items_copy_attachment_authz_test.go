package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// Attachment authorization for the cross-workspace copy — TASK-2408 /
// BUG-2407.
//
// The hole: PlanAttachmentCopy scoped every lookup to the SOURCE WORKSPACE
// (DR-11a) and stopped there. The workspace is not the caller, so a
// restricted member who could edit any item in workspace A could paste
// `pad-attachment:<uuid>` for an attachment hanging off an item they could
// not see, copy that item into a workspace they own, and read the bytes
// through the ordinary blob endpoint.
//
// What these tests pin, in the order the acceptance criteria state it:
//
//  1. the hidden reference is NOT cloned, and the clone is not readable in
//     the destination (the mutation-verified core);
//  2. the preflight's counts for a hidden reference are BYTE-IDENTICAL to
//     its counts for a dangling one — if they diverge, the fix moved the
//     existence oracle instead of closing it;
//  3. the same treatment for an archived parent, a foreign parent and (for
//     a restricted caller) an orphan;
//  4. the CONTROL: attachments the caller can see still copy, variants
//     included, and are readable in the destination. A fix that authorizes
//     nothing through is not a fix;
//  5. preflight and copy agree, because they share one authorizer.

// --- fixture -----------------------------------------------------------

// copyAttachmentFixture extends the shared copy fixture with a real
// attachment registry (the blob endpoint has to serve bytes for the
// "readable in the destination" half of the proof), a collection in A the
// attacker cannot see, and an item inside it carrying an attachment.
type copyAttachmentFixture struct {
	*copyPreflightFixture

	// attacker is a restricted editor: A limited to the SOURCE collection
	// (so they may edit the item being copied) and B limited to the
	// destination collection (so the copy itself is authorized).
	attacker *models.User

	secretItem *models.Item
	secretAtt  *models.Attachment
	secretBody []byte
}

func newCopyAttachmentFixture(t *testing.T) *copyAttachmentFixture {
	t.Helper()
	f := newCopyPreflightFixture(t)

	dir := t.TempDir()
	fs, err := attachments.NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	reg := attachments.NewRegistry()
	reg.Register(attachments.FSPrefix, fs)
	f.srv.SetAttachments(reg, 0)

	secretColl := mustSchemaCollection(t, f.srv, f.wsA.ID, "Secrets A", srcSchemaJSON)
	secretItem, err := f.srv.store.CreateItem(f.wsA.ID, secretColl.ID, models.ItemCreate{
		Title: "The Secret", Fields: `{}`, CreatedBy: f.owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(secret): %v", err)
	}

	body := distinctPNG(t, 0x5a)
	secretAtt := putBlob(t, f.srv, &models.Attachment{
		WorkspaceID: f.wsA.ID, ItemID: &secretItem.ID, Filename: "secret.png",
	}, body)

	return &copyAttachmentFixture{
		copyPreflightFixture: f,
		attacker: f.restrictedEditor("copy-att-attacker@example.com", "copyattattacker",
			[]string{f.collA.ID}, []string{f.collB.ID}),
		secretItem: secretItem,
		secretAtt:  secretAtt,
		secretBody: body,
	}
}

// setSourceContent rewrites the source item's body — the attacker's own
// legitimate edit of an item they hold, which is the whole vector.
func (f *copyAttachmentFixture) setSourceContent(content string) {
	f.t.Helper()
	if _, err := f.srv.store.UpdateItem(f.source.ID, models.ItemUpdate{Content: &content}); err != nil {
		f.t.Fatalf("UpdateItem(content): %v", err)
	}
}

// preflightAsAttacker runs the dry run as the restricted editor and returns
// both the parsed response and the RAW bytes, because one of the assertions
// below is on the bytes.
func (f *copyAttachmentFixture) preflightAsAttacker() (ItemCopyPreflight, []byte) {
	f.t.Helper()
	rr := f.call(f.attacker, reqOpts{wsRoleCtx: "editor"}, f.resolvableBody())
	if rr.Code != http.StatusOK {
		f.t.Fatalf("preflight as the restricted editor: got %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var out ItemCopyPreflight
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		f.t.Fatalf("parse preflight: %v\nbody: %s", err, rr.Body.String())
	}
	return out, rr.Body.Bytes()
}

func (f *copyAttachmentFixture) copyAsAttacker() ItemCopyResult {
	f.t.Helper()
	rr := f.callCopy(f.attacker, reqOpts{wsRoleCtx: "editor"}, f.resolvableBody())
	if rr.Code != http.StatusCreated {
		f.t.Fatalf("copy as the restricted editor: got %d, want 201: %s", rr.Code, rr.Body.String())
	}
	var out ItemCopyResult
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		f.t.Fatalf("parse copy result: %v\nbody: %s", err, rr.Body.String())
	}
	return out
}

// preflightAs / copyAs select the caller: the restricted attacker, or the
// unrestricted owner. Which one a case uses is load-bearing — see
// TestCopy_UnauthorizedParentsAreAllUnresolvable.
func (f *copyAttachmentFixture) preflightAs(restricted bool) ItemCopyPreflight {
	f.t.Helper()
	if restricted {
		out, _ := f.preflightAsAttacker()
		return out
	}
	return f.ok(f.resolvableBody())
}

func (f *copyAttachmentFixture) copyAs(restricted bool) ItemCopyResult {
	f.t.Helper()
	if restricted {
		return f.copyAsAttacker()
	}
	return f.copyOK(f.resolvableBody())
}

// destinationAttachments lists every live attachment row in workspace B,
// read straight out of the database rather than from any response: the
// claim is about what the copy PERSISTED.
func (f *copyAttachmentFixture) destinationAttachments() []models.Attachment {
	f.t.Helper()
	rows, err := f.srv.store.DB().Query(f.srv.store.D().Rebind(
		`SELECT id, content_hash FROM attachments WHERE workspace_id = ? AND deleted_at IS NULL`),
		f.wsB.ID)
	if err != nil {
		f.t.Fatalf("query destination attachments: %v", err)
	}
	defer rows.Close()
	var out []models.Attachment
	for rows.Next() {
		var a models.Attachment
		if err := rows.Scan(&a.ID, &a.ContentHash); err != nil {
			f.t.Fatalf("scan destination attachment: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterate destination attachments: %v", err)
	}
	return out
}

// destinationContent reads the copied item's stored body.
func (f *copyAttachmentFixture) destinationContent(itemID string) string {
	f.t.Helper()
	item, err := f.srv.store.GetItem(itemID)
	if err != nil {
		f.t.Fatalf("GetItem(copy): %v", err)
	}
	if item == nil {
		f.t.Fatalf("GetItem(copy): the copied item is missing")
	}
	return item.Content
}

// --- 1. the mutation-verified core -------------------------------------

// TestCopy_HiddenAttachmentIsNotCloned is BUG-2407's headline regression.
//
// MUTATION-VERIFIED: with the authorizer removed from the two call sites,
// this test fails on the cloned-row assertion AND on the download — the
// secret PNG comes back 200 with its exact bytes from workspace B, which is
// the exfiltration the bug describes.
func TestCopy_HiddenAttachmentIsNotCloned(t *testing.T) {
	f := newCopyAttachmentFixture(t)
	f.setSourceContent("![](pad-attachment:" + f.secretAtt.ID + ")")

	// Precondition: the attacker genuinely cannot read these bytes by the
	// front door. Without this the test could pass for the wrong reason.
	if rr := downloadAs(f.srv, http.MethodGet, f.wsA.ID, f.secretAtt.ID, "", f.attacker, "editor"); rr.Code != http.StatusNotFound {
		t.Fatalf("fixture precondition: the attacker can already read the secret blob directly (%d)", rr.Code)
	}

	pre, _ := f.preflightAsAttacker()
	if pre.Warnings.AttachmentCount != 0 || pre.Warnings.AttachmentBytes != 0 {
		t.Errorf("preflight promised the hidden attachment: count=%d bytes=%d, want 0/0",
			pre.Warnings.AttachmentCount, pre.Warnings.AttachmentBytes)
	}
	if pre.Warnings.UnresolvableRefCount != 1 {
		t.Errorf("unresolvable_ref_count = %d, want 1 — a hidden reference is unresolvable",
			pre.Warnings.UnresolvableRefCount)
	}

	res := f.copyAsAttacker()
	if res.Warnings.AttachmentCount != 0 {
		t.Errorf("the copy reported %d cloned attachments, want 0", res.Warnings.AttachmentCount)
	}
	if got := f.destinationAttachments(); len(got) != 0 {
		t.Fatalf("the copy cloned %d attachment row(s) into workspace B; the hidden blob leaked", len(got))
	}

	// The literal reference survives in the copied body, exactly as a
	// dangling one does — the copy renders as broken as the source did for
	// this caller, and nothing was rewritten to point anywhere readable.
	if content := f.destinationContent(res.Item.ID); !bytes.Contains([]byte(content), []byte(f.secretAtt.ID)) {
		t.Errorf("the copied body no longer carries the literal reference: %q", content)
	}

	// And the source is untouched: refusing to clone is not deleting.
	if att, err := f.srv.store.GetAttachment(f.secretAtt.ID); err != nil || att == nil || att.DeletedAt != nil {
		t.Errorf("the source attachment did not survive the refused copy (err=%v, row=%v)", err, att)
	}
}

// --- 2. the preflight must stay oracle-free ----------------------------

// TestCopyPreflight_HiddenAttachmentReadsAsDangling is the anti-oracle
// assertion, and it is on the RESPONSE BYTES rather than on the three
// counters, because the counters are only the part of the response we
// happen to have thought of. A preflight is read-only and repeatable, so
// any difference at all between "this UUID names a live attachment you
// cannot see" and "this UUID names nothing" is an oracle the caller can
// query at will.
func TestCopyPreflight_HiddenAttachmentReadsAsDangling(t *testing.T) {
	f := newCopyAttachmentFixture(t)

	f.setSourceContent("![](pad-attachment:" + f.secretAtt.ID + ")")
	hidden, hiddenRaw := f.preflightAsAttacker()

	f.setSourceContent("![](pad-attachment:00000000-0000-4000-8000-000000000000)")
	dangling, danglingRaw := f.preflightAsAttacker()

	if !bytes.Equal(hiddenRaw, danglingRaw) {
		t.Fatalf("a hidden attachment reference is distinguishable from a dangling one:\n"+
			" hidden:   %s\n dangling: %s", hiddenRaw, danglingRaw)
	}
	// Belt and braces: identical is only interesting if it is identically
	// UNRESOLVABLE rather than identically empty.
	if hidden.Warnings.UnresolvableRefCount != 1 || dangling.Warnings.UnresolvableRefCount != 1 {
		t.Fatalf("both cases should count one unresolvable reference, got hidden=%d dangling=%d",
			hidden.Warnings.UnresolvableRefCount, dangling.Warnings.UnresolvableRefCount)
	}
}

// --- 3. the rest of the outcome set ------------------------------------

// TestCopy_UnauthorizedParentsAreAllUnresolvable walks the outcomes
// resolveAttachmentParentItem distinguishes, plus the orphan rule, and
// requires every one of them to land in the same bucket. The read path
// collapses these onto one 404; the copy collapses them onto one
// unresolvable count, which is the same claim in this endpoint's vocabulary.
//
// EACH CASE RUNS AS THE CALLER THAT ISOLATES ITS GATE, which is not a
// detail. Run as the restricted attacker, the archived and foreign cases
// would pass on collection visibility alone — they would keep passing with
// includeArchived flipped to true, or with the workspace-identity check
// deleted, and would be proving nothing about the branch they name. Both
// therefore run as the OWNER, who can see the secret item perfectly well
// and must still be refused. Only the orphan case needs a restricted
// caller, because the orphan rule IS about restriction (Codex round 2).
func TestCopy_UnauthorizedParentsAreAllUnresolvable(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup returns the attachment id to reference.
		setup func(f *copyAttachmentFixture) string
		// restricted selects the caller: the restricted attacker, or the
		// unrestricted owner.
		restricted bool
	}{
		{
			// DR-13: an archived parent is not readable, so it is not
			// copyable. As the OWNER — who could see this item until the
			// moment it was archived — so the refusal can only be coming
			// from the liveness gate.
			name: "archived parent",
			setup: func(f *copyAttachmentFixture) string {
				if err := f.srv.store.DeleteItem(f.secretItem.ID); err != nil {
					f.t.Fatalf("DeleteItem(secret): %v", err)
				}
				return f.secretAtt.ID
			},
		},
		{
			// item_id pointing into ANOTHER workspace. The planner's own
			// scope does not catch this: it checks the ATTACHMENT's
			// workspace, and this row is in A. Also as the OWNER, who CAN
			// see the foreign item — that is exactly the confused deputy
			// resolveAttachmentParentItem's workspace-identity check
			// exists to stop, and a restricted caller would mask it.
			name: "foreign parent",
			setup: func(f *copyAttachmentFixture) string {
				foreign, err := f.srv.store.CreateItem(f.wsB.ID, f.collB.ID, models.ItemCreate{
					Title: "Elsewhere", Fields: `{}`, CreatedBy: f.owner.ID,
				})
				if err != nil {
					f.t.Fatalf("CreateItem(foreign): %v", err)
				}
				if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(
					`UPDATE attachments SET item_id = ? WHERE id = ?`), foreign.ID, f.secretAtt.ID); err != nil {
					f.t.Fatalf("point the attachment at a foreign item: %v", err)
				}
				return f.secretAtt.ID
			},
		},
		{
			// item_id NULL. The orphan rule (PLAN-2382 DR-4): the storage
			// listing hides orphans from restricted members, so cloning one
			// for a restricted member would confirm what the listing
			// refuses to.
			name: "orphan, restricted caller",
			setup: func(f *copyAttachmentFixture) string {
				if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(
					`UPDATE attachments SET item_id = NULL WHERE id = ?`), f.secretAtt.ID); err != nil {
					f.t.Fatalf("orphan the attachment: %v", err)
				}
				return f.secretAtt.ID
			},
			restricted: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCopyAttachmentFixture(t)
			id := tc.setup(f)
			f.setSourceContent("![](pad-attachment:" + id + ")")

			pre := f.preflightAs(tc.restricted)
			if pre.Warnings.AttachmentCount != 0 || pre.Warnings.UnresolvableRefCount != 1 {
				t.Errorf("preflight: count=%d unresolvable=%d, want 0/1",
					pre.Warnings.AttachmentCount, pre.Warnings.UnresolvableRefCount)
			}

			res := f.copyAs(tc.restricted)
			if res.Warnings.AttachmentCount != 0 {
				t.Errorf("the copy cloned %d attachment(s), want 0", res.Warnings.AttachmentCount)
			}
			if got := f.destinationAttachments(); len(got) != 0 {
				t.Errorf("the copy persisted %d attachment row(s) in workspace B, want 0", len(got))
			}
		})
	}
}

// TestCopy_OrphanCopiesForAnUnrestrictedCaller is the counterweight to the
// orphan case above: the rule is about RESTRICTED callers, not about
// orphans, and applying it to everyone would silently stop legitimate
// copies of workspace-level uploads. Same rule, same asymmetry, as the read
// path's orphan gate.
func TestCopy_OrphanCopiesForAnUnrestrictedCaller(t *testing.T) {
	f := newCopyAttachmentFixture(t)
	orphan := putBlob(t, f.srv, &models.Attachment{
		WorkspaceID: f.wsA.ID, Filename: "orphan.png",
	}, distinctPNG(t, 0x77))
	f.setSourceContent("![](pad-attachment:" + orphan.ID + ")")

	res := f.copyOK(f.resolvableBody()) // the owner: unrestricted in A
	if res.Warnings.AttachmentCount != 1 {
		t.Fatalf("an unrestricted caller could not copy an orphan attachment: count=%d, want 1",
			res.Warnings.AttachmentCount)
	}
}

// --- 4. the control ----------------------------------------------------

// TestCopy_VisibleAttachmentsStillClone is the assertion that keeps the fix
// honest. Refusing everything would satisfy every test above.
//
// It uses the SAME restricted attacker, referencing an attachment on the
// source item — which they hold — so what is being pinned is precisely the
// visibility boundary and not the caller's role.
func TestCopy_VisibleAttachmentsStillClone(t *testing.T) {
	f := newCopyAttachmentFixture(t)

	origBody := distinctPNG(t, 0x11)
	orig := putBlob(t, f.srv, &models.Attachment{
		WorkspaceID: f.wsA.ID, ItemID: &f.source.ID, Filename: "mine.png",
	}, origBody)
	variantKey := models.AttachmentVariantThumbMd
	variantBody := distinctPNG(t, 0x22)
	variant := putBlob(t, f.srv, &models.Attachment{
		WorkspaceID: f.wsA.ID, ItemID: &f.source.ID, ParentID: &orig.ID, Variant: &variantKey,
		Filename: "mine-thumb.png",
	}, variantBody)

	f.setSourceContent("![](pad-attachment:" + orig.ID + ")")

	pre, _ := f.preflightAsAttacker()
	// Two rows: the original and its variant. Variants are cloned with
	// their parent — a half-copied variant set is worse than neither.
	if pre.Warnings.AttachmentCount != 2 {
		t.Errorf("preflight attachment_count = %d, want 2 (original + variant)", pre.Warnings.AttachmentCount)
	}
	if want := orig.SizeBytes + variant.SizeBytes; pre.Warnings.AttachmentBytes != want {
		t.Errorf("preflight attachment_bytes = %d, want %d", pre.Warnings.AttachmentBytes, want)
	}
	if pre.Warnings.UnresolvableRefCount != 0 {
		t.Errorf("unresolvable_ref_count = %d, want 0 — this caller can see this attachment",
			pre.Warnings.UnresolvableRefCount)
	}

	res := f.copyAsAttacker()
	if res.Warnings.AttachmentCount != pre.Warnings.AttachmentCount ||
		res.Warnings.AttachmentBytes != pre.Warnings.AttachmentBytes ||
		res.Warnings.UnresolvableRefCount != pre.Warnings.UnresolvableRefCount {
		t.Errorf("preflight and copy disagree: preflight %+v, copy %+v", pre.Warnings, res.Warnings)
	}

	cloned := f.destinationAttachments()
	if len(cloned) != 2 {
		t.Fatalf("workspace B has %d attachment rows, want 2", len(cloned))
	}

	// The clone is REAL: the rewritten body points at the new id, and the
	// bytes come back through the destination workspace.
	content := f.destinationContent(res.Item.ID)
	if bytes.Contains([]byte(content), []byte(orig.ID)) {
		t.Errorf("the copied body still points at workspace A's id: %q", content)
	}
	var served bool
	for _, c := range cloned {
		if c.ContentHash != orig.ContentHash {
			continue
		}
		if !bytes.Contains([]byte(content), []byte(c.ID)) {
			continue
		}
		rr := downloadAs(f.srv, http.MethodGet, f.wsB.ID, c.ID, "", f.attacker, "editor")
		if rr.Code != http.StatusOK {
			t.Fatalf("the cloned attachment is not readable in workspace B: %d %s", rr.Code, rr.Body.String())
		}
		if !bytes.Equal(rr.Body.Bytes(), origBody) {
			t.Errorf("the cloned attachment served %d bytes, want the %d-byte original",
				rr.Body.Len(), len(origBody))
		}
		served = true
	}
	if !served {
		t.Fatal("no cloned row matched the rewritten reference — the copy did not actually carry the attachment")
	}
}
