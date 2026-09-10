import { api } from '$lib/api/client';
import type { Workspace, WorkspaceMembership } from '$lib/types';
import * as perms from '$lib/utils/permissions';
import { createKeyedSingleFlight } from './singleFlight';
// Identity, for the membership answer cache below. `auth.svelte.ts` does not
// import this module, so this direction adds no cycle.
import { authStore } from './auth.svelte';
import { untrack } from 'svelte';

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
// EXCEPT for a workspace this session has already answered once, where it
// stays true across the replacing call and the previous answer keeps being
// served while the refetch runs (TASK-2988, `answeredMembership` below).
// Dropping to "unknown" during a repeat resolution unmounted permission-gated
// blocks that read the store directly, taking the state of any dialog inside
// one with it. A repeat is not constant — `recoverIfMissing` is CALLED on every
// sync result but only re-resolves when `current` is null or names a different
// workspace, and a create makes the latter true while a route stays mounted —
// but it needs no misuse to happen, and the consumer cannot defend itself
// because "unknown" and "denied" look identical from outside.
//
// A create that THROWS is outside all of that. It changes no state at all,
// because a failed create says nothing about the workspace you are still
// looking at; whatever this flag was before such a create, it still is.
let membershipKnown = $state(false);
let loading = $state(false);

// Monotonic sequence guarding async /me responses against navigation races.
// A /me response is only applied if its captured token still matches at
// resolution time, which prevents a slow /me for workspace A from clobbering a
// freshly-set membership for workspace B.
//
// Every `setCurrent` claims the token on entry. `create` OBSERVES it on entry
// and claims only once the workspace exists, so a create that fails or loses a
// selection race increments nothing — see the comment in `create` for the three
// orderings that shapes.
let membershipSeq = 0;

// The last membership ANSWER settled, per user and workspace slug — for the
// life of the PAGE, which outlives a sign-out, so the same user returning after
// a logout can be served an answer from before it
// (TASK-2988). Populated only by `settleMembership`, which is also the only
// writer of `membershipKnown = true` — one function so a future settle site
// cannot record the state without the answer, or the answer without the state.
//
// Read on entry to `setCurrent` to answer a REPEAT resolution of a workspace
// already seen, instead of dropping to "unknown" for the length of the
// refetch. Keyed by the slug being SET (and by user, below), so it can only ever
// serve the grants of that workspace — never the previous one, which is what the
// entry clear exists to prevent.
//
// KEYED BY USER AS WELL AS SLUG, and that is a correctness requirement rather
// than tidiness: logout is an SPA navigation (`authStore.clear()` +
// `goto('/login')`), so this map outlives a sign-out. Keyed by slug alone, the
// NEXT user to resolve the same workspace would be served the PREVIOUS user's
// grants until their own `/me` settled — a cross-account grant leak in the UI
// (codex round 3). Keying on identity makes the CACHE's invalidation
// structural: a different user simply has no entry, so no logout path has to
// remember to clear anything. It does not fence the store's other state —
// `current`, `workspaces` and a `currentMembership` already published are not
// identity-scoped. That used to mean a clean account change with no settle in
// flight left the previous user's live permission state on screen (codex round
// 6); BUG-2991 closed it the same structural way, with the
// `authStore.onIdentityChange` subscription at the foot of this file, so the
// drop happens whether or not anything is racing.
//
// The identity is captured when the CALL starts and carried to its settle, and
// `settleIfCurrent` discards a settle whose captured id no longer matches the
// signed-in one. Taking it at settle time instead would let a `/me` issued as
// one user land under the next user's key — the root layout renders after an
// auth failure, so an empty id is reachable and is not merely a cache miss.
//
// Not a $state: it is never rendered and only ever read here, so tracking it
// would add invalidations with nothing to invalidate.
//
// A cached DENIAL is served too, deliberately — denied is an answer, and a
// denied workspace flickering to "unknown" is the same defect wearing the
// opposite sign. Inherited from the settle sites: a transient /me or
// workspace-resolution failure is recorded as a denial, exactly as those sites
// already treat it as an answer. A serve is normally corrected by the same
// call's settle, but not always and not on a bound: a failed workspace GET
// settles a denial without reaching `/me`, a superseded call returns without
// settling at all (the newer call settles instead), and a request that never
// answers leaves the served value standing indefinitely. Nothing SCHEDULES a
// retry either — `recoverIfMissing` retries on a null or mismatched `current`,
// not on a null membership, which is pre-existing. The server stays the
// enforcement boundary throughout; what is at stake here is only what the UI
// shows.
const answeredMembership = new Map<string, WorkspaceMembership | null>();

/**
 * The signed-in user's id, read WITHOUT establishing a reactive dependency.
 *
 * `setCurrent` reads this synchronously before its first await, and not every
 * caller is inside `untrack` — the settings page calls `load(wsSlug)` straight
 * from an `$effect`. A tracked read there would make that effect re-run on any
 * session change, which is a dependency the caller never asked for and cannot
 * see (codex round 4).
 */
