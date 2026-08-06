// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-2430 — DockedSheet is a GLOBAL Escape + touch-gesture owner: three
// instances stay mounted by `BottomNav`, its Escape listener is on `window`,
// and it is a `role="dialog"` that is NOT registered with any escape stack.
// Before this change it closed on EVERY Escape — including one meant for a
// viewer stacked above it — and its swipe-to-dismiss could start before a
// viewer opened and still dismiss the sheet afterwards.
//
// Every case here comes in a pair: BLOCKED (a viewer lease is frontmost) and
// the EMPTY-STACK REGRESSION (no lease at all → byte-identical old behaviour).
// The regressions are the half that would hide a real bug: a guard that
// declines unconditionally passes every "does not close" assertion.
//
// HOW TO READ THE PAIRS. The BLOCKED tests are what fail if a guard is deleted
// or weakened. The EMPTY-STACK REGRESSIONS are the opposite check — they fail
// if a guard declines UNCONDITIONALLY, which is the way a "deference" change
// silently breaks the app for the 99% of the time no viewer is open. Neither
// half subsumes the other, and an empty-stack test passing with the guard
// removed is by design, not a false green. Every guard in the files under test
// was mutation-verified to kill at least one case here (one documented
// exception, flagged at the guard itself).
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick, flushSync } from 'svelte';
import DockedSheet from './DockedSheet.svelte';
import {
	acquire,
	noteEscapeConsumedByViewer,
	__resetViewerBackdropForTests,
} from '$lib/a11y/viewerBackdrop';

const bodySnippet = createRawSnippet(() => ({
	render: () => `<div><button type="button">Inside</button></div>`,
}));

function baseProps(overrides: Record<string, unknown> = {}) {
	return { open: true, onclose: vi.fn(), label: 'Menu', children: bodySnippet, ...overrides };
}

function panel(): HTMLElement {
	const el = document.querySelector('.ds-panel');
	if (!el) throw new Error('.ds-panel not found');
	return el as HTMLElement;
}

function grip(): HTMLElement {
	const el = document.querySelector('.ds-grip');
	if (!el) throw new Error('.ds-grip not found');
	return el as HTMLElement;
}

/** A body-portaled viewer root, exactly the shape `acquire` expects. */
function mountViewer(): HTMLElement {
	const root = document.createElement('div');
	root.className = 'attachment-viewer';
	root.setAttribute('role', 'dialog');
	document.body.appendChild(root);
	return root;
}

/** jsdom has no TouchEvent constructor; a plain Event with `touches` suffices. */
function touch(type: string, clientY: number): Event {
	const e = new Event(type, { bubbles: true, cancelable: true });
	Object.defineProperty(e, 'touches', { value: [{ clientX: 0, clientY }] });
	return e;
}

function escape(): KeyboardEvent {
	return new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
}

/** An auto-repeat Escape — what a HELD key fires after the first press. */
function escapeRepeat(): KeyboardEvent {
	return new KeyboardEvent('keydown', {
		key: 'Escape',
		bubbles: true,
		cancelable: true,
		repeat: true,
	});
}

