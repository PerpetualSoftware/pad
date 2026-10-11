// The one entry every create/restore/import door calls on a plan-limit
// refusal (PLAN-3291 DR-3, TASK-3293). It used to be a 6-second toast with a
// generic billing link; it now raises the persistent plan-limit prompt
// (TASK-3601), which names the limit and offers the payer Upgrade, anyone else
// "ask <owner>", and nothing where billing is off or commerce is not allowed.
// The name is kept so the 46 call sites stay as they were.
import type { PadApiError } from '$lib/api/client';
import { planLimitPrompt } from './planLimitPrompt.svelte';

/**
 * `scope` puts the prompt in that flow's inline host when one is mounted
 * (invite, import, create-workspace, new-item); `blocked` names what was
 * stopped, for "Continue: <blocked>" after an upgrade.
 */
export function showPlanLimitToast(err: PadApiError, opts: { scope?: string; blocked?: string } = {}): void {
	planLimitPrompt.raise(err, opts);
}
