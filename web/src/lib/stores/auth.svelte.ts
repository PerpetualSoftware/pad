import { api, type AuthSession } from '$lib/api/client';

let session = $state<AuthSession | null>(null);
let loading = $state(false);

export const authStore = {
	get session() { return session; },
	get user() { return session?.user ?? null; },
	get userId() { return session?.user?.id ?? ''; },
	get authenticated() { return session?.authenticated ?? false; },
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

	clear() {
		session = null;
	}
};