afterEach(() => {
	cleanup();
	__resetViewerBackdropForTests();
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

describe('DockedSheet — Escape ownership (TASK-2430)', () => {
	it('EMPTY-STACK REGRESSION: closes on Escape when no viewer lease exists', async () => {
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ onclose }) });
		await tick();
		flushSync();

		window.dispatchEvent(escape());
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('ignores an auto-repeat Escape, so a HELD key cannot cascade past the viewer', async () => {
		// BUG-2441's per-event consumption mark cannot cover a hold: every
		// auto-repeat is a FRESH event object, and by the second one the
		// viewer's lease is already released — so an unguarded handler would
		// close the viewer on the first press and this sheet on the second,
		// from ONE physical press. Found by the final full-diff review
		// (TASK-2448); the two route guards already rejected repeats.
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ onclose }) });
		await tick();
		flushSync();

		window.dispatchEvent(escapeRepeat());
		expect(onclose).not.toHaveBeenCalled();
	});
	it('EMPTY-STACK REGRESSION: a repeat is ignored but the next REAL press still closes', async () => {
		// The other half: the guard must reject repeats WITHOUT deadening the
		// handler. A guard that declined unconditionally would pass the test
		// above and break the sheet entirely.
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ onclose }) });
		await tick();
		flushSync();

		window.dispatchEvent(escapeRepeat());
		expect(onclose).not.toHaveBeenCalled();
		window.dispatchEvent(escape());
		expect(onclose).toHaveBeenCalledTimes(1);
	});
	it('declines Escape while a viewer lease is frontmost', async () => {
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ onclose }) });
		await tick();
		flushSync();

		const lease = acquire(mountViewer());
		window.dispatchEvent(escape());
		expect(onclose).not.toHaveBeenCalled();

		// …and takes Escape back the moment the viewer goes away, which is what
		// proves the guard is lease-driven rather than a blanket refusal.
		lease.release();
		window.dispatchEvent(escape());
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('OWNER ARGUMENT: an Escape ORIGINATING inside the viewer is still declined', async () => {
		// The discriminating case for "the argument is the SURFACE ASKING TO ACT,
		// not `event.target`". The blocked test above dispatches on `window`,
		// where the target is outside the viewer too, so it would ALSO pass with
		// the wrong argument. This is the REALISTIC press: the viewer holds
		// focus, so the event's target is a viewer control — and an `e.target`
		// owner would answer "not blocked" and close the sheet underneath.
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ onclose }) });
		await tick();
		flushSync();

		const viewer = mountViewer();
		const btn = document.createElement('button');
		viewer.appendChild(btn);
		acquire(viewer);

		btn.dispatchEvent(escape());
		expect(onclose).not.toHaveBeenCalled();
	});

	it('EMPTY-STACK REGRESSION: still closes on an already-`defaultPrevented` Escape', () => {
		// Unchanged behaviour, asserted so it stays that way. An earlier revision
		// of TASK-2430 added a `defaultPrevented` bail here; it was reverted
		// because it fires with no viewer present, which this task promises not to
		// do. If that check is ever wanted, this test is the one to update — and
		// updating it should be a deliberate act, not a silent side effect.
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ onclose }) });
		flushSync();

		const e = escape();
		e.preventDefault();
		window.dispatchEvent(e);
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('declines an Escape a VIEWER already consumed, after the lease is gone', async () => {
		// TASK-2448 / BUG-2441 — the real dispatch, reproduced. The viewer's own
		// escape handler runs earlier in the same event and tears itself down
		// synchronously, so this listener genuinely sees an EMPTY lease stack:
		// the test releases the lease before dispatching, exactly as the browser
		// does. Only the event-scoped mark can save the sheet here.
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ onclose }) });
		await tick();
		flushSync();

		const lease = acquire(mountViewer());
		const e = escape();
		noteEscapeConsumedByViewer(e); // what Lightbox does before `onClose()`
		lease.release(); // what Svelte's synchronous teardown does next
		window.dispatchEvent(e);
		expect(onclose).not.toHaveBeenCalled();

		// The NEXT press is the sheet's — one Escape closes one layer, it does
		// not make the sheet permanently deaf.
		window.dispatchEvent(escape());
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('EMPTY-STACK REGRESSION: an unmarked Escape still closes it', async () => {
		// The other half of the pair above: the marker is the ONLY thing that
		// changed, so a press no viewer touched behaves exactly as before —
		// including when a viewer once existed and has since gone.
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ onclose }) });
		await tick();
		flushSync();

		acquire(mountViewer()).release();
		window.dispatchEvent(escape());
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('ignores Escape while closed (unchanged)', async () => {
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ open: false, onclose }) });
		await tick();
		flushSync();

		window.dispatchEvent(escape());
		expect(onclose).not.toHaveBeenCalled();
	});
});

