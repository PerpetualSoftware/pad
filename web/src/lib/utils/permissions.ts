/**
 * Pure permission helpers (PLAN-1100 / TASK-1101).
 *
 * Mirrors the server's ResolveUserPermission cascade exactly so the UI cannot
 * show edit affordances the server would reject:
 *
 *     owner → item grant → collection grant → membership role + visibility → deny
 *
 * Item grant beats collection grant beats membership role even when the item
 * grant is *less* permissive (e.g. ItemGrant.view + CollectionGrant.edit on
 * the same item → effective view, server rejects edit).
 *
 * These functions are pure and stateless. The workspace store wraps them with
 * its `currentMembership` state. Splitting the cascade out keeps it isolated
 * for testing and makes the logic reusable from non-store contexts.
 *
 * Server-side coverage of the cascade lives in
 * `internal/store/permissions_test.go` and `internal/store/grants_test.go`,
 * plus the `/me` endpoint shape in `internal/server/handlers_me_test.go`.
 * Frontend unit tests are deferred until web/ adopts a unit-test runner;
 * the helpers below are intentionally thin so the cost of that gap is small.
 */

import type { WorkspaceMembership } from '$lib/types';

export function canEditWorkspace(m: WorkspaceMembership | null): boolean {
	return m?.role === 'owner';
}

/**
 * canViewCollection — should the collection appear in nav / be browseable?
 *
 * This is the broad nav-visibility predicate. It uses
 * `visible_collection_ids` which deliberately includes collections that
 * only contain item-granted items (so a guest with one ItemGrant on
 * TASK-5 sees Tasks in the sidebar). For per-item access, use canViewItem
 * — which uses the strict `full_access_collection_ids` instead.
 */
export function canViewCollection(m: WorkspaceMembership | null, collId: string): boolean {
	if (!m) return false;
	if (m.role === 'owner') return true;
	if (m.collection_grants.some((g) => g.collection_id === collId)) return true;
	if (m.collection_access === 'all') return true;
	return m.visible_collection_ids.includes(collId);
}

/**
 * canEditCollection — collection-level write affordances (e.g. "+ New").
 *
 * Item grants intentionally do not promote here — collection-wide write
 * requires a collection-level grant or a role that implies write *and*
 * full access to this collection. Server enforcement matches.
 *
 * NOTE: the membership fallback uses the strict full-access set, NOT the
 * broader nav set. A restricted editor whose only access to a collection
 * comes from an item grant has that collection in visible_collection_ids
 * (so it appears in nav) but NOT in full_access_collection_ids — and must
 * not see collection-wide affordances like "+ New" because the server
 * would reject collection-level writes.
 */
export function canEditCollection(m: WorkspaceMembership | null, collId: string): boolean {
	if (!m) return false;
	if (m.role === 'owner') return true;
	const cg = m.collection_grants.find((g) => g.collection_id === collId);
	if (cg) return cg.permission === 'edit';
	if (m.role !== 'editor') return false;
	// Editor membership: needs full access to this collection.
	if (m.collection_access === 'all') return true;
	return m.full_access_collection_ids.includes(collId);
}

/**
 * canViewItem — can the user actually see this specific item?
 *
 * Mirrors the server's request-handler filtering (guestResourceFilter +
 * isItemVisibleToGuest), NOT just the broad nav predicate:
 *   - owner → true
 *   - explicit item grant → true
 *   - explicit collection grant → true (covers all items in that collection)
 *   - collection_access === "all" → true (member with full access)
 *   - collection_access === "specific" + collection in
 *     full_access_collection_ids → true
 *   - else → false (e.g. guest with single item-grant cannot see siblings,
 *     and a restricted member cannot see siblings of an item-granted item
 *     in a collection they don't otherwise have full access to)
 */
export function canViewItem(
	m: WorkspaceMembership | null,
	item: { id: string; collection_id: string }
): boolean {
	if (!m) return false;
	if (m.role === 'owner') return true;
	if (m.item_grants.some((g) => g.item_id === item.id)) return true;
	if (m.collection_grants.some((g) => g.collection_id === item.collection_id)) return true;
	if (m.collection_access === 'all') return true;
	return m.full_access_collection_ids.includes(item.collection_id);
}

/**
 * canEditItem — item-level write affordances (title, content, fields,
 * delete, status, comment composer, drag handle).
 *
 * Mirrors server precedence — item grant beats collection grant beats
 * membership role. So ItemGrant.view + CollectionGrant.edit on the same
 * item → false (item grant wins, even though it's less permissive).
 *
 * The fallback path uses canViewItem's strict membership-based check so a
 * guest cannot edit siblings of an item-granted item.
 */
export function canEditItem(
	m: WorkspaceMembership | null,
	item: { id: string; collection_id: string }
): boolean {
	if (!m) return false;
	if (m.role === 'owner') return true;
	const ig = m.item_grants.find((g) => g.item_id === item.id);
	if (ig) return ig.permission === 'edit';
	const cg = m.collection_grants.find((g) => g.collection_id === item.collection_id);
	if (cg) return cg.permission === 'edit';
	// Membership fallback — must (a) be in the strict full-access set when
	// "specific" access, and (b) have a role that implies write.
	if (m.collection_access === 'specific'
		&& !m.full_access_collection_ids.includes(item.collection_id)) {
		return false;
	}
	return m.role === 'editor';
}
