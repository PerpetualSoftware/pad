// The choice a user makes when a body write meets edits a tab has not stored
// yet (BUG-3050 U1): KEEP the tab's edits (the save is not sent), or OVERWRITE
// them. Never decided for the user. Modelled on openChildrenDialog: a queue of
// promise-returning requests answered by one dialog mounted in the root layout,
// all abandoned (as KEEP) on an identity change.
import { authStore } from './auth.svelte';

export type PendingEditsKind = 'save' | 'duplicate' | 'excerpt' | 'stale';
/** A stale raw save's answer (BUG-3540): reload the stored body, overwrite it,
 *  or dismiss (keep editing; nothing is sent). */
export type StaleAnswer = 'reload' | 'overwrite' | 'dismiss';
/** 'set_aside': the edits were set aside by an editor upgrade (BUG-3244) and
 *  opening the item will not store them, so the dialog must not say it will. */
export type PendingEditsReason = 'pending' | 'set_aside';

interface PendingRequest {
	itemRef: string;
	kind: PendingEditsKind;
	reason: PendingEditsReason;
	resolve: (overwrite: boolean) => void;
	/** Set for kind 'stale' only: its three-way answer. */
	resolveStale?: (answer: StaleAnswer) => void;
}

let active = $state<PendingRequest | null>(null);
const queue: PendingRequest[] = [];

function advanceQueue(): void {
	active = queue.shift() ?? null;
}

/** Resolves true to OVERWRITE (or, for a duplicate, copy the stored body anyway), false to keep. */
function request(itemRef: string, kind: PendingEditsKind = 'save', reason: PendingEditsReason = 'pending'): Promise<boolean> {
	return new Promise<boolean>((resolve) => {
		const entry: PendingRequest = { itemRef, kind, reason, resolve };
		if (active === null) active = entry;
		else queue.push(entry);
	});
}

/**
 * BUG-3540: a raw save met a body that changed since the raw editor's text was
 * seeded. Resolves 'reload', 'overwrite' or 'dismiss'; there is no default, and
 * Escape or close dismisses (nothing is sent, nothing is replaced).
 */
function requestStale(itemRef: string): Promise<StaleAnswer> {
	return new Promise<StaleAnswer>((resolve) => {
		const entry: PendingRequest = {
			itemRef,
			kind: 'stale',
			reason: 'pending',
			resolve: (overwrite) => resolve(overwrite ? 'overwrite' : 'dismiss'),
			resolveStale: resolve,
		};
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

function reload(): void {
	const a = active;
	if (!a) return;
	if (a.resolveStale) a.resolveStale('reload');
	else a.resolve(false);
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
	requestStale,
	keep: () => answer(false),
	reload,
	overwrite: () => answer(true),
	abandonAll,
};

authStore.onIdentityChange(() => {
	pendingEditsDialog.abandonAll();
});
