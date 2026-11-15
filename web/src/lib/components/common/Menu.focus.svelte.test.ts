import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import Menu from './Menu.svelte';

vi.mock('$lib/stores/breakpoint.svelte', () => ({ viewport: { isMobile: false } }));

afterEach(() => {
	cleanup();
	document.body.innerHTML = '';
});

const item = (disabled: boolean) => createRawSnippet(() => ({
	render: () => `<button role="menuitem" ${disabled ? 'disabled' : ''}>${disabled ? 'Checking…' : 'Undo Done'}</button>`,
}));

async function setup(mode: 'anchored' | 'portal', disabled: boolean) {
	const trigger = document.createElement('button');
	document.body.append(trigger);
	trigger.focus();
	const onclose = vi.fn();
	const view = render(Menu, { props: { open: false, onclose, trigger, mode, children: item(disabled) } });
	await view.rerender({ open: true });
	return { ...view, trigger, onclose, panel: screen.getByRole('menu') };
}

describe.each(['anchored', 'portal'] as const)('Menu desktop focus (%s)', mode => {
	it('focuses the panel when its only item is disabled; Escape closes and restores trigger focus', async () => {
		const { panel, trigger, onclose } = await setup(mode, true);
		await waitFor(() => expect(panel).toHaveFocus());
		expect(panel).toHaveAttribute('tabindex', '-1');
		for (const key of ['ArrowDown', 'ArrowUp', 'Home', 'End']) {
			await fireEvent.keyDown(document.activeElement!, { key });
			expect(panel).toHaveFocus();
		}
		expect(onclose).not.toHaveBeenCalled();
		await fireEvent.keyDown(document.activeElement!, { key: 'Escape' });
		expect(onclose).toHaveBeenCalledTimes(1);
		expect(trigger).toHaveFocus();
	});

	it('focuses the first enabled item; Escape closes and restores trigger focus', async () => {
		const { trigger, onclose } = await setup(mode, false);
		await waitFor(() => expect(screen.getByRole('menuitem')).toHaveFocus());
		await fireEvent.keyDown(document.activeElement!, { key: 'Escape' });
		expect(onclose).toHaveBeenCalledTimes(1);
		expect(trigger).toHaveFocus();
	});

	it('follows focusKey swaps to an all-disabled panel and back to an enabled item', async () => {
		const { panel, rerender } = await setup(mode, false);
		await waitFor(() => expect(screen.getByRole('menuitem')).toHaveFocus());
		await rerender({ focusKey: 'checking', children: item(true) });
		await waitFor(() => expect(panel).toHaveFocus());
		await rerender({ focusKey: 'undo', children: item(false) });
		await waitFor(() => expect(screen.getByRole('menuitem', { name: 'Undo Done' })).toHaveFocus());
	});
});
