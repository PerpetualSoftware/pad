// The one place a plan-limit refusal becomes something the user sees
// (TASK-3601; it replaced the 6-second toast). It stays until dismissed.
// A flow with a natural place for it (invite, import, create workspace, new
// item) mounts an inline host for its scope; every other refusal shows in
// the global host.
import type { PadApiError } from '$lib/api/client';
import { authStore } from '$lib/stores/auth.svelte';
import { planLimitPromptModel, type PlanLimitDetails, type PlanLimitPromptModel } from './planLimitPrompt';

export interface PlanLimitPromptState extends PlanLimitPromptModel {
	id: number;
	/** The inline host this prompt belongs to, or null for the global one. */
	scope: string | null;
	/** What was blocked, for "Continue: <action>" after an upgrade. */
	blocked: string;
}

let current = $state<PlanLimitPromptState | null>(null);
let seq = 0;
const inlineHosts = new Map<string, number>();

export const planLimitPrompt = {
	get current(): PlanLimitPromptState | null {
		return current;
	},
	/** Show the prompt for this refusal, in `scope`'s inline host if one is mounted. */
	raise(err: PadApiError, opts: { scope?: string; blocked?: string } = {}): void {
		const model = planLimitPromptModel(err.message, err.details as PlanLimitDetails | undefined, {
			commerceAllowed: authStore.commerceAllowed,
			billingAvailable: authStore.billingAvailable,
		});
		const scope = opts.scope && (inlineHosts.get(opts.scope) ?? 0) > 0 ? opts.scope : null;
		current = { ...model, id: ++seq, scope, blocked: opts.blocked ?? '' };
	},
	dismiss(): void {
		current = null;
	},
	/** An inline host for `scope` is mounted; the returned function unmounts it. */
	registerInlineHost(scope: string): () => void {
		inlineHosts.set(scope, (inlineHosts.get(scope) ?? 0) + 1);
		return () => {
			const n = (inlineHosts.get(scope) ?? 1) - 1;
			if (n <= 0) inlineHosts.delete(scope);
			else inlineHosts.set(scope, n);
			// A prompt whose host went away (the dialog closed) goes with it.
			if (n <= 0 && current?.scope === scope) current = null;
		};
	},
};

// It names the account owner and the account's limits: not the next user's to
// see after a sign-out or account switch (as toastStore.clearAll does).
authStore.onIdentityChange(() => {
	current = null;
});
