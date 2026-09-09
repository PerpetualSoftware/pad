import { api } from '$lib/api/client';
import type { Workspace, WorkspaceMembership } from '$lib/types';
import * as perms from '$lib/utils/permissions';
import { createKeyedSingleFlight } from './singleFlight';

let workspaces = $state<Workspace[]>([]);
let current = $state<Workspace | null>(null);
let currentMembership = $state<WorkspaceMembership | null>(null);
// Whether `currentMembership` is an ANSWER or merely NOT YET FETCHED (BUG-2978).
// It is null in both cases, which makes the two indistinguishable to consumers —
// and they are opposites: one is "wait", the other is "no access".
//
// False from the moment a call that will replace membership begins — which
// includes resolving the workspace itself, and the part of a create that
// follows a successful API call — until that call settles: a fetched
// membership, a 403, or a workspace that did not resolve.
//
// A create that THROWS is outside all of that. It changes no state at all,
// because a failed create says nothing about the workspace you are still
// looking at; whatever this flag was before such a create, it still is.
let membershipKnown = $state(false);
let loading = $state(false);

// Monotonic sequence guarding async /me responses against navigation races.
// Each setCurrent / create call increments the counter; a /me response is
// only applied if its captured token still matches at resolution time. This
// prevents a slow /me for workspace A from clobbering a freshly-set
// membership for workspace B.
let membershipSeq = 0;

// The keyed single-flight loader fencing `loadAll` (TASK-2947) — the same
// primitive `collections.svelte.ts` uses, which is the point: generation,
// join slot and ownership-guarded cleanup had been hand-rolled in both stores
// and drifted three times in one afternoon (TASK-2200 rounds 3, 4 and 5).
//
// `loadAll` takes no argument, so there is one key. It is still the KEYED
// primitive rather than a bespoke unkeyed one, because a second shape would be
// the sibling that drifts.
//
// `recoverIfMissing` JOINS an in-flight run via `inFlightFor` rather than
// skipping past it: acting on a still-empty `workspaces` sends `setCurrent`
// down its single-workspace fallback for no reason (codex round 4).
const LOAD_ALL_KEY = 'all';
const loadAllFlight = createKeyedSingleFlight<string>({
	setLoading: (v) => { loading = v; },
});

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
 *
 * That conflation is safe for gating an affordance and NOT safe for caching one:
 * see `membershipKnown` below, which is what tells the two apart.
 */

export const workspaceStore = {
	get workspaces() { return workspaces; },
	get current() { return current; },
	get loading() { return loading; },

	get currentMembership() { return currentMembership; },

	/**
	 * True once a membership fetch has SETTLED for the current workspace, so a
	 * null `currentMembership` means "no access" rather than "not yet loaded".
	 *
	 * False spans the whole replacing call — workspace resolution and creation
	 * included, not just the `/me` request itself.
	 *
	 * Consumers that cache a permission to avoid flickering during that window
	 * (BUG-2978) must gate on this rather than on `currentMembership !==
	 * null`: gating on non-null holds the last good answer forever when the
	 * answer becomes a definitive denial — a removed member or a 403 keeps
	 * owner-only affordances on screen. The server remains the enforcement
	 * boundary either way, but the UI should not lie.
	 */
	get membershipKnown() { return membershipKnown; },

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
		return loadAllFlight.run(LOAD_ALL_KEY, async ({ isLatest }) => {
			const list = await api.workspaces.list();
			// WHICH RESPONSE COMMITS (TASK-2947, and a behaviour change rather
			// than a move). Two overlapping calls used to leave the OLDER list in
			// `workspaces` if it resolved last — this store had the ownership
			// guard on its CLEANUP and none on its COMMIT, where
			// `collections.svelte.ts` had both. It was unreachable from the
			// recovery path, which only ever joins, and it was named in this
			// store's own comment as pre-existing; extracting the primitive is
			// where it closes, because the primitive owns the rule.
			if (!isLatest()) return;
			workspaces = list;
		});
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
				await (loadAllFlight.inFlightFor(LOAD_ALL_KEY) ?? workspaceStore.loadAll());
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
		membershipKnown = false;

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
				if (seq === membershipSeq) { currentMembership = m; membershipKnown = true; }
			} catch {
				// A 403 or a removed member is an ANSWER, not a pending state.
				if (seq === membershipSeq) { currentMembership = null; membershipKnown = true; }
			}
		} else if (seq === membershipSeq) {
			// The workspace itself did not resolve (404, or no access): also an
			// answer, and the same one.
			membershipKnown = true;
		}
	},

	async create(data: { name: string; description?: string; template?: string }) {
		// CLEAR NOTHING UNTIL THE CREATE HAS SUCCEEDED (codex round 3).
		//
		// This used to clear membership at entry, mirroring `setCurrent` — but
		// `setCurrent` is switching to a workspace it already names, while a
		// create that FAILS leaves the current workspace exactly as it was. So
		// the clear was making an assertion about the wrong workspace, and my
		// round-2 fix made that assertion louder rather than removing it:
		// settling the flag turned "we don't know" into "no access to the
		// workspace you are still looking at", which hid a mounted settings
		// page's owner controls until the next `setCurrent`. A failed create
		// says nothing about the current membership, so it now changes nothing.
		//
		// OBSERVE the sequence token at entry, CLAIM it only on success (codex
		// round 4). Three orderings have to come out right and the obvious two
		// spellings each get one wrong:
		//
		//  - claiming at entry (the original) makes a FAILED create invalidate a
		//    `setCurrent` that is still in flight — its writes are discarded on
		//    the seq check and membership is left unresolved with nothing coming
		//    to fix it;
		//  - claiming only after the call lets a create that STARTED EARLIER but
		//    resolved later override a navigation the user began in between.
		//
		// Reading the token at entry and comparing before claiming gives all
		// three: a navigation started after this create wins (it bumped the
		// token), a navigation still in flight from before loses (this create is
		// the newer intent), and a failed create claims nothing and therefore
		// invalidates nothing.
		const entrySeq = membershipSeq;
		const ws = await api.workspaces.create(data);
		if (membershipSeq !== entrySeq) return ws;
		const seq = ++membershipSeq;
		currentMembership = null;
		membershipKnown = false;
		workspaces = [...workspaces, ws];
		current = ws;
		// New workspace — refresh membership for the just-created context.
		try {
			const m = await api.workspaces.me(ws.slug);
			if (seq === membershipSeq) { currentMembership = m; membershipKnown = true; }
		} catch {
			if (seq === membershipSeq) { currentMembership = null; membershipKnown = true; }
		}
		return ws;
	}
};
