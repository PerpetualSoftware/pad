// The one entry every create/restore/import door calls on a plan-limit
// refusal (PLAN-3291 DR-3, TASK-3293). It used to be a 6-second toast with a
// generic billing link; it now raises the persistent plan-limit prompt
// (TASK-3601), which names the limit and offers the payer Upgrade, anyone else
// "ask <owner>", and nothing where billing is off or commerce is not allowed.
// The name is kept so the 46 call sites stay as they were.
import { isPlanLimitError, type PadApiError } from '$lib/api/client';
import { planLimitPrompt } from './planLimitPrompt.svelte';

/**
 * `scope` puts the prompt in that flow's inline host when one is mounted
 * (invite, import, create-workspace, new-item); `blocked` names what was
 * stopped, for "Continue: <blocked>" after an upgrade.
 */
export function showPlanLimitToast(err: PadApiError, opts: { scope?: string; blocked?: string } = {}): void {
	planLimitPrompt.raise(err, opts);
}

/**
 * The message an editor's failure callback receives after a plan-limit
 * refusal has already raised the prompt (TASK-3619: an upload or image edit
 * past the storage limit). The callback skips its "Couldn't upload" toast on
 * it, so the user sees the prompt, not both.
 */
export const PLAN_LIMIT_PROMPTED = 'plan-limit-prompted';

/**
 * For an editor's upload/transform callback: a plan-limit refusal comes back
 * as an Error carrying PLAN_LIMIT_PROMPTED, having raised the prompt only when
 * `sameIdentity` says the user who started the request is still signed in. A
 * refusal for the previous identity is dropped silently, neither prompted nor
 * toasted, since its message names that identity's owner and plan (codex r2).
 * Any other error is returned as it was. The caller throws the result.
 */
export function promptOnPlanLimit(err: unknown, sameIdentity: () => boolean): unknown {
	if (!isPlanLimitError(err)) return err;
	if (sameIdentity()) showPlanLimitToast(err);
	return new Error(PLAN_LIMIT_PROMPTED);
}
