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

function notifyIdentityChange() {
	const id = session?.user?.id ?? '';
	if (!identityEstablished) {
		identityEstablished = true;
		notifiedUserId = id;
		return;
	}
	if (id === notifiedUserId) return;
	notifiedUserId = id;
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
				if (isCurrent()) {
					session = null;
					notifyIdentityChange();
				}
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
