// TASK-2235: the Insights Customize popover closed only by re-clicking its
// button. It now closes on Escape (returning focus to the button when focus
// was in it) and on a press outside it. It is a disclosure, not a modal.
import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { tick } from 'svelte';
import { page } from '$app/state';
import { acquire, __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';

const never = () => new Promise<never>(() => {});
vi.mock('$lib/api/client', () => ({
	api: {
		collections: { list: () => never() },
		report: { getLayout: () => never(), get: () => never(), saveLayout: () => never() }
	}
}));

import InsightsPage from './+page.svelte';

async function settle() {
	for (let i = 0; i < 10; i++) await Promise.resolve();
	await tick();
}

async function openCustomize() {
	render(InsightsPage);
	await settle();
	const btn = screen.getByRole('button', { name: 'Customize' });
	await fireEvent.click(btn);
	await settle();
	expect(document.querySelector('.customize-panel')).not.toBeNull();
	return btn;
}

const panelOpen = () => document.querySelector('.customize-panel') !== null;

beforeEach(() => {
	page.params.workspace = 'ws';
	page.params.username = 'alice';
});
afterEach(() => {
	cleanup();
	__resetViewerBackdropForTests();
	document.body.innerHTML = '';
});

describe('Insights Customize popover (TASK-2235)', () => {
	it('Escape closes it and returns focus to the button', async () => {
		const btn = await openCustomize();
		const box = document.querySelector<HTMLInputElement>('.customize-panel input')!;
		box.focus();
		const e = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
		box.dispatchEvent(e);
		await settle();
		expect(panelOpen()).toBe(false);
		expect(document.activeElement).toBe(btn);
		expect(e.defaultPrevented).toBe(true);
	});

	it('a press outside closes it; a press inside does not', async () => {
		await openCustomize();
		const box = document.querySelector<HTMLElement>('.customize-panel input')!;
		box.dispatchEvent(new Event('pointerdown', { bubbles: true }));
		await settle();
		expect(panelOpen()).toBe(true);

		document.body.dispatchEvent(new Event('pointerdown', { bubbles: true }));
		await settle();
		expect(panelOpen()).toBe(false);
	});

	it('leaves an Escape a viewer in front owns, and a held repeat', async () => {
		await openCustomize();
		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', repeat: true, cancelable: true }));
		await settle();
		expect(panelOpen()).toBe(true);

		const viewer = document.createElement('div');
		viewer.className = 'attachment-viewer';
		document.body.appendChild(viewer);
		const lease = acquire(viewer);
		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', cancelable: true }));
		await settle();
		expect(panelOpen()).toBe(true);
		lease.release();
	});

	it('the button still toggles it', async () => {
		const btn = await openCustomize();
		await fireEvent.click(btn);
		await settle();
		expect(panelOpen()).toBe(false);
	});
});
