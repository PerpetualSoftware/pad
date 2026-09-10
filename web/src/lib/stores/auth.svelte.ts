import { api, type AuthSession } from '$lib/api/client';

let session = $state<AuthSession | null>(null);
let loading = $state(false);
// Coalesces concurrent load() calls so the root layout and auth-page onMount
// (register, forgot-password) can both request the session without firing
// duplicate /auth/session requests — or, worse, having a late failure from a
// duplicate fetch overwrite a successful fetch's session=null.
let inflight: Promise<AuthSession | null> | null = null;
// Bumps on clear(). Any fetch started in a previous generation is stale: its
// success must not resurrect a logged-out session, and its finally must not
// clobber a new inflight that started after clear(). Readers only write
// state when the generation they captured at fetch-start still matches.
let generation = 0;

// Identity-change subscribers (BUG-2991).
//
// `workspace.svelte.ts` holds state that BELONGS to the signed-in user but is
// not keyed by them — `current`, `workspaces`, and a `currentMembership` that
// has already been published. Logout is an SPA navigation, so none of it is
// torn down by a page load, and a clean account change with no `/me` in flight
// left the previous user's workspace list and live permission state on screen.
// `settleIfCurrent`'s identity fence only covers the case where a settle is
// racing the change; it cannot cover the case where nothing is racing at all.
//
// A subscription rather than a call at each sign-out site, for the same reason
// `answeredMembership` is keyed by user rather than cleared on logout: it makes
// the invalidation STRUCTURAL. There are two `authStore.clear()` sites today
// and a third would not have to remember anything, and it fires on sign-IN as a
// different user too, which no logout path could have covered.
//
// The direction of the dependency is unchanged: `workspace.svelte.ts` imports
// this module and registers here, so this module still imports nothing of it.
const identityListeners = new Set<() => void>();
// The id whose listeners have already been notified. Compared rather than
// assumed, so a `load()` that returns the SAME user (the common case — the root
// layout fetches the session on every cold start) notifies nobody and cannot
// clear a store mid-session.
let notifiedUserId = '';
// Whether an identity has been ESTABLISHED yet. The first resolution of the
// session is not a change from anything, and firing on it would be a live
// hazard rather than a no-op: the root layout loads the session and the
// workspace list concurrently, so a cold start whose `/auth/session` lands
// second would clear a `workspaces` array that had just been populated. A
// cold start has nothing stale to drop, so the first answer only records the
// baseline. Every later transition — including back to '' on sign-out — fires.
let identityEstablished = false;

// The identity EPOCH (BUG-3005): a monotonic counter bumped on exactly the
// transitions `onIdentityChange` fires on.
//
// WHY A COUNTER AND NOT THE USER ID. A store that starts a request as one user
// and commits its response later needs to ask "is this still the identity I
// issued under?", and a user id answers a WEAKER question — "is this a
// different user?". The two come apart on sign out of A, in as B, back in as
// A: a request issued during the FIRST A session settles, compares its
// captured id against the current one, finds them equal, and commits data
// belonging to a session that has ended. An ordinal cannot be confused that
// way, because the epoch it captured is strictly lower than the one it settles
// into. `workspace.svelte.ts`'s `userId` fence has exactly that hole; it is
// narrower than the primary defect (it needs a request to outlive two identity
// changes) and it is the same class.
//
// NOT BUMPED ON THE BASELINE, matching `notifyIdentityChange`'s own early
// return: the first resolution of the session is not a change from anything,
// and a fence captured before an identity was established has nothing stale to
// refuse.
//
// THE RESIDUAL THAT LEAVES, stated rather than discovered (codex round 1).
// A request issued before the baseline resolves carries whatever cookie the
// browser had; the baseline then reports whoever that cookie belongs to. Those
// are the same principal unless the cookie CHANGED mid-flight — another tab
// signing out and in — in which case the pre-baseline request settles under a
// fence that still says current, and commits.
//
// CORRECTED (codex round 2). This comment previously justified the exemption by
// saying the root layout issues the workspace list concurrently with
// /auth/session, so refusing pre-baseline settles would empty the app. That is
// FALSE, and I had taken it from the `identityEstablished` comment below rather
// than reading the layout: `workspaceStore.loadAll()` is gated on `authReady`
// (routes/+layout.svelte), which flips only after `authStore.load()` resolves,
// and the layout renders NO children until then — so no route can issue a
// fenced request before the baseline either.
//
// What that means for the residual: on the ORDINARY path it needs a fence
// captured by something that runs before any child renders, and the only
// pre-baseline request there is `authStore.load()` itself, which takes no
// fence. TWO EXCEPTIONS, both real (codex round 3, correcting this comment a
// second time): the share-page branch sets `authReady` WITHOUT calling
// `authStore.load()` at all, and the auth-FAILURE path proceeds deliberately —
// so children can render and fetch with no baseline established in either. The
// residual is therefore narrow rather than unreachable, and it is a property of
// the callers rather than of this code.
//
// The exemption itself stays for a different and simpler reason: the bump and
// the listener notification are the same event, and firing listeners on the
// baseline is a live hazard the `identityEstablished` comment below documents.
// Separating them would mean two signals where one is honest.
//
// The honest boundary is therefore: the epoch covers identity changes THIS TAB
// observed, and a cross-tab change during the pre-baseline window is outside
// it.
//
// Distinct from `generation` above, which bumps ONLY on `clear()` and fences
// this module's own `/auth/session` fetches. A sign-in as a different user
// through `load()` moves the identity without touching `generation`, so
// `generation` cannot serve as the epoch — it would miss the swap.
// `$state`, unlike `generation` and `notifiedUserId` beside it, because this
// one is READ FROM A TEMPLATE: the workspace layout keys its leaf-page block on
// it (`{#key authStore.identityEpoch}`) to remount route components whose own
// state is not identity-scoped. A plain `let` is invisible to the template and
// the block never re-runs — which is exactly how the first version of that
// remount shipped, silently, until a test counted mounts (codex round 2).
let identityEpoch = $state(0);