function currentUserId(): string {
	return untrack(() => authStore.userId);
}

/** Cache key for `slug` under `userId`. See `answeredMembership`. */
function membershipKey(userId: string, slug: string): string {
	// A newline separator, which appears in neither a user id (uuid) nor a
	// workspace slug (kebab-case), so no id/slug pair can collide with
	// another by concatenation.
	return `${userId}\n${slug}`;
}

/**
 * Settle membership for `userId`'s view of `slug`: publish the answer and
 * remember it. Every path that turns `membershipKnown` true goes through here.
 *
 * Reached through `settleIfCurrent` rather than called directly, so the two
 * fences a settle needs cannot be forgotten at a call site.
 */
function settleMembership(userId: string, slug: string, m: WorkspaceMembership | null) {
	currentMembership = m;
	membershipKnown = true;
	answeredMembership.set(membershipKey(userId, slug), m);
}

/**
 * Settle only if this call is still the one that speaks for the store.
 *
 * TWO fences, for two different races. `seq` is the navigation fence: a slow
 * `/me` for workspace A must not clobber a membership freshly set for B.
 * `userId` is the identity fence: logout is an SPA navigation, so a `/me`
 * issued as one user can settle after another has signed in — and without this
 * it would both publish that answer and record it under the NEW user's cache
 * key (codex round 4). Neither fence subsumes the other: a sign-in does not
 * advance `membershipSeq`, and a navigation does not change the user.
 *
 * The two fences DISPOSE of a rejected settle differently, and that asymmetry
 * is the point. A superseded call returns silently, because a newer call is
 * already speaking and its state is the right state. An identity mismatch
 * instead CLEARS to unknown: if the answer we were about to publish belongs to
 * a user who is no longer signed in, then so does whatever is published right
 * now — including an answer replayed from the cache a moment ago, which is how
 * a stale owner read would otherwise survive a sign-out (codex round 5).
 * Unknown is the fail-safe reading, since the helpers treat it as no access.
 *
 * It also drops `current`, which is what makes the clear SELF-HEALING rather
 * than terminal (codex round 6). `recoverIfMissing` re-resolves on a null
 * `current` and runs on every sync result, so the new user gets a real answer
 * at the next one; leaving `current` in place would have left them unknown
 * until a navigation, and a sign-in that does not navigate would never get one.
 * The cost is the sidebar's links blanking for that interval — the TASK-2200
 * shape, and the mechanism built for it is exactly what recovers here.
 */
