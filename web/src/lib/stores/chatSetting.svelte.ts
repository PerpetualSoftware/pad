// The signed-in user's Chat display preference (TASK-4315). Off by default.
// It is a display preference, not access control: nothing here gates the API,
// and the Chat surfaces (shell, entries, inbox and thread) read it to decide
// whether to show themselves.
//
// The value lives on the auth session's user, so it follows the user across
// reloads and devices, and an identity change drops it with the session.
import { api } from '$lib/api/client';
import { authStore } from '$lib/stores/auth.svelte';

export const chatSetting = {
	/** True only when the server says this user turned Chat on. */
	get enabled(): boolean {
		return authStore.user?.chat_enabled === true;
	},

	/**
	 * Write the caller's own preference. Resolves with the value the server
	 * stored. The session is updated only if the same user is still signed in
	 * when the write settles, so a late answer never lands on the next identity.
	 * A failed write rejects and changes nothing.
	 */
	async set(next: boolean): Promise<boolean> {
		const userId = authStore.userId;
		const isSameIdentity = authStore.identityFence();
		const updated = await api.auth.setChatEnabled(next);
		const stored = updated.chat_enabled ?? next;
		if (isSameIdentity()) authStore.setChatEnabled(userId, stored);
		return stored;
	}
};
