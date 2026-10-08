// TASK-2202 — the toast live regions exist before any toast does, errors go to
// the assertive one, hover and focus hold a toast, and a link toast answers
// Space as well as Enter (but not keys pressed on its own buttons).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';

const nav = vi.hoisted(() => ({ goto: [] as string[] }));
vi.mock('$app/navigation', () => ({
	goto: vi.fn(async (url: string) => {
		nav.goto.push(url);
	})
}));

import ToastContainer from './ToastContainer.svelte';
import { toastStore } from '$lib/stores/toast.svelte';

const regions = (c: HTMLElement) => ({
	polite: c.querySelector('[aria-live="polite"]') as HTMLElement | null,
	assertive: c.querySelector('[aria-live="assertive"]') as HTMLElement | null
});

// jsdom has no Web Animations API, which Svelte's fly/fade transitions use.
// A stand-in that finishes at once: these tests are about announcements and
// timers, not motion.
if (!('animate' in Element.prototype)) {
	(Element.prototype as unknown as { animate: unknown }).animate = function () {
		const animation = {
			onfinish: null as null | (() => void),
			cancel() {},
			finish() {},
			currentTime: 0,
			addEventListener() {},
			finished: Promise.resolve()
		};
		queueMicrotask(() => animation.onfinish?.());
		return animation;
	};
}

beforeEach(() => {
	nav.goto.length = 0;
	toastStore.clearAll();
});
afterEach(() => {
	cleanup();
	toastStore.clearAll();
	vi.useRealTimers();
});

describe('ToastContainer announcements (TASK-2202)', () => {
	it('both live regions are mounted and empty before any toast exists', () => {
		const { container } = render(ToastContainer);
		const r = regions(container);
		expect(r.polite, 'polite region exists with no toasts').not.toBeNull();
		expect(r.assertive, 'assertive region exists with no toasts').not.toBeNull();
		expect(r.polite!.textContent?.trim()).toBe('');
		expect(r.assertive!.textContent?.trim()).toBe('');
	});

	it('an error is announced in the assertive region, a success in the polite one, each once', async () => {
		const { container } = render(ToastContainer);
		const r = regions(container);
		const politeBefore = r.polite;
		toastStore.show('Saved', 'success');
		await tick();
		toastStore.show('Could not save', 'error');
		await tick();
		expect(r.polite!.textContent).toContain('Saved');
		expect(r.polite!.textContent).not.toContain('Could not save');
		expect(r.assertive!.textContent).toContain('Could not save');
		// The SAME region element: content was added to a region that already
		// existed, which is what screen readers announce.
		expect(regions(container).polite).toBe(politeBefore);
		// The toast itself sits in its region, once: no hidden copy of the
		// text that a screen reader (or a test) would find twice.
		expect(r.assertive!.querySelector('.toast-error')).not.toBeNull();
		expect(r.polite!.querySelector('.toast-error')).toBeNull();
		expect(container.textContent!.split('Could not save').length - 1).toBe(1);
	});

	it('the same text twice in a row is a new node, so it is read again', async () => {
		const { container } = render(ToastContainer);
		toastStore.show('Saved', 'success');
		await tick();
		toastStore.show('Saved', 'success');
		await tick();
		expect(regions(container).polite!.querySelectorAll('.toast').length).toBe(2);
	});
});

describe('ToastContainer hold and keys (TASK-2202)', () => {
	it('hovering a toast holds it past its duration; leaving lets it go', async () => {
		vi.useFakeTimers();
		const { container } = render(ToastContainer);
		const id = toastStore.show('Saved', 'success', 3000);
		flushSync();
		const el = container.querySelector(`[data-toast-id="${id}"]`) as HTMLElement;
		await fireEvent.mouseEnter(el);
		vi.advanceTimersByTime(10_000);
		expect(toastStore.toasts.some((t) => t.id === id)).toBe(true);
		await fireEvent.mouseLeave(el);
		vi.advanceTimersByTime(3000);
		expect(toastStore.toasts.some((t) => t.id === id)).toBe(false);
	});

	it('focus inside a toast holds it, and moving focus between its own buttons keeps the hold', async () => {
		vi.useFakeTimers();
		const { container } = render(ToastContainer);
		const id = toastStore.show('Archived', 'success', 3000, undefined, { label: 'Undo', onAction: () => {} });
		flushSync();
		const el = container.querySelector(`[data-toast-id="${id}"]`) as HTMLElement;
		const undo = el.querySelector('.toast-action') as HTMLElement;
		const dismiss = el.querySelector('.toast-dismiss') as HTMLElement;
		await fireEvent.focusIn(undo, { relatedTarget: null });
		await fireEvent.focusOut(undo, { relatedTarget: dismiss });
		await fireEvent.focusIn(dismiss, { relatedTarget: undo });
		vi.advanceTimersByTime(10_000);
		expect(toastStore.toasts.some((t) => t.id === id), 'held while focus stays inside').toBe(true);
		await fireEvent.focusOut(dismiss, { relatedTarget: null });
		vi.advanceTimersByTime(3000);
		expect(toastStore.toasts.some((t) => t.id === id)).toBe(false);
	});

	it('a link toast navigates on Space as well as Enter', async () => {
		const { container } = render(ToastContainer);
		const id = toastStore.show('Open it', 'info', 3000, '/somewhere');
		flushSync();
		const el = container.querySelector(`[data-toast-id="${id}"]`) as HTMLElement;
		await fireEvent.keyDown(el, { key: ' ' });
		expect(nav.goto).toEqual(['/somewhere']);
	});

	it('Enter on the Dismiss button dismisses without navigating', async () => {
		const { container } = render(ToastContainer);
		const id = toastStore.show('Open it', 'info', 3000, '/somewhere');
		flushSync();
		const dismiss = container.querySelector(`[data-toast-id="${id}"] .toast-dismiss`) as HTMLElement;
		await fireEvent.keyDown(dismiss, { key: 'Enter' });
		expect(nav.goto).toEqual([]);
	});
});
