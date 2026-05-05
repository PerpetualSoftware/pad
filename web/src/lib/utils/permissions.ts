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

export function canViewCollection(m: WorkspaceMembership | null, collId: string): boolean {
	if (!m) return false;
	if (m.role === 'owner') return true;
	if (m.collection_grants.some((g) => g.collection_id === collId)) return true;
	if (m.collection_access === 'all') return true;
	return m.visible_collection_ids.includes(collId);
}

export function canEditCollection(m: WorkspaceMembership | null, collId: string): boolean {
	if (!m) return false;
	if (m.role === 'owner') return true;
	const cg = m.collection_grants.find((g) => g.collection_id === collId);
	if (cg) return cg.permission === 'edit';
	if (!canViewCollection(m, collId)) return false;
	return m.role === 'editor';
}

export function canViewItem(
	m: WorkspaceMembership | null,
	item: { id: string; collection_id: string }
): boolean {
	if (!m) return false;
	if (m.role === 'owner') return true;
	if (m.item_grants.some((g) => g.item_id === item.id)) return true;
	return canViewCollection(m, item.collection_id);
}

export function canEditItem(
	m: WorkspaceMembership | null,
	item: { id: string; collection_id: string }
): boolean {
	if (!m) return false;
	if (m.role === 'owner') return true;
	const ig = m.item_grants.find((g) => g.item_id === item.id);
	if (ig) return ig.permission === 'edit';
	return canEditCollection(m, item.collection_id);
}
