// A general confirmation, answered by one dialog mounted in the root layout
// (TASK-2221, audit C39): call sites `await confirmDialog.request({...})`
// instead of the native, blocking `confirm()`. Modelled on
// pendingEditsDialog / openChildrenDialog: a queue of promise-returning
// requests, all answered NO on an identity change, since a question asked of
// one signed-in user is never the next one's to answer.
import { authStore } from './auth.svelte';

export interface ConfirmRequest {
	title: string;
	message: string;
	/** The confirm button's label, e.g. "Delete". */
	confirmLabel: string;
	/** A destructive confirm renders as the red button. */
	danger?: boolean;
	/** The dismiss button's label; "Cancel" when absent. Name it when the
	 * confirm label is itself a cancel ("Cancel invitation"). */
	cancelLabel?: string;
}

interface Pending extends ConfirmRequest {
	resolve: (ok: boolean) => void;
}

let active = $state<Pending | null>(null);
const queue: Pending[] = [];

function request(req: ConfirmRequest): Promise<boolean> {
	return new Promise<boolean>((resolve) => {
		const entry: Pending = { ...req, resolve };
		if (active === null) active = entry;
		else queue.push(entry);
	});
}

function answer(ok: boolean): void {
	const a = active;
	if (!a) return;
	active = queue.shift() ?? null;
	a.resolve(ok);
}

function abandonAll(): void {
	const pending = active ? [active, ...queue] : [...queue];
	queue.length = 0;
	active = null;
	for (const entry of pending) entry.resolve(false);
}

export const confirmDialog = {
	get active(): ConfirmRequest | null {
		return active;
	},
	request,
	confirm: () => answer(true),
	cancel: () => answer(false),
	abandonAll,
};

authStore.onIdentityChange(() => {
	confirmDialog.abandonAll();
});