describe('DockedSheet — swipe-to-dismiss (TASK-2430)', () => {
	it('EMPTY-STACK REGRESSION: a past-threshold swipe still dismisses', async () => {
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ onclose }) });
		await tick();
		flushSync();

		const g = grip();
		g.dispatchEvent(touch('touchstart', 0));
		g.dispatchEvent(touch('touchmove', 200));
		flushSync();
		expect(panel().style.transform).toBe('translateY(200px)');
		g.dispatchEvent(touch('touchend', 200));
		expect(onclose).toHaveBeenCalledTimes(1);
	});

	it('EMPTY-STACK REGRESSION: a short swipe still snaps back without closing', async () => {
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ onclose }) });
		await tick();
		flushSync();

		const g = grip();
		g.dispatchEvent(touch('touchstart', 0));
		g.dispatchEvent(touch('touchmove', 10));
		g.dispatchEvent(touch('touchend', 10));
		expect(onclose).not.toHaveBeenCalled();
	});

	it('does not START a swipe while a viewer lease is frontmost', async () => {
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ onclose }) });
		await tick();
		flushSync();

		acquire(mountViewer());
		const g = grip();
		g.dispatchEvent(touch('touchstart', 0));
		g.dispatchEvent(touch('touchmove', 200));
		flushSync();
		// Never engaged, so the panel never moved.
		expect(panel().style.transform).toBe('');
		g.dispatchEvent(touch('touchend', 200));
		expect(onclose).not.toHaveBeenCalled();
	});

	it('a swipe STARTED under a viewer does not come alive when the viewer closes', async () => {
		// The move/end gates alone would satisfy the "does not START" test above —
		// they cancel the drag on the next event either way. This is what makes
		// the START gate itself load-bearing: the touch under the viewer never
		// armed the gesture, so nothing resumes once the lease is gone.
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ onclose }) });
		await tick();
		flushSync();

		const lease = acquire(mountViewer());
		const g = grip();
		g.dispatchEvent(touch('touchstart', 0));

		lease.release();
		g.dispatchEvent(touch('touchmove', 200));
		flushSync();
		expect(panel().style.transform).toBe('');
		g.dispatchEvent(touch('touchend', 200));
		expect(onclose).not.toHaveBeenCalled();
	});

	it('STRADDLE: a swipe that began BEFORE the viewer opened is abandoned, not completed', async () => {
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ onclose }) });
		await tick();
		flushSync();

		const g = grip();
		// Gesture starts with no viewer — legitimately engages and drags.
		g.dispatchEvent(touch('touchstart', 0));
		g.dispatchEvent(touch('touchmove', 120));
		flushSync();
		expect(panel().style.transform).toBe('translateY(120px)');

		// …the viewer opens mid-gesture. Touch events keep arriving at the same
		// target, so the START gate alone would let this dismiss the sheet under
		// the viewer.
		acquire(mountViewer());
		g.dispatchEvent(touch('touchmove', 200));
		flushSync();
		expect(panel().style.transform).toBe('');

		g.dispatchEvent(touch('touchend', 200));
		expect(onclose).not.toHaveBeenCalled();
	});

	it('STRADDLE: a viewer that opens between the last move and the release still blocks the dismiss', async () => {
		const onclose = vi.fn();
		render(DockedSheet, { props: baseProps({ onclose }) });
		await tick();
		flushSync();

		const g = grip();
		g.dispatchEvent(touch('touchstart', 0));
		g.dispatchEvent(touch('touchmove', 200));
		flushSync();
		// Past the dismiss threshold and still un-blocked at this point — so the
		// ONLY thing that can stop the close is the gate on the terminal event.
		expect(panel().style.transform).toBe('translateY(200px)');

		acquire(mountViewer());
		g.dispatchEvent(touch('touchend', 200));
		expect(onclose).not.toHaveBeenCalled();
	});
});
