// Signing out with unsaved edits asks first (BUG-3571, lead ruling part 1).
//
// An open item registers what it would lose: `atRisk()` is true while it holds
// edits the server may not have (the collab provider's editsAtRiskOnClose, or a
// dirty raw editor), and `discard()` makes it drop them for good. Every
// sign-out control awaits `confirmSignOut()` before logging out.
//
// Confirming is the user choosing to discard: the item drops its text and
// keeps nothing (no kept draft, no unload save, no second prompt), because
// logout navigates away BEFORE the identity changes, and that unload would
// otherwise ask again and save or keep the text on a shared machine.
import { confirmDialog } from './confirmDialog.svelte';

export interface SignOutGuardSource {
	atRisk: () => boolean;
	discard: () => void;
}

const sources = new Set<SignOutGuardSource>();

// Set once the user confirms signing out over at-risk edits, until the page
// navigates away. Open items read it to stand down every save, keep and
// prompt during that navigation. A logout that fails resets it
// (signOutFailed), so an aborted sign-out does not leave items unable to save.
let discarding = false;

/** Whether a confirmed sign-out discard is under way. */
export function signOutDiscarding(): boolean {
	return discarding;
}

/** The logout after a confirm failed: items save normally again. */
export function signOutFailed(): void {
	discarding = false;
}

/** Register an open item; the returned function unregisters it. */
export function registerSignOutGuard(source: SignOutGuardSource): () => void {
	sources.add(source);
	return () => {
		sources.delete(source);
	};
}

/**
 * Whether signing out may go ahead. Asks only when an open item holds edits
 * at risk; Cancel is the dialog's default. On a confirm every at-risk item
 * discards its edits before this answers true.
 */
export async function confirmSignOut(): Promise<boolean> {
	const atRisk = [...sources].filter((s) => s.atRisk());
	if (atRisk.length === 0) return true;
	const ok = await confirmDialog.request({
		title: 'Sign out and discard unsaved edits?',
		message:
			"An open item has edits that haven't reached the server yet. Signing out discards them, and nothing is kept on this device.",
		confirmLabel: 'Sign out and discard',
		danger: true,
		cancelLabel: 'Stay signed in',
	});
	if (ok) {
		discarding = true;
		for (const s of atRisk) s.discard();
	}
	return ok;
}
