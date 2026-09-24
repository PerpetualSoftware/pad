/**
 * BUG-3013: the words a relation value that does not resolve must never be
 * described with.
 *
 * A miss has three causes the client cannot tell apart: legacy free text, a
 * target that is gone, and a target that is alive in a collection this member
 * may not see (see `UNRESOLVED_LABEL` in `$lib/collections/relationGroups`).
 * Each word below asserts one cause, and the third cause makes every one of
 * them false for some viewer. The list is the bug's own list plus the phrase
 * the old tooltip used ("does not match any item in this workspace") and the
 * old FieldEditor comment's "does not point at anything", in either apostrophe.
 * "Does not resolve" is deliberately absent: qualified by "to an item you can
 * see" it is the neutral wording, not a claim.
 *
 * Checks VISIBLE text and the hover / assistive text (`title`, `aria-label`),
 * because the old claim lived in a `title` and a text-only check passes it.
 */
export const EXISTENCE_CLAIM =
	/deleted|removed|missing|does ?n[o'\u2019]t exist|no longer exists|does ?n[o'\u2019]t match|does ?n[o'\u2019]t point|points? (?:to|at) nothing|not found/i;

/** Every visible, hover and aria string under `root` that makes the claim. */
export function existenceClaimsIn(root: Element): string[] {
	const texts = [root.textContent ?? ''];
	// `root` itself included: the chip element is usually the one carrying the
	// `title`, and querySelectorAll searches descendants only.
	for (const el of [root, ...root.querySelectorAll('[title], [aria-label]')]) {
		texts.push(el.getAttribute('title') ?? '', el.getAttribute('aria-label') ?? '');
	}
	return texts.filter((t) => EXISTENCE_CLAIM.test(t));
}
