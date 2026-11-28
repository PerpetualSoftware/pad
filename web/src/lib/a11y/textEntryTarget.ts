// Whether a keydown target is a text-entry control, where ESC belongs to the
// control (clearing or blurring it) rather than to the page's pane-close
// handler (PLAN-2105 / TASK-2118). Unknown input types default to "text"
// (treated as text-editing) so a host errs toward preserving local handling.
const NON_TEXT_INPUT_TYPES = new Set([
	'checkbox', 'radio', 'button', 'submit', 'reset', 'range', 'color', 'file', 'image',
]);

export function isTextEntryTarget(el: HTMLElement | null | undefined): boolean {
	if (!el) return false;
	if (el.isContentEditable) return true;
	const tag = el.tagName;
	if (tag === 'TEXTAREA') return true;
	if (tag === 'INPUT') return !NON_TEXT_INPUT_TYPES.has((el as HTMLInputElement).type);
	return false;
}
