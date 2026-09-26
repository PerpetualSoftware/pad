// The choice a user makes when a body write meets edits a tab has not stored
// yet (BUG-3050 U1): KEEP the tab's edits (the save is not sent), or OVERWRITE
// them. Never decided for the user. Modelled on openChildrenDialog: a queue of
// promise-returning requests answered by one dialog mounted in the root layout,
// all abandoned (as KEEP) on an identity change.
import { authStore } from './auth.svelte';

export type PendingEditsKind = 'save' | 'duplicate';

interface PendingRequest {
	itemRef: string;
	kind: PendingEditsKind;
	resolve: (overwrite: boolean) => void;
}

let active = $state<PendingRequest | null>(null);
const queue: PendingRequest[] = [];

function advanceQueue(): void {
	active = queue.shift() ?? null;
}

/** Resolves true to OVERWRITE (or, for a duplicate, copy the stored body anyway), false to keep. */
function request(itemRef: string, kind: PendingEditsKind = 'save'): Promise<boolean> {
	return new Promise<boolean>((resolve) => {
		const entry: PendingRequest = { itemRef, kind, resolve };
		if (active === null) active = entry;
		else queue.push(entry);
	});
}

function answer(overwrite: boolean): void {
	const a = active;
	if (!a) return;
	a.resolve(overwrite);
	advanceQueue();
}

function abandonAll(): void {
	const pending = active ? [active, ...queue] : [...queue];
	queue.length = 0;
	active = null;
	for (const entry of pending) entry.resolve(false);
}

export const pendingEditsDialog = {
	get active(): PendingRequest | null {
		return active;
	},
	request,
	keep: () => answer(false),
	overwrite: () => answer(true),
	abandonAll,
};

authStore.onIdentityChange(() => {
	pendingEditsDialog.abandonAll();
});
