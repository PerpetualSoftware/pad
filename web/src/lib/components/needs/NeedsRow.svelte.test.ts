import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';
import NeedsRow from './NeedsRow.svelte';
import type { NeedsRow as Row } from '$lib/types';

// TASK-4904: on desktop home a Needs title opens the item in the docked pane.
const row = (path = '/dave/northwind/tasks/fix-the-thing'): Row => ({
	workspace: { slug: 'northwind', name: 'Northwind', owner_username: 'dave' },
	item: { ref: 'TASK-7', title: 'Fix the thing', collection: 'Tasks', collection_slug: 'tasks', created_at: '2026-11-01T10:00:00Z', updated_at: '2026-11-01T10:00:00Z', path, seq: 3 },
	why: { rule: 'asked', text: 'Asked you' },
	because: [],
	waiting_on: { kind: 'person', rule: 'asked' },
	waiting_since: '2026-11-12T10:00:00Z'
});
const mount = (props: Record<string, unknown> = {}, r: Row = row()) =>
	render(NeedsRow, { row: r, view: 'active', ondismiss: vi.fn(), ondefer: vi.fn(), onrestore: vi.fn(), ...props });
function trapNavigation() {
	const seen: boolean[] = [];
	const trap = (ev: Event) => { seen.push(ev.defaultPrevented); ev.preventDefault(); };
	document.addEventListener('click', trap);
	onTestFinished(() => document.removeEventListener('click', trap));
	return seen;
}
const click = (el: Element, init: MouseEventInit = {}) =>
	el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, ...init }));
afterEach(cleanup);

describe('NeedsRow pane open', () => {
	it('a plain click on the title calls onopenitem with owner, workspace, collection slug and ref, and is default-prevented', () => {
		const seen = trapNavigation();
		const onopenitem = vi.fn();
		mount({ onopenitem });
		const link = screen.getByRole('link', { name: 'Fix the thing' });
		expect(link).toHaveAttribute('href', '/dave/northwind/tasks/fix-the-thing');
		expect(link).toHaveAttribute('data-pane-key', 'dave/northwind/TASK-7');
		click(link);
		expect(seen).toEqual([true]);
		expect(onopenitem.mock.calls).toEqual([[{ owner: 'dave', workspace: 'northwind', collection: 'tasks', ref: 'TASK-7' }]]);
	});

	it('a modified or middle click is left to the browser', () => {
		const seen = trapNavigation();
		const onopenitem = vi.fn();
		mount({ onopenitem });
		const link = screen.getByRole('link', { name: 'Fix the thing' });
		for (const init of [{ metaKey: true }, { ctrlKey: true }, { shiftKey: true }, { altKey: true }, { button: 1 }]) click(link, init);
		expect(seen).toEqual([false, false, false, false, false]);
		expect(onopenitem).not.toHaveBeenCalled();
	});

	it('no interception when compact or without onopenitem', () => {
		const seen = trapNavigation();
		const onopenitem = vi.fn();
		mount({ onopenitem, compact: true });
		click(screen.getByRole('link', { name: 'Fix the thing' }));
		cleanup();
		mount();
		click(screen.getByRole('link', { name: 'Fix the thing' }));
		expect(seen).toEqual([false, false]);
		expect(onopenitem).not.toHaveBeenCalled();
	});

	it('an empty path renders no link and no pane key', () => {
		const onopenitem = vi.fn();
		const { container } = mount({ onopenitem }, row(''));
		expect(screen.queryByRole('link')).toBeNull();
		expect(container.querySelector('[data-pane-key]')).toBeNull();
		expect(screen.getByText('Fix the thing')).toBeTruthy();
	});
});
