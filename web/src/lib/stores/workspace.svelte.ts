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

// The `loadAll` request currently in flight, or null. Consumed by
// `recoverIfMissing`, which must JOIN it rather than skip past it: acting on a
// still-empty `workspaces` sends `setCurrent` down its single-workspace
// fallback for no reason (TASK-2200, codex round 4). Cleared in `loadAll`'s
// own `finally`, so a failed request does not leave a dead promise behind for
// the next caller to await.
let inFlightLoadAll: Promise<void> | null = null;

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
		// Published so `recoverIfMissing` can JOIN this request rather than
		// skip past it — see the note there (codex round 4).
		const load = (async () => {
			try {
				workspaces = await api.workspaces.list();
			} finally {
				loading = false;
				inFlightLoadAll = null;
			}
		})();
		inFlightLoadAll = load;
		return load;
	},

	/**
	 * Re-acquire workspace identity when a previous attempt left it missing
	 * (TASK-2200).
	 *
	 * THE DEFECT THIS EXISTS FOR. A cold load while the server is unreachable
	 * leaves `workspaces` empty and `current` null, and NOTHING retried either:
	 * the root layout attempts `loadAll` once per auth resolution, and
	 * `setCurrent` runs once from an effect keyed on a workspace slug that does
	 * not change. Every other caller of both is a user action — a topbar
	 * reorder, the workspace switcher, the create-workspace modal. So when the
	 * server came back the board could recover (its own Retry, and the items
	 * cache) inside a shell with no navigation at all: the sidebar builds its
	 * links from `current`, so with `current` null there are no links to build,
	 * and only F5 fixed it.
	 *
	 * GATED ON THE CONDITION, NOT ON A SIGNAL TYPE. The caller is the workspace
	 * layout's sync subscriber, and it calls this on EVERY sync result rather
	 * than on `full_refresh` alone. That is deliberate and measured: after a
	 * server returns, `/changes` usually SUCCEEDS with nothing to report, so the
	 * result is `caught_up` — the type that means "nothing was missed" is
	 * exactly the one that arrives when everything was missed, because the
	 * cursor was seeded during the outage. Gating recovery on `full_refresh`
	 * would therefore miss the common case. The condition below is the real
	 * gate, and it is cheap: two reads, both false on a healthy session.
	 *
	 * IDEMPOTENT AND SELF-LIMITING. When identity is intact this does nothing
	 * and issues no request. `loading` keeps it from stacking a second list call
	 * on an in-flight one.
	 */
	async recoverIfMissing(ws: string): Promise<void> {
		if (workspaces.length === 0) {
			try {
				// JOIN an in-flight list request rather than skipping past it
				// (codex round 4). The first draft skipped on `!loading` and
				// went straight to `setCurrent`, which — with `workspaces` still
				// empty — falls back to a single-workspace fetch; if THAT failed
				// while the in-flight list succeeded moments later, `current`
				// stayed null and the shell stayed broken until the next sync
				// result. Recoverable, but a wasted round, and the skip was the
				// same "in flight" question `ensureCollections` answers by
				// joining. Got it right in one place and wrong in the other, in
				// one unit; they now answer it the same way.
				await (inFlightLoadAll ?? workspaceStore.loadAll());
			} catch {
				// Still unreachable. Leave both pieces of state as they are —
				// the next sync result asks again, and asking again is the whole
				// mechanism. Swallowing here rather than rethrowing keeps a
				// failure from taking the caller's other subscribers down.
				return;
			}
		}
		// Checked AFTER the list attempt, and against the slug rather than for
		// mere presence: `setCurrent` resolves out of `workspaces` when it can
		// and falls back to a single-workspace fetch when it cannot, so running
		// it second gives it the array to work with. A `current` pointing at a
		// DIFFERENT workspace is also wrong here — this is the recovery path for
		// the workspace the layout is showing.
		if (!current || current.slug !== ws) {
			// `setCurrent` catches its own failures into `current = null`, so
			// there is nothing to catch here and nothing to report: a still-down
			// server simply leaves the condition true for the next attempt.
			await workspaceStore.setCurrent(ws);
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
