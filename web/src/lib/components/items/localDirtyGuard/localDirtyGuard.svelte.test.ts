import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { editorStore } from '$lib/stores/editor.svelte';
import GuardProbe from './GuardProbe.svelte';

// PLAN-2154 Phase 0 / R4 (TASK-2156): editorStore is a module singleton.
// With a second concurrently-mounted <ItemDetail> (the future full-page-host
// + docked pane), one instance's content edit used to flip the SHARED
// `editorStore.dirty` flag that the OTHER instance's SSE archive/delete
// guards also read — a frozen/background master could self-redirect off a
// pane edit it has nothing to do with. The fix shadows `dirty` per-instance
// (`localDirty`) and reroutes the guards to read the shadow instead of the
// singleton.
//
// GuardProbe.svelte mirrors the exact write pattern (editorStore.setDirty +
// localDirty in the same handler) and the exact guard formula ItemDetail's
// SSE guards use, so this test exercises real component instances backed by
// the real `editorStore` module — not a reimplementation of the logic.
function click(root: HTMLElement, testid: string) {
	root.querySelector<HTMLButtonElement>(`[data-testid="${testid}"]`)!.click();
	flushSync();
}

function text(root: HTMLElement, testid: string): string {
	return root.querySelector(`[data-testid="${testid}"]`)!.textContent ?? '';
}

describe('per-instance localDirty shadow (PLAN-2154 Phase 0 / TASK-2156)', () => {
	let targetA: HTMLElement;
	let targetB: HTMLElement;
	let instanceA: ReturnType<typeof mount>;
	let instanceB: ReturnType<typeof mount>;

	beforeEach(() => {
		// editorStore is a module singleton — reset it so a prior test's
		// dirty write can't bleed into this one.
		editorStore.setDirty(false);
		targetA = document.body.appendChild(document.createElement('div'));
		targetB = document.body.appendChild(document.createElement('div'));
		instanceA = mount(GuardProbe, { target: targetA });
		instanceB = mount(GuardProbe, { target: targetB });
		flushSync();
	});

	afterEach(() => {
		unmount(instanceA);
		unmount(instanceB);
		targetA.remove();
		targetB.remove();
	});

	it("instance B editing does NOT trip instance A's destructive guard", () => {
		click(targetB, 'edit');

		// The shared singleton WAS mutated by B's edit — proving this isn't
		// a no-op scenario; the old (pre-fix) guard, which read this
		// singleton directly, would have tripped for EVERY instance.
		expect(editorStore.dirty).toBe(true);
		expect(text(targetA, 'guard-global')).toBe('true');

		// The per-instance shadow-driven guard on A is unaffected by B's edit.
		expect(text(targetA, 'guard')).toBe('false');
		// B's own guard, naturally, IS tripped by its own edit.
		expect(text(targetB, 'guard')).toBe('true');
	});

	it("instance A's own edit still trips its own guard (single-instance behavior preserved)", () => {
		click(targetA, 'edit');
		expect(text(targetA, 'guard')).toBe('true');
	});

	it('save clears the local shadow', () => {
		click(targetA, 'edit');
		expect(text(targetA, 'guard')).toBe('true');
		click(targetA, 'save');
		expect(text(targetA, 'guard')).toBe('false');
	});

	it("B's save does not clear A's still-dirty local shadow", () => {
		click(targetA, 'edit');
		click(targetB, 'edit');
		click(targetB, 'save');
		// The shared singleton follows whichever instance wrote it last.
		expect(editorStore.dirty).toBe(false);
		// A's own shadow is untouched by B's save.
		expect(text(targetA, 'guard')).toBe('true');
	});
});
