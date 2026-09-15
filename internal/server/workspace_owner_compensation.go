package server

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// addOwnerOrCompensate adds userID as the owner of a workspace that was just
// created, and makes that membership FATAL to the create (BUG-2715).
//
// THE DEFECT. Four doors created a workspace, then added the creator as owner,
// then discarded the result: a failure left a workspace that exists, is seeded,
// and that NOBODY CAN ADMINISTER — returned to the caller as a success. The
// error was made visible by TASK-2658 and stayed non-fatal; this makes the
// door answer "a usable workspace, or nothing".
//
// WHY A COMPENSATION RATHER THAN ONE TRANSACTION. The ruling's first choice was
// create + seed + owner in a single transaction. Measured, that is a store-wide
// refactor rather than a unit: SeedCollectionsFromTemplate issues no SQL of its
// own but orchestrates five public Store methods, none of which has an
// executor-taking variant — `createWorkspaceQ` is the ONLY execQueryer helper in
// the whole store — and two of them (CreateCollection, and CreateItem via
// tryCreateItem) open transactions of their own. The transactional version is
// filed separately with that measurement attached.
//
// THE CRASH WINDOW, which is the honest cost of compensating instead. Between
// the failed owner-add and the DeleteWorkspace below, this process can die. The
// husk it leaves is exactly the state this function exists to prevent, and
// nothing here closes that window — only the single transaction would. It is
// small and it is not zero, and a reader deciding whether the transactional
// version is worth building should weigh it rather than discover it.
//
// RECONCILE BEFORE DESTROYING (BUG-3026), which is why this is not simply
// "delete on error". AddWorkspaceMember returns the raw tx.Commit() error, so a
// commit whose acknowledgement was lost LANDS THE ROW and reports failure.
// Deleting on that error destroys a workspace whose membership actually
// succeeded. The four arms below are the same four handleAutoCreateWorkspace
// uses, and for the same reasons — "did my write land?" and "is it safe to
// destroy this?" are different questions, and only the ROLE answers the first.
//
// What differs from that door is the OUTCOME, not the reasoning: it reconciles
// to a successful auto-create, whereas these doors must refuse. Three of the
// four arms therefore return an error.
//
// THE REMOVAL IS A SOFT DELETE FOLLOWED BY A PURGE, and both halves are needed.
//
// The soft delete releases the slug (uniqueWorkspaceSlug filters
// `deleted_at IS NULL`), so the caller's retry reclaims the name instead of
// landing on `name-2` — the husk-holds-the-slug symptom BUG-2892 fixed for
// imports. But a soft delete alone is not "nothing": ListDeletedWorkspaces is
// scoped by `workspaces.owner_id`, NOT by membership, so the row would surface
// in the creator's deleted-workspaces list as a workspace they never knowingly
// created — and restoring it would hand them back exactly the ownerless
// workspace this function exists to prevent. (An earlier revision of this
// comment claimed that list was membership-scoped and therefore that the row
// was invisible; it is not, and the claim was wrong.)
//
// PurgeWorkspaceData finishes it, and it takes the workspace ID rather than the
// slug — so the removal cannot miss because a slug moved. It refuses to touch a
// workspace whose `deleted_at` is unset, which is why the soft delete comes
// first rather than being skipped.
func (s *Server) addOwnerOrCompensate(door, workspaceID, workspaceSlug, userID string) error {
	addErr := s.store.AddWorkspaceMember(workspaceID, userID, "owner")
	if addErr == nil {
		return nil
	}

	membershipCheck := s.store.GetWorkspaceMember
	if s.membershipCheck != nil {
		membershipCheck = s.membershipCheck
	}

	switch member, cerr := membershipCheck(workspaceID, userID); {
	case cerr != nil:
		// UNREADABLE — the read failed too, so a genuine absence and an
		// ack-lost success are indistinguishable. Keep the workspace: an orphan
		// is recoverable, destroying a live workspace on a state nobody could
		// read is not. Still an error, because we cannot say it is usable.
		slog.Error(door+": add owner member failed and the membership read also failed; "+
			"KEEPING the workspace because its state is unknown",
			"workspace_id", workspaceID, "workspace_slug", workspaceSlug, "user_id", userID,
			"error", addErr, "check_error", cerr)
		return fmt.Errorf("add workspace owner: %w (membership state unreadable: %v)", addErr, cerr)

	case member == nil:
		// ABSENT — this caller's owner row genuinely is not there.
		//
		// That answers "did MY write land?" and NOT "is it safe to destroy
		// this?". Those are the two questions BUG-3026 separated, and absence
		// of one member is not absence of all of them: deleting on it alone
		// would remove a workspace somebody else can reach. The doors that call
		// this create the workspace microseconds earlier and nothing else knows
		// its id yet, so the list is expected to be empty — which is exactly
		// why checking is cheap, and why a non-empty answer means something has
		// happened that this function should not be bulldozing.
		//
		// RESIDUAL, stated rather than implied (codex round 2): this is a
		// check-then-delete and it is NOT atomic. A member added between the
		// list returning empty and the purge is destroyed with the workspace.
		// The guard narrows the window; it does not close it, and only the
		// single-transaction form does. The window is bounded by a workspace id
		// nobody outside this request has seen yet, which is why the residual is
		// accepted here rather than fixed with a lock.
		if rerr := s.removeUnusableWorkspace(door, workspaceID, workspaceSlug, userID, addErr); rerr != nil {
			return rerr
		}
		return fmt.Errorf("add workspace owner: %w", addErr)

	// The literal matches AddWorkspaceMember's argument above and every other
	// role comparison in this package; there is no shared constant.
	case member.Role == "owner":
		// LANDED despite the reported error — the ack-lost commit. The workspace
		// IS usable, which is the only thing this function promises, so it is a
		// success.
		slog.Warn(door+": add owner member reported an error but the owner row is present; reconciled to success",
			"workspace_id", workspaceID, "workspace_slug", workspaceSlug, "user_id", userID, "error", addErr)
		return nil

	default:
		// PRESENT WITH THE WRONG ROLE. Not usable by this caller (owner >
		// editor > viewer) and not safe to delete, since somebody does have
		// access. Deliberately NOT repaired by writing the role: this path
		// cannot tell an ack-lost write from another actor's deliberate one,
		// and escalating a permission on that ambiguity is the wrong default.
		slog.Error(door+": add owner member failed and the membership row carries a different role; "+
			"KEEPING the workspace, manual intervention required",
			"workspace_id", workspaceID, "workspace_slug", workspaceSlug, "user_id", userID,
			"found_role", member.Role, "error", addErr)
		return fmt.Errorf("add workspace owner: %w (membership present with role %q)", addErr, member.Role)
	}
}

