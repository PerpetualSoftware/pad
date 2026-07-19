import { describe, it, expect, afterEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import FreezeProbe from './FreezeProbe.svelte';
import FieldSaveProbe from './FieldSaveProbe.svelte';

// PLAN-2154 Phase 2 / D2 / R12 (TASK-2172) — retain-alive master freeze.
//
// HT-2176 Option A: the freeze blocks the INITIATION of NEW edits while peeking;
// a pre-pane in-flight/debounced save completes on its own (not suppressed, not
// re-flushed on un-peek). These tests therefore assert the NEW-EDIT GATES only —
// (a) every new-edit surface is disabled/gated while peeking, and byte-identical
// to the canEdit-only baseline when not. There are deliberately NO suspend /
// resume / re-flush assertions: nothing is suspended under Option A.
//
// FreezeProbe mounts the CANONICAL freeze gate expressions from ItemDetail,
// backed by the shared `computeMutationsEnabled` helper, so the gate predicate +
// wiring can't drift. The running-app assertion is TASK-2175's — no host passes
// `peeking={true}` until TASK-2174.

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

// Every mutation surface the master-freeze gates behind `mutationsEnabled`.
const MUTATION_SURFACES = [
	'delete-btn',
	'move-btn',
	'add-relationship-btn',
	'editor-mutation-ui',
	'title-editable',
];

describe('retain-alive master freeze wiring (TASK-2172)', () => {
	let root: HTMLElement | null = null;
	let instance: ReturnType<typeof mount> | null = null;

	function render(props: { canEdit?: boolean; peeking?: boolean; canRestore?: boolean }) {
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

	it('peeking=true freezes EVERY mutation surface even for an editor (canEdit=true)', () => {
		const r = render({ canEdit: true, peeking: true });

		expect(text(r, 'mutationsEnabled')).toBe('false');
		// Editor is read-only and the raw editor is read-only; the field input
		// is read-only too (no NEW field edit can be started while peeking).
		expect(text(r, 'editor-editable')).toBe('false');
		expect(text(r, 'raw-readonly')).toBe('true');
		expect(text(r, 'field-readonly')).toBe('true');
		// Child + timeline receive the frozen signal (child keeps the REAL
		// canEdit — the freeze rides the separate `frozen` prop, not canEdit).
		expect(text(r, 'child-canEdit')).toBe('true');
		expect(text(r, 'child-frozen')).toBe('true');
		expect(text(r, 'timeline-frozen')).toBe('true');
		// Every gated mutation control is unmounted.
		for (const surface of MUTATION_SURFACES) {
			expect(present(r, surface), `${surface} must be frozen`).toBe(false);
		}
		// Title falls through to the read-only heading, and archived restore hides.
		expect(present(r, 'title-readonly')).toBe(true);
		expect(present(r, 'archived-restore-btn')).toBe(false);
		// The mode toggle (provider-teardown control) is hidden — retain-alive.
		expect(present(r, 'mode-toggle')).toBe(false);
		// Star disabled; Share + the whole quick-actions menu hidden/dismissed.
		expect(disabled(r, 'star-btn')).toBe(true);
		expect(present(r, 'share-btn')).toBe(false);
		expect(present(r, 'quickactions-menu')).toBe(false);
	});

	it('peeking=false, canEdit=true keeps every mutation surface live (byte-identical baseline)', () => {
		const r = render({ canEdit: true, peeking: false });

		expect(text(r, 'mutationsEnabled')).toBe('true');
		expect(text(r, 'editor-editable')).toBe('true');
		expect(text(r, 'raw-readonly')).toBe('false');
		expect(text(r, 'field-readonly')).toBe('false');
		expect(text(r, 'child-canEdit')).toBe('true');
		expect(text(r, 'child-frozen')).toBe('false');
		expect(text(r, 'timeline-frozen')).toBe('false');
		for (const surface of MUTATION_SURFACES) {
			expect(present(r, surface), `${surface} must be live`).toBe(true);
		}
		expect(present(r, 'title-readonly')).toBe(false);
		expect(present(r, 'archived-restore-btn')).toBe(true);
		// A non-peeking master keeps its mode toggle (editor or viewer alike).
		expect(present(r, 'mode-toggle')).toBe(true);
		// Star enabled; Share + quick-actions menu live.
		expect(disabled(r, 'star-btn')).toBe(false);
		expect(present(r, 'share-btn')).toBe(true);
		expect(present(r, 'quickactions-menu')).toBe(true);
	});

	it('star stays enabled for a non-peeking VIEWER, and share/quick-actions for a non-peeking archived owner (byte-identity)', () => {
		// A viewer (canEdit=false, mutationsEnabled=false) can still star when not
		// peeking — star gates on peeking, not mutationsEnabled.
		let r = render({ canEdit: false, peeking: false, isOwner: false });
		expect(disabled(r, 'star-btn')).toBe(false);
		unmount(instance!);
		root!.remove();

		// An archived-item owner (isOwner=true, canEdit=false) keeps Share + the
		// quick-actions menu when not peeking — they gate on `!peeking`, not
		// mutationsEnabled (which would fold in the archived canEdit=false).
		r = render({ canEdit: false, peeking: false, isOwner: true });
		expect(present(r, 'share-btn')).toBe(true);
		expect(present(r, 'quickactions-menu')).toBe(true);

		// Peeking freezes both regardless.
		unmount(instance!);
		root!.remove();
		r = render({ canEdit: false, peeking: true, isOwner: true });
		expect(disabled(r, 'star-btn')).toBe(true);
		expect(present(r, 'share-btn')).toBe(false);
		expect(present(r, 'quickactions-menu')).toBe(false);
	});

	it('peeking gate is independent of canEdit — a view-only master already freezes without peeking', () => {
		// canEdit=false alone (a genuine read-only viewer) hides the mutation UI;
		// `mutationsEnabled` collapses to canEdit when not peeking, so the freeze
		// prop changes nothing for that caller.
		const r = render({ canEdit: false, peeking: false });
		expect(text(r, 'mutationsEnabled')).toBe('false');
		expect(text(r, 'editor-editable')).toBe('true'); // still a live (read-only) editor, NOT peeking
		// The mode toggle stays for a read-only viewer — it's peeking-gated, not
		// mutation-gated (the provider only needs protecting from a peek teardown).
		expect(present(r, 'mode-toggle')).toBe(true);
		for (const surface of MUTATION_SURFACES) {
			expect(present(r, surface)).toBe(false);
		}
	});

	it('archived restore rides `canRestore && !peeking`, not canEdit (archived items force canEdit false)', () => {
		// Not peeking: restore shows for a permitted user.
		let r = render({ canEdit: false, peeking: false, canRestore: true });
		expect(present(r, 'archived-restore-btn')).toBe(true);
		unmount(instance!);
		root!.remove();

		// Peeking: restore hides even though canRestore is true.
		r = render({ canEdit: false, peeking: true, canRestore: true });
		expect(present(r, 'archived-restore-btn')).toBe(false);
	});
});

// HT-2176 Option A / fix #1 (TASK-2172): a FIELD value typed BEFORE the pane
// opened must SAVE. FieldEditor debounces onchange ~500ms; if peeking begins
// before it fires, the field flips read-only (blocking any NEW edit) but the
// pending debounce still fires onchange — and `updateField` no longer rechecks
// `mutationsEnabled`, so the pre-pane value completes. This mounts the REAL
// FieldEditor to lock that behavior in.
describe('pre-pane debounced field save completes under Option A (TASK-2172)', () => {
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

	it('a value typed before peeking still fires onchange after the field goes read-only', () => {
		vi.useFakeTimers();
		const onchange = vi.fn();
		const r = mountProbe(onchange);

		// Type a value (pre-pane) — arms the 500ms debounce, does NOT fire yet.
		const input = r.querySelector<HTMLInputElement>('input.field-input')!;
		input.value = 'ui/editor';
		input.dispatchEvent(new Event('input', { bubbles: true }));
		flushSync();
		expect(onchange).not.toHaveBeenCalled();

		// Pane opens mid-edit → the field flips read-only (input unmounts, no new
		// edit possible) but the armed debounce survives.
		r.querySelector<HTMLButtonElement>('[data-testid="begin-peek"]')!.click();
		flushSync();
		expect(r.querySelector('input.field-input')).toBeNull(); // NEW edit blocked

		// The debounce fires → the pre-pane value reaches the parent (→ updateField,
		// which no longer suppresses it). The freeze did NOT drop the typed value.
		vi.advanceTimersByTime(500);
		expect(onchange).toHaveBeenCalledTimes(1);
		expect(onchange).toHaveBeenCalledWith('ui/editor');
	});
});
