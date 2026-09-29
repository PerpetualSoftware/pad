// The one place a plan-limit refusal becomes a toast (PLAN-3291 DR-3,
// TASK-3293). Every create/restore/import door used to append its own
// " Upgrade to Pro" and link /console/billing, which put a purchase call to
// action inside the mobile apps (BUG-3290). A refusal still says what
// happened everywhere; only where commerce is allowed does it add the upgrade
// copy and link.
import { planLimitMessage, type PadApiError } from '$lib/api/client';
import { authStore } from '$lib/stores/auth.svelte';
import { toastStore } from '$lib/stores/toast.svelte';

export const PLAN_LIMIT_UPGRADE_SUFFIX = ' Upgrade to Pro';
export const BILLING_PATH = '/console/billing';

export function showPlanLimitToast(err: PadApiError): void {
	const message = planLimitMessage(err);
	if (authStore.commerceAllowed) {
		toastStore.show(message + PLAN_LIMIT_UPGRADE_SUFFIX, 'error', 6000, BILLING_PATH);
	} else {
		toastStore.show(message, 'error', 6000);
	}
}