// removeUnusableWorkspace takes back a workspace that cannot be made usable,
// and is the shared removal both of this door's failure points use (BUG-2715,
// codex round 4).
//
// The owner-add failure was compensated first and the SEED failure was not,
// which left the invariant true of one step and false of the other: a seeding
// error returned 500 with the message "Workspace created but failed to seed
// collections" and kept a live, ownerless, unseeded workspace holding its slug,
// so the retry landed on `name-2`. That is the same husk, reached one step
// earlier — and it is BUG-2892's symptom on a door BUG-2892 did not touch.
//
// Returns nil when the workspace is gone, and an ERROR when it was kept: the
// caller must refuse in both cases, but the error carries why it could not be
// removed so the log and the response do not claim a cleanup that did not
// happen.
//
// RESIDUAL, same as before: this is a check-then-delete and it is not atomic.
// A member added between the list returning empty and the purge is destroyed
// with the workspace. Bounded by a workspace id nobody outside this request has
// seen yet; only the single-transaction form closes it.
//
// BLOBS COME BEFORE THE PURGE (BUG-3094). PurgeWorkspaceData hard-deletes the
// workspace's attachment rows and, by its own contract, never touches bytes —
// and the orphan GC discovers reclaimable blobs THROUGH tombstoned rows
// (OrphanedAttachments: deleted_at IS NOT NULL). So a purge that runs while
// blobs exist deletes the only record that anything will ever reclaim them.
// The three auto-create doors never carry a blob, which is why this did not
// fire there; the bundle-import rollback fires after blobs were rehydrated.
// The order is the retention sweeper's own: list the attachments, reclaim
// every blob with reclaimWorkspaceBlobs (dedupe-aware and in-flight-aware),
// and purge only when that returns true. When it returns false — a transient
// backend or count error, or an upload of the same content in flight — this
// STOPS after the soft delete: the tombstoned rows plus the soft-deleted
// workspace are exactly what the sweeper consumes after the grace window,
// with the same ordering. The row is then restorable until that sweep, which
// is logged at Error, and the caller's contract is unchanged: soft-deleted
// and pending the sweep counts as gone, as it always has.
//
// Single-instance assumption, carried from the reclaimer: two server
// PROCESSES reclaiming concurrently could both observe the other's row and
// both skip a shared blob's delete, orphaning it — the same cross-process
// limitation the orphan GC has. Deferred with multi-instance support.
func (s *Server) removeUnusableWorkspace(door, workspaceID, workspaceSlug, userID string, cause error) error {
	// Absence of ONE member is not absence of all of them — the two questions
	// BUG-3026 separated. Deleting on the first would remove a workspace
	// somebody else can reach.
	others, lerr := s.store.ListWorkspaceMembers(workspaceID)
	if lerr != nil {
		slog.Error(door+": cannot make the workspace usable and the member list could not be read; "+
			"KEEPING it rather than deleting on an unknown state",
			"workspace_id", workspaceID, "workspace_slug", workspaceSlug, "user_id", userID,
			"error", cause, "list_error", lerr)
		return fmt.Errorf("%w (member list unreadable: %v)", cause, lerr)
	}
	if len(others) > 0 {
		slog.Error(door+": cannot make the workspace usable but it has other members; "+
			"KEEPING it, manual intervention required",
			"workspace_id", workspaceID, "workspace_slug", workspaceSlug, "user_id", userID,
			"other_members", len(others), "error", cause)
		return fmt.Errorf("%w (workspace has %d other member(s))", cause, len(others))
	}

	slog.Error(door+": removing the workspace nobody could administer",
		"workspace_id", workspaceID, "workspace_slug", workspaceSlug, "user_id", userID, "error", cause)
	if delErr := s.store.DeleteWorkspace(workspaceSlug); delErr != nil {
		slog.Error(door+": failed to soft-delete the workspace; manual intervention required",
			"workspace_id", workspaceID, "workspace_slug", workspaceSlug, "user_id", userID, "error", delErr)
		return nil
	}

	// Soft-deleted. Everything from here leaves the workspace GONE from the
	// caller's point of view; what varies is whether the purge runs now or
	// is left to the retention sweeper, and every branch that leaves it says
	// so in one sentence: restorable until the retention sweep.
	// One reclaim-then-purge at a time in this process, shared with the
	// sweeper — see workspaceReclaimMu. Taken before the LIST so another
	// sequence cannot purge the rows this one's dedupe guard is about to count.
	s.workspaceReclaimMu.Lock()
	defer s.workspaceReclaimMu.Unlock()
	blobs, blobErr := s.store.WorkspaceAttachmentBlobs(workspaceID)
	if blobErr != nil {
		slog.Error(door+": workspace soft-deleted but its attachments could not be listed, so it was not purged; "+
			"it will appear as restorable until the retention sweep",
			"workspace_id", workspaceID, "workspace_slug", workspaceSlug, "user_id", userID, "error", blobErr)
		return nil
	}
	if len(blobs) > 0 {
		// context.Background rather than a request context: this is a
		// compensation for a request that has already failed, and a client
		// that disconnects mid-way must not turn a reclaim into a leak.
		var res workspacePurgeResult
		if s.attachments == nil || !s.reclaimWorkspaceBlobs(context.Background(),
			store.WorkspacePurgeCandidate{ID: workspaceID, Slug: workspaceSlug}, blobs, &res) {
			slog.Error(door+": workspace soft-deleted but its attachment blobs could not all be reclaimed now, "+
				"so it was not purged; the tombstoned rows keep the blobs discoverable and it will appear as "+
				"restorable until the retention sweep",
				"workspace_id", workspaceID, "workspace_slug", workspaceSlug, "user_id", userID,
				"attachments", len(blobs), "blobs_reclaimed", res.BlobsReclaimed)
			return nil
		}
	}
	if purgeErr := s.store.PurgeWorkspaceData(workspaceID); purgeErr != nil {
		// Soft-deleted but not purged: the slug is free and the door refuses, so
		// the caller is not harmed — but the row sits in the creator's
		// deleted-workspaces list until the retention sweeper reaches it.
		slog.Error(door+": workspace soft-deleted but not purged; it will appear as restorable until "+
			"the retention sweep",
			"workspace_id", workspaceID, "workspace_slug", workspaceSlug, "user_id", userID, "error", purgeErr)
	}
	return nil
}
