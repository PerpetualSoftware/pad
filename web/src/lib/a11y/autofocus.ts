/**
 * Focus an element when it mounts (TASK-2259).
 *
 * The auth pages put the cursor in their first field, and the 2FA step puts it
 * in the code field the moment that step renders: the code input mounts when
 * the step changes, so mounting is the right trigger and no $effect on the step
 * is needed.
 *
 * An action rather than the `autofocus` attribute: Svelte flags the attribute
 * (a11y_autofocus), and it fires only on the initial page load, not when a
 * later step mounts a new input.
 *
 * It never takes focus away from something the person already focused, such as
 * a field they clicked before hydration finished. `enabled` lets a page pick
 * which of several inputs is first, e.g. the password when the email is
 * read-only. It is read at mount only.
 */
export function autofocus(node: HTMLElement, enabled: boolean = true): void {
	if (!enabled) return;
	const active = document.activeElement;
	if (active && active !== document.body && active !== node) return;
	node.focus();
}
