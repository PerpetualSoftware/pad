import { api } from '$lib/api/client';
import type { Workspace, WorkspaceMembership } from '$lib/types';
import * as perms from '$lib/utils/permissions';

let workspaces = $state<Workspace[]>([]);
let current = $state<Workspace | null>(null);
let currentMembership = $state<WorkspaceMembership | null>(null);
let loading = $state(false);

// Monotonic sequence guarding async /me responses against navigation races.
// Each setCurrent / create call increments the counter; a /me response is
// only applied if its captured token still matches at resolution time. This
// prevents a slow /me for workspace A from clobbering a freshly-set
// membership for workspace B.
let membershipSeq = 0;

/**
 * Resource-scoped permission helpers (PLAN-1100 / TASK-1101).
 *
 * Wraps the pure cascade in `$lib/utils/permissions` with the store's
 * `currentMembership` state. The cascade mirrors the server's
 * ResolveUserPermission exactly so the UI cannot show edit affordances the
 * server would reject:
 *
 *     owner → item grant → collection grant → membership role + visibility → deny
 *
 * Item grant beats collection grant beats membership role even when the
 * item grant is less permissive. `currentMembership` is null when not loaded
 * yet or the fetch failed; in that case all helpers return false (treat
 * unknown as no access).
 */

export const workspaceStore = {
	get workspaces() { return workspaces; },
	get current() { return current; },
	get loading() { return loading; },

	get currentMembership() { return currentMembership; },

	get currentRole() {
		return currentMembership?.role ?? null;
	},

	get isOwner() {
		return currentMembership?.role === 'owner';
	},

	/** Owner-only chrome: settings tabs, members mutation, archive workspace. */
	get canEditWorkspace() {
		return perms.canEditWorkspace(currentMembership);
	},

	canViewCollection(collId: string): boolean {
		return perms.canViewCollection(currentMembership, collId);
	},

	canEditCollection(collId: string): boolean {
		return perms.canEditCollection(currentMembership, collId);
	},

	canViewItem(item: { id: string; collection_id: string }): boolean {
		return perms.canViewItem(currentMembership, item);
	},

	canEditItem(item: { id: string; collection_id: string }): boolean {
		return perms.canEditItem(currentMembership, item);
	},

	async loadAll() {
		loading = true;
		try {
			workspaces = await api.workspaces.list();
		} finally {
			loading = false;
		}
	},

	async setCurrent(ws: Workspace | string) {
		// Capture a sequence token for this call. Any /me response received
		// after a later setCurrent / create has run will be discarded — see
		// membershipSeq comment at top of file.
		const seq = ++membershipSeq;

		// Clear stale membership immediately so helpers don't briefly answer
		// "yes" using the previous workspace's grants while /me is in flight.
		currentMembership = null;

		// Resolve the workspace itself. Membership is fetched once we know
		// the slug.
		let resolved: Workspace | null = null;
		let slug: string;
		if (typeof ws === 'object') {
			resolved = ws;
			slug = ws.slug;
		} else {
			slug = ws;
			const found = workspaces.find((w) => w.slug === ws);
			if (found) {
				resolved = found;
			} else {
				try {
					resolved = await api.workspaces.get(ws);
				} catch {
					resolved = null;
				}
			}
		}

		// Drop any later writes from a stale call.
		if (seq !== membershipSeq) return;
		current = resolved;

		// Fetch the current user's membership context. If the workspace
		// doesn't resolve (404) or membership fetch fails (403), leave the
		// membership null so helpers return false.
		if (resolved) {
			try {
				const m = await api.workspaces.me(slug);
				if (seq === membershipSeq) currentMembership = m;
			} catch {
				if (seq === membershipSeq) currentMembership = null;
			}
		}
	},

	async create(data: { name: string; description?: string; template?: string }) {
		const seq = ++membershipSeq;
		currentMembership = null;
		const ws = await api.workspaces.create(data);
		if (seq !== membershipSeq) return ws;
		workspaces = [...workspaces, ws];
		current = ws;
		// New workspace — refresh membership for the just-created context.
		try {
			const m = await api.workspaces.me(ws.slug);
			if (seq === membershipSeq) currentMembership = m;
		} catch {
			if (seq === membershipSeq) currentMembership = null;
		}
		return ws;
	}
};
