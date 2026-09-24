import { describe, it, expect, vi, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';

// BUG-3195: the nested x/y count is a per-row progress count, so an abandoned
// grandchild leaves both numbers, and 0/0 shows nothing. Mounts the REAL
// component so the wiring from the abandonedStatuses prop to the rendered
// count is what is measured, not the helper alone.

const childrenMock = vi.fn(async () => [] as unknown[]);

vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			children: (...args: unknown[]) => childrenMock(...args)
		}
	}
}));

const { default: NestedChildren } = await import('./NestedChildren.svelte');

const item = (id: string, status: string) => ({
	id,
	slug: id,
	title: id,
	collection_slug: 'tasks',
	fields: JSON.stringify({ status })
});

let instance: ReturnType<typeof mount> | null = null;
let target: HTMLElement | null = null;

afterEach(() => {
	if (instance) unmount(instance);
	instance = null;
	target?.remove();
	target = null;
	childrenMock.mockReset();
});

async function render(statuses: string[], abandonedStatuses?: string[]) {
	childrenMock.mockResolvedValue(statuses.map((s, i) => item(`c${i}`, s)));
	target = document.createElement('div');
	document.body.appendChild(target);
	instance = mount(NestedChildren, {
		target,
		props: {
			wsSlug: 'ws',
			parentSlug: 'p',
			terminalStatuses: ['done', 'cancelled'],
			abandonedStatuses
		}
	});
	for (let i = 0; i < 5; i++) {
		await tick();
		await Promise.resolve();
	}
	flushSync();
	return target.querySelector('.nested-count')?.textContent ?? null;
}

describe('NestedChildren progress (BUG-3195)', () => {
	it('leaves an abandoned child out of both numbers', async () => {
		expect(await render(['done', 'cancelled', 'open'], ['cancelled'])).toBe('1/2');
	});

	it('shows no count when every child is abandoned', async () => {
		expect(await render(['cancelled'], ['cancelled'])).toBeNull();
		// The rows themselves still render: the child exists, it just is not
		// part of the progress.
		expect(target?.querySelectorAll('.nested-children').length).toBe(1);
	});

	it('control: with nothing declared abandoned the same child counts as done', async () => {
		expect(await render(['done', 'cancelled', 'open'], [])).toBe('2/3');
	});
});