function settleIfCurrent(
	seq: number,
	userId: string,
	slug: string,
	m: WorkspaceMembership | null,
) {
	if (seq !== membershipSeq) return;
	if (userId !== currentUserId()) {
		currentMembership = null;
		membershipKnown = false;
		current = null;
		return;
	}
	settleMembership(userId, slug, m);
}

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
	 * True once membership RESOLUTION has settled for the workspace
	 * `currentMembership` describes, so a null `currentMembership` means "no
	 * access" rather than "not yet loaded". Not necessarily `current`: between
	 * `setCurrent`'s entry and its `current = resolved`, membership already
	 * names the workspace being SET while `current` still names the previous
	 * one. Consumers read grants from the helpers and identity from `current`,
	 * and the grants are the ones for the workspace the route is moving to.
	 * Resolution, not fetch: a workspace that does not resolve at all settles
	 * this without any `/me` request being made.
	 *
	 * False spans the whole replacing call — workspace resolution and creation
	 * included, not just the `/me` request itself — for a workspace this
	 * session has NOT answered before. For one it has, the flag stays true
	 * across the call and the prior answer is served meanwhile (TASK-2988), so
	 * a repeat resolution is invisible to consumers. Only a FIRST resolution
	 * shows "unknown".
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
	 * and issues no request. A concurrent list call is not duplicated either:
	 * `inFlightFor` JOINS the one already running — `loading` is a rendering
	 * signal here and is not consulted for that.
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

		// Drop stale membership so helpers can't briefly answer using the
		// PREVIOUS workspace's grants while /me is in flight — but serve this
		// workspace's own last answer if the session has one, rather than
		// dropping to "unknown" (TASK-2988). Both halves matter and they are
		// about different workspaces: what must never be visible is another
		// workspace's grants, and `answeredMembership` is keyed by the slug
		// being set, so what it serves is only ever this workspace's.
		//
		// Without this, a REPEAT setCurrent for a workspace already resolved
		// once made `membershipKnown` false again, and consumers cannot tell
		// that from "no access" — so a permission-gated block reading the store
		// DIRECTLY unmounted for the length of a refetch, destroying the state
		// of any dialog inside one. Consumers holding a sticky copy (the
		// settings page, the dashboard CTA) rode it out, which is why the four
		// sites this was found at are the ones that did not.
		const entrySlug = typeof ws === 'object' ? ws.slug : ws;
		// Captured once, and used for both the lookup here and the settle below,
		// so this call records under the identity that ISSUED it rather than
		// whoever happens to be signed in when its `/me` comes back.
		const callUser = currentUserId();
		const entryKey = membershipKey(callUser, entrySlug);
		if (answeredMembership.has(entryKey)) {
			// Re-settling the same answer, so this goes through the one settle
			// function rather than writing the two pieces of state here: it
			// keeps `settleMembership` the sole writer of `membershipKnown =
			// true`, which is the claim its own comment makes.
			settleMembership(callUser, entrySlug, answeredMembership.get(entryKey) ?? null);
		} else {
			currentMembership = null;
			membershipKnown = false;
		}

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
				settleIfCurrent(seq, callUser, slug, m);
			} catch {
				// A 403 or a removed member is an ANSWER, not a pending state.
				settleIfCurrent(seq, callUser, slug, null);
			}
		} else {
			// The workspace itself did not resolve (404, or no access): also an
			// answer, and the same one.
			settleIfCurrent(seq, callUser, slug, null);
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
		// Identity captured BEFORE the POST, not after it. A create issued as one
		// user can return after another has signed in, and capturing on success
		// would cache the first user's membership under the SECOND user's key —
		// which is the leak the key was introduced to close (codex round 5).
		const callUser = currentUserId();
		const ws = await api.workspaces.create(data);

		// IDENTITY FENCE, BEFORE ANY STORE MUTATION (BUG-2991).
		//
		// The append below is unconditional and the selection is guarded only by
		// the sequence token — and neither of those is an identity question. A
		// create issued by user A that returns after user B has signed in
		// (logout is an SPA navigation, so no page load tears the store down)
		// put A's workspace into B's list, and, absent an intervening sequence
		// claim, made it B's `current`. The sequence token cannot catch it: a
		// sign-in does not advance `membershipSeq`, which is the same reason
		// `settleIfCurrent` needs two fences rather than one.
		//
		// Refusing to touch the store is the whole fix. The workspace was really
		// created, so `ws` is still returned rather than thrown — the caller
		// asked for a workspace and got one — and what it does with it is
		// outside this store. `authStore.onIdentityChange` has already reset the
		// store for the new user by the time we get here, so there is nothing to
		// clear and nothing to re-resolve; the membership fetch below is skipped
		// with it, since it would settle for a user who is not signed in.
		if (currentUserId() !== callUser) return ws;

		// THE LIST IS ADDITIVE; ONLY THE SELECTION IS RACED (codex round 5).
		// Two concurrent creates both succeed on the server, so both workspaces
		// exist and both belong in `workspaces` — but only one can be the
		// selected one. Appending before the token check means the loser of the
		// selection race is still listed rather than invisible until the next
		// `loadAll`. That loss predates this change: the entry-claim spelling
		// dropped the EARLIER-started create's workspace, this one would have
		// dropped the later-COMPLETING one, and neither is a loss anyone chose.
		//
		// Which create ends up SELECTED is first-to-complete, and is left
		// deliberately unspecified beyond that: with two creates in flight there
		// is no intent to honour, and the list — the part a user would notice
		// missing — no longer depends on the answer.
		workspaces = [...workspaces, ws];
		if (membershipSeq !== entrySeq) return ws;
		const seq = ++membershipSeq;
		currentMembership = null;
		membershipKnown = false;
		current = ws;
		// New workspace — refresh membership for the just-created context.
		try {
			const m = await api.workspaces.me(ws.slug);
			settleIfCurrent(seq, callUser, ws.slug, m);
		} catch {
			settleIfCurrent(seq, callUser, ws.slug, null);
		}
		return ws;
	}
};

/**
 * Drop everything scoped to the signed-in user when the signed-in user changes
 * (BUG-2991).
 *
 * `answeredMembership` is deliberately NOT cleared: it is keyed by user id, so
 * the next user has no entry and the previous user's answers are still correct
 * for them if they sign back in — which is the invalidation TASK-2988 made
 * structural, and clearing it here would undo it.
 *
 * `membershipSeq` is advanced so that anything already in flight — a `/me`, a
 * `setCurrent`, the tail of a `create` — is superseded rather than allowed to
 * write into the new user's store. `settleIfCurrent` would reject those on its
 * own identity fence; the bump also covers the writes that happen BEFORE a
 * settle, which is the half `create` was missing.
 *
 * Registered at module scope rather than called from each sign-out site, so a
 * future sign-out path cannot forget it. Never unsubscribed: this module lives
 * as long as the page does.
 */
authStore.onIdentityChange(() => {
	membershipSeq++;
	workspaces = [];
	current = null;
	currentMembership = null;
	membershipKnown = false;
});
