import { api, type AuthSession } from '$lib/api/client';

let session = $state<AuthSession | null>(null);
let loading = $state(false);

export const authStore = {
	get session() { return session; },
	get user() { return session?.user ?? null; },
	get userId() { return session?.user?.id ?? ''; },
	get authenticated() { return session?.authenticated ?? false; },
	get cloudMode() { return session?.cloud_mode ?? false; },
	get loading() { return loading; },

	async load() {
		loading = true;
		try {
			session = await api.auth.session();
		} catch (err) {
			session = null;
			loading = false;
			throw err; // Re-throw so callers can distinguish fetch errors from "not authenticated".
		} finally {
			loading = false;
		}
		return session;
	},

	// ensureLoaded returns the cached session when one exists, otherwise fetches it.
	// Use this on auth pages (register, forgot-password, etc.) that navigate in via
	// SPA routing after the user has logged out — logout clears the store, and the
	// root layout's one-shot onMount doesn't re-run on subsequent navigation, so a
	// page that relies on session fields (e.g. cloud_mode) would otherwise see
	// stale nulls and render the self-hosted branch on Pad Cloud.
	async ensureLoaded() {
		if (session) return session;
		return this.load();
	},

	clear() {
		session = null;
	}
};
