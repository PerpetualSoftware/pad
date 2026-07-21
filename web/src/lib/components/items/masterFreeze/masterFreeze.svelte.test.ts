import { describe, it, expect, afterEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import FreezeProbe from './FreezeProbe.svelte';
import FieldSaveProbe from './FieldSaveProbe.svelte';

// BUG-2263 — the master/pane freeze is INVISIBLE to the user.
//
// The freeze's only job is to keep exactly one TYPEABLE collab content editor
// (single-owner of the editorStore/activeItem/tab-title singletons). It is NOT a
// data-collision barrier — master and pane are always different items. So while
// peeking, ONLY the content surfaces stay frozen (the rich editor's editable bit,
// the raw editor, the editor bubble/link chrome, and the provider-lifecycle mode
// toggle); EVERY REST surface — fields, title, delete/move/add-relationship,
// children, timeline, star, share, quick-actions, archived restore — stays
// interactive on both sides, gated on `canEdit` (permission) alone. A click flips
// activePane first, so the interaction lands in one gesture.
//
// FreezeProbe renders the CANONICAL gate expressions from ItemDetail, backed by
// the shared `computeMutationsEnabled` helper (which now scopes to content chrome
// only), so the gate predicates can't drift. The running-app assertions live in
// the pane-full-page e2e specs.

function target(): HTMLElement {
	return document.body.appendChild(document.createElement('div'));
}

function text(root: HTMLElement, testid: string): string {
	return root.querySelector(`[data-testid="${testid}"]`)?.textContent ?? '';
}

function present(root: HTMLElement, testid: string): boolean {
	return root.querySelector(`[data-testid="${testid}"]`) != null;
}

function disabled(root: HTMLElement, testid: string): boolean {
	return (root.querySelector(`[data-testid="${testid}"]`) as HTMLButtonElement | null)?.disabled ?? false;
}

// REST surfaces that stay LIVE on the peeking side (invisible freeze).
const REST_LIVE_SURFACES = [
	'delete-btn',
	'move-btn',
	'add-relationship-btn',
	'title-editable',
];

describe('invisible master/pane freeze wiring (BUG-2263)', () => {
	let root: HTMLElement | null = null;
	let instance: ReturnType<typeof mount> | null = null;

	function render(props: {
		canEdit?: boolean;
		peeking?: boolean;
		canRestore?: boolean;
		isOwner?: boolean;
		quickActionsPresent?: boolean;
	}) {
		root = target();
		instance = mount(FreezeProbe, { target: root, props });
		flushSync();
		return root;
	}

	afterEach(() => {
		if (instance) unmount(instance);
		root?.remove();
		instance = null;
		root = null;
	});

	it('peeking=true freezes ONLY the content surfaces; every REST surface stays live', () => {
		const r = render({ canEdit: true, peeking: true });

		// mutationsEnabled still collapses to false while peeking, but now gates
		// only the content-editor chrome.
		expect(text(r, 'mutationsEnabled')).toBe('false');

		// CONTENT bucket — frozen: the editor is not typeable, the raw editor is
		// read-only, and the selection bubble/link chrome is gone (it only appears on
		// a selection, which requires activation).
		expect(text(r, 'editor-editable')).toBe('false');
		expect(text(r, 'raw-readonly')).toBe('true');
		expect(present(r, 'editor-mutation-ui')).toBe(false);
		// The Rich/Markdown toggle now renders on the peeking side too (BUG-2263
		// follow-up): a click activates the side before the guarded provider flip.
		expect(present(r, 'mode-toggle')).toBe(true);

		// REST bucket — INVISIBLE: fields interactive, children/timeline not frozen.
		expect(text(r, 'field-readonly')).toBe('false');
		expect(text(r, 'child-canEdit')).toBe('true');
		expect(text(r, 'child-frozen')).toBe('false');
		expect(text(r, 'timeline-frozen')).toBe('false');

		// Every REST mutation control stays mounted while peeking.
		for (const surface of REST_LIVE_SURFACES) {
			expect(present(r, surface), `${surface} must stay live while peeking`).toBe(true);
		}
		// Title shows its editable affordance (not the viewer heading).
		expect(present(r, 'title-readonly')).toBe(false);
		// Archived restore, star (enabled), Share stay live.
		expect(present(r, 'archived-restore-btn')).toBe(true);
		expect(disabled(r, 'star-btn')).toBe(false);
		expect(present(r, 'share-btn')).toBe(true);
		// TWO documented exceptions (Codex P1) — same-item / same-collection WRITES
		// stay confined to the ACTIVE side. On the peeking side: version restore
		// (writes items.content, collides with the retained Y.Doc) is hidden; and the
		// owner collection-management menu is hidden here because no prompt actions
		// are seeded — see the dedicated exceptions test for the prompts-still-visible
		// case where only the Manage control is gated.
		expect(present(r, 'version-restore-btn')).toBe(false);
		expect(present(r, 'quickactions-menu')).toBe(false);
	});

	it('peeking=false, canEdit=true is identical to peeking=true for every REST surface (invisibility)', () => {
		const r = render({ canEdit: true, peeking: false });

		expect(text(r, 'mutationsEnabled')).toBe('true');
		// The editor's own state (typeable, raw not read-only) + its selection chrome
		// are the ONLY differences from the peeking case. The mode toggle now renders
		// in BOTH states (asserted true here and in the peeking case above).
		expect(text(r, 'editor-editable')).toBe('true');
		expect(text(r, 'raw-readonly')).toBe('false');
		expect(present(r, 'editor-mutation-ui')).toBe(true);
		expect(present(r, 'mode-toggle')).toBe(true);

		// REST surfaces are byte-identical to the peeking case above.
		expect(text(r, 'field-readonly')).toBe('false');
		expect(text(r, 'child-canEdit')).toBe('true');
		expect(text(r, 'child-frozen')).toBe('false');
		expect(text(r, 'timeline-frozen')).toBe('false');
		for (const surface of REST_LIVE_SURFACES) {
			expect(present(r, surface), `${surface} must be live`).toBe(true);
		}
		expect(present(r, 'title-readonly')).toBe(false);
		expect(present(r, 'archived-restore-btn')).toBe(true);
		expect(disabled(r, 'star-btn')).toBe(false);
		expect(present(r, 'share-btn')).toBe(true);
		// (quick-actions + version restore — the two exceptions — are covered in
		// their own test below.)
	});

	it('the two same-item/same-collection WRITE surfaces are confined to the active side; prompt actions stay visible (Codex P1)', () => {
		// Seed prompt actions so the quick-actions menu trigger has read-only content
		// to keep it visible on both sides.
		// Active side (peeking=false): prompts + Manage + version restore all present.
		let r = render({ canEdit: true, peeking: false, isOwner: true, quickActionsPresent: true });
		expect(present(r, 'quickactions-menu')).toBe(true);
		expect(present(r, 'quickactions-prompt')).toBe(true);
		expect(present(r, 'quickactions-manage')).toBe(true);
		expect(present(r, 'version-restore-btn')).toBe(true);
		unmount(instance!);
		root!.remove();
		instance = null;
		root = null;

		// Peeking side: prompts STILL visible (invisible freeze for the read-only
		// part), but the collection-management WRITE (Manage) is gone and version
		// restore is frozen — both would collide (last-write-wins / Y.Doc overwrite).
		r = render({ canEdit: true, peeking: true, isOwner: true, quickActionsPresent: true });
		expect(present(r, 'quickactions-menu')).toBe(true);
		expect(present(r, 'quickactions-prompt')).toBe(true);
		expect(present(r, 'quickactions-manage')).toBe(false);
		expect(present(r, 'version-restore-btn')).toBe(false);
	});

	it('a true viewer (canEdit=false) still sees the read-only forms, regardless of peeking', () => {
		// canEdit=false is a genuine read-only viewer — fields/title degrade to
		// read-only, mutation controls hide. This is PERMISSION, not the freeze,
		// and is identical whether peeking or not.
		for (const peeking of [false, true]) {
			const r = render({ canEdit: false, peeking, isOwner: false });
			expect(text(r, 'field-readonly')).toBe('true');
			expect(present(r, 'title-readonly')).toBe(true);
			expect(present(r, 'title-editable')).toBe(false);
			for (const surface of REST_LIVE_SURFACES) {
				if (surface === 'title-editable') continue;
				expect(present(r, surface), `${surface} hidden for a viewer`).toBe(false);
			}
			// Star stays available to viewers.
			expect(disabled(r, 'star-btn')).toBe(false);
			unmount(instance!);
			root!.remove();
			instance = null;
			root = null;
		}
	});

	it('archived restore rides `canRestore` alone (not peeking) — archived items force canEdit false', () => {
		// canRestore true: restore shows whether peeking or not (invisible freeze).
		for (const peeking of [false, true]) {
			const r = render({ canEdit: false, peeking, canRestore: true });
			expect(present(r, 'archived-restore-btn')).toBe(true);
			unmount(instance!);
			root!.remove();
			instance = null;
			root = null;
		}
		// canRestore false: hidden regardless.
		const r = render({ canEdit: false, peeking: true, canRestore: false });
		expect(present(r, 'archived-restore-btn')).toBe(false);
	});
});

// BUG-2263: a FIELD stays interactive across a peeking flip — the input does NOT
// unmount, and a typed value's debounce still fires onchange (→ updateField,
// which is side-independent). This mounts the REAL FieldEditor to lock it in.
describe('field stays interactive across a peeking flip (BUG-2263)', () => {
	let root: HTMLElement | null = null;
	let instance: ReturnType<typeof mount> | null = null;

	afterEach(() => {
		if (instance) unmount(instance);
		root?.remove();
		instance = null;
		root = null;
		vi.useRealTimers();
	});

	function mountProbe(onchange: (v: any) => void) {
		root = target();
		instance = mount(FieldSaveProbe, { target: root, props: { onchange } });
		flushSync();
		return root;
	}

	it('the input stays mounted after peeking begins, and the debounced value still saves', () => {
		vi.useFakeTimers();
		const onchange = vi.fn();
		const r = mountProbe(onchange);

		// Type a value — arms the 500ms debounce, does NOT fire yet.
		const input = r.querySelector<HTMLInputElement>('input.field-input')!;
		input.value = 'ui/editor';
		input.dispatchEvent(new Event('input', { bubbles: true }));
		flushSync();
		expect(onchange).not.toHaveBeenCalled();

		// Pane opens mid-edit → the field must STAY interactive (invisible freeze):
		// peeking flips true, but the input is NOT unmounted (contrast the old
		// degrade-to-readonly behavior).
		r.querySelector<HTMLButtonElement>('[data-testid="begin-peek"]')!.click();
		flushSync();
		expect(text(r, 'probe-peeking')).toBe('true');
		expect(r.querySelector('input.field-input')).not.toBeNull();

		// The debounce fires → the value reaches the parent (→ updateField).
		vi.advanceTimersByTime(500);
		expect(onchange).toHaveBeenCalledTimes(1);
		expect(onchange).toHaveBeenCalledWith('ui/editor');
	});
});
