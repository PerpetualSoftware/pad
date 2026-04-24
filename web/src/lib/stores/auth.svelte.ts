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

export const authStore = {
	get session() { return session; },
	get user() { return session?.user ?? null; },
	get userId() { return session?.user?.id ?? ''; },
	get authenticated() { return session?.authenticated ?? false; },
	get cloudMode() { return session?.cloud_mode ?? false; },
	get loading() { return loading; },

	async load() {
		if (inflight) return inflight;
		loading = true;
		const myGeneration = generation;
		const isCurrent = () => generation === myGeneration;
		inflight = api.auth.session()
			.then((s) => {
				if (isCurrent()) session = s;
				return session;
			})
			.catch((err) => {
				if (isCurrent()) session = null;
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

	clear() {
		session = null;
		generation++;
		// Drop the in-flight promise reference so the next ensureLoaded()/load()
		// call fires a fresh fetch rather than attaching to a pre-logout request.
		// The old promise may still resolve/reject in the background; the
		// generation guard above prevents it from writing to any state.
		inflight = null;
		loading = false;
	}
};
