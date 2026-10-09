// TASK-2235: the notification panel is modal (its backdrop covers the page)
// but used to take no focus, ignore Escape and let Tab walk into the page.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';
import NotificationPanel from './NotificationPanel.svelte';
import { toastStore } from '$lib/stores/toast.svelte';
import { acquire, __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';

// jsdom has no Web Animations API, which Svelte's fly/fade outros use. A
// stand-in that finishes at once (the ToastContainer test's): these tests are
// about focus, not motion.
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

function panel(): HTMLElement {
	const el = document.querySelector('.panel');
	if (!el) throw new Error('.panel not found');
	return el as HTMLElement;
}

async function settle() {
	await tick();
	flushSync();
}

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
	__resetViewerBackdropForTests();
	toastStore.clearHistory();
	document.body.innerHTML = '';
});

describe('NotificationPanel focus and keys (TASK-2235)', () => {
	it('is a labelled modal dialog and takes focus on open', async () => {
		render(NotificationPanel, { props: { visible: true, onclose: vi.fn() } });
		await settle();
		const p = panel();
		expect(p.getAttribute('role')).toBe('dialog');
		expect(p.getAttribute('aria-modal')).toBe('true');
		const heading = document.getElementById(p.getAttribute('aria-labelledby')!);
		expect(heading?.textContent).toBe('Notifications');
		expect(document.activeElement).toBe(p);
	});

	it('closes on Escape, but not on a held repeat or behind a viewer', async () => {
		const onclose = vi.fn();
		render(NotificationPanel, { props: { visible: true, onclose } });
		await settle();

		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', repeat: true, cancelable: true }));
		expect(onclose).not.toHaveBeenCalled();

		const viewer = document.createElement('div');
		viewer.className = 'attachment-viewer';
		document.body.appendChild(viewer);
		const lease = acquire(viewer);
		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', cancelable: true }));
		expect(onclose).not.toHaveBeenCalled();
		lease.release();

		const evt = new KeyboardEvent('keydown', { key: 'Escape', cancelable: true });
		window.dispatchEvent(evt);
		expect(onclose).toHaveBeenCalledTimes(1);
		expect(evt.defaultPrevented).toBe(true);
	});

	it('ignores Escape while closed', async () => {
		const onclose = vi.fn();
		render(NotificationPanel, { props: { visible: false, onclose } });
		await settle();
		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', cancelable: true }));
		expect(onclose).not.toHaveBeenCalled();
	});

	it('keeps Tab inside: off its last control, Tab wraps to its first', async () => {
		vi.spyOn(HTMLElement.prototype, 'getClientRects').mockReturnValue([
			{ width: 1, height: 1 } as DOMRect
		] as unknown as DOMRectList);
		toastStore.show('One', 'info');
		render(NotificationPanel, { props: { visible: true, onclose: vi.fn() } });
		await settle();

		const controls = Array.from(panel().querySelectorAll<HTMLElement>('button, a[href]'));
		const first = controls[0];
		const last = controls[controls.length - 1];
		expect(first).not.toBe(last);
		last.focus();
		const evt = new KeyboardEvent('keydown', { key: 'Tab', cancelable: true });
		window.dispatchEvent(evt);
		expect(document.activeElement).toBe(first);
		expect(evt.defaultPrevented).toBe(true);
	});

	it('returns focus to the trigger when it closes', async () => {
		const trigger = document.createElement('button');
		document.body.appendChild(trigger);
		trigger.focus();
		const view = render(NotificationPanel, { props: { visible: true, onclose: vi.fn() } });
		await settle();
		expect(document.activeElement).toBe(panel());

		await view.rerender({ visible: false, onclose: vi.fn() });
		await settle();
		expect(document.activeElement).toBe(trigger);
	});
});