function notifyIdentityChange() {
	const id = session?.user?.id ?? '';
	if (!identityEstablished) {
		identityEstablished = true;
		notifiedUserId = id;
		return;
	}
	if (id === notifiedUserId) return;
	notifiedUserId = id;
	// BEFORE the listeners, not after. A listener may capture a fence while it
	// runs — dropping state and immediately reloading is the expected shape —
	// and a fence captured under the OLD epoch would refuse that reload's own
	// response.
	//
	// THE CONTRACT THIS PLACES ON LISTENERS (codex round 1): a fence captured
	// inside a listener passes, so a listener must reload for the identity that
	// is signed in NOW and never re-issue a request built from the identity it
	// was just told about. The fence cannot check this for them — it compares
	// epochs, and by the time a listener runs the epoch is already the new one.
	// Every listener on this branch satisfies it, and most satisfy it by doing
	// nothing further: clearing state and stopping cannot re-issue anything.
	// The two that DO reload — the workspace layout and the starred page — read
	// the workspace from the live route rather than from anything captured
	// before the change (codex round 2 read "stops" as a claim that they all
	// reload, so this says which is which).
	identityEpoch++;
	for (const fn of identityListeners) fn();
}

export const authStore = {
	get session() { return session; },
	get user() { return session?.user ?? null; },
	get userId() { return session?.user?.id ?? ''; },
	get authenticated() { return session?.authenticated ?? false; },
	get cloudMode() { return session?.cloud_mode ?? false; },
	// mcpPublicUrl is the canonical URL clients paste into their MCP-capable
	// agent. Empty string ('') when PAD_MCP_PUBLIC_URL is unset on the server.
	// Components that conditionally render Remote-MCP onboarding UI should
	// branch on `authStore.mcpPublicUrl !== ''`.
	get mcpPublicUrl() { return session?.mcp_public_url ?? ''; },
	// billingAvailable is true when the server has PAD_BILLING_AVAILABLE=true
	// AND is in cloud mode. Components gate "Upgrade to Pro" Stripe CTAs on
	// this value — false means the CTA is hidden entirely, not just disabled.
	// TASK-800.
	get billingAvailable() { return session?.billing_available ?? false; },
	// emailConfigured is false on a self-host instance with no transactional
	// email provider wired. Defaults to true when absent (older servers, or
	// before the session loads) so reset/invite flows keep their normal copy
	// unless the server explicitly says email is off. The /forgot-password
	// page swaps to host-recovery guidance when this is false.
	get emailConfigured() { return session?.email_configured ?? true; },
	// emailVerified is false ONLY for a Pad Cloud self-serve signup that hasn't
	// confirmed its email yet (PLAN-1933 DR-3 / TASK-1940). Defaults to TRUE
	// when the field is absent — older servers, self-hosted instances (which
	// never mint unverified users), OAuth/invited/admin-created accounts, and
	// the pre-load window. This default is load-bearing: the verification
	// banner gates on `!emailVerified`, so a missing field or self-host must
	// NEVER surface it. Mirrors the `emailConfigured ?? true` pattern.
	get emailVerified() { return session?.user?.email_verified ?? true; },
	get loading() { return loading; },

	async load() {
		if (inflight) return inflight;
		loading = true;
		const myGeneration = generation;
		const isCurrent = () => generation === myGeneration;
		inflight = api.auth.session()
			.then((s) => {
				if (isCurrent()) {
					session = s;
					notifyIdentityChange();
				}
				return session;
			})
			.catch((err) => {
				if (isCurrent()) session = null;
				// DELIBERATELY NO `notifyIdentityChange()` HERE (BUG-2991, codex
				// round 9). A rejection is a FETCH ERROR, not an identity
				// signal: "not authenticated" comes back as a resolved session
				// (that is what the re-throw below exists to distinguish), so a
				// rejected `/auth/session` says a request failed and nothing
				// about who is signed in.
				//
				// Treating it as a sign-out was destructive rather than merely
				// wrong. Listeners drop the workspace store, re-arm the layout's
				// load latch and CLOSE the create-workspace modal, and an
				// in-flight create or import then returns without a toast, a
				// navigation or a close — so a user who is still legitimately
				// signed in lost a successful operation, silently, because one
				// session poll failed. `/console/billing` polls `load()`, so
				// this is reachable in ordinary use rather than exotic.
				//
				// `session = null` still happens, because that is the state the
				// rest of the app already had for a failed load; what changes is
				// that nobody is told an identity CHANGED. A real sign-out goes
				// through `clear()`, which does notify.
				throw err; // Re-throw so callers can distinguish fetch errors from "not authenticated".
			})
			.finally(() => {
				if (isCurrent()) {
					loading = false;
					inflight = null;
				}
			});
		return inflight;
	},

	// ensureLoaded returns the cached session when one exists, otherwise fetches it.
	// Use this on auth pages (register, forgot-password, etc.) that navigate in via
	// SPA routing after the user has logged out — logout clears the store, and the
	// root layout's one-shot onMount doesn't re-run on subsequent navigation, so a
	// page that relies on session fields (e.g. cloud_mode) would otherwise see
	// stale nulls and render the self-hosted branch on Pad Cloud. Concurrent calls
	// coalesce through load()'s in-flight promise.
	async ensureLoaded() {
		if (session) return session;
		return this.load();
	},

	/**
	 * Register a listener fired when the SIGNED-IN USER changes — sign-out,
	 * sign-in, or a swap between two accounts. Returns an unsubscribe function.
	 *
	 * Fired only on a real change of user id, so a session refetch that returns
	 * the same user is silent. See `identityListeners` above for why this exists
	 * rather than a call at each sign-out site.
	 */
	onIdentityChange(fn: () => void): () => void {
		identityListeners.add(fn);
		return () => identityListeners.delete(fn);
	},

	/**
	 * The current identity epoch — a monotonic counter of identity CHANGES.
	 * Read it to compare two moments; use `identityFence()` to guard a settle.
	 */
	get identityEpoch() { return identityEpoch; },

	/**
	 * Capture the current identity, and return a predicate that says whether it
	 * is still the current one.
	 *
	 * The intended shape, for any store that reads user-scoped data:
	 *
	 *     const isSameIdentity = authStore.identityFence();
	 *     const result = await api.something();
	 *     if (!isSameIdentity()) return;   // issued as someone else; drop it
	 *
	 * Offered as a helper rather than left to each call site because this store
	 * and `workspace.svelte.ts` already hand-rolled the same three parts of a
	 * single-flight loader and drifted three times in one afternoon
	 * (`singleFlight.ts`'s own note). A fence is smaller and there are more
	 * places that need one.
	 *
	 * It is NOT a navigation fence and does not replace one. A workspace switch
	 * under one identity leaves the epoch alone, which is correct — that race is
	 * about WHICH WORKSPACE the answer describes, not about who asked. Stores
	 * that can race a switch need both, and neither subsumes the other.
	 */
	identityFence(): () => boolean {
		const captured = identityEpoch;
		return () => identityEpoch === captured;
	},

	clear() {
		session = null;
		generation++;
		notifyIdentityChange();
		// Drop the in-flight promise reference so the next ensureLoaded()/load()
		// call fires a fresh fetch rather than attaching to a pre-logout request.
		// The old promise may still resolve/reject in the background; the
		// generation guard above prevents it from writing to any state.
		inflight = null;
		loading = false;
	}
};
