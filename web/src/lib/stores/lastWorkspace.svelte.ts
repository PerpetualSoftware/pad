// The workspace the user was last in, in memory only (TASK-4044). /messages is
// outside any workspace, so its "Back" link needs to remember where the user
// came from. A reload or a direct landing has nothing stored, and Back falls
// back to /console. Never persisted: it is one account's navigation history.
import { authStore } from '$lib/stores/auth.svelte';

export interface LastWorkspace {
	slug: string;
	owner_username: string;
}

class LastWorkspaceStore {
	current = $state<LastWorkspace | null>(null);

	constructor() {
		authStore.onIdentityChange(() => {
			this.current = null;
		});
	}

	note(owner_username: string, slug: string): void {
		if (!slug) return;
		this.current = { slug, owner_username };
	}

	clear(): void {
		this.current = null;
	}
}

export const lastWorkspace = new LastWorkspaceStore();
