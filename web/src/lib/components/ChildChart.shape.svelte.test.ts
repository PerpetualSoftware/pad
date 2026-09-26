// BUG-3052 unit 1, the `new Date` throw class: the parent's start/end date are
// stored field values, and `new Date({"toString":0})` throws, which took the
// whole children section's chart down.
import { afterEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import ChildChart from './ChildChart.svelte';

const kid = (id: string) =>
	({ id, slug: id, title: id, collection_slug: 'tasks', fields: '{"status":"open"}',
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-02T00:00:00Z' }) as never;

let instance: ReturnType<typeof mount> | undefined;
let target: HTMLElement | undefined;
afterEach(() => {
	if (instance) unmount(instance);
	instance = undefined;
	target?.remove();
});

describe('ChildChart with a stored date String() cannot convert (BUG-3052)', () => {
	it('mounts, rather than throwing', () => {
		const hostile = JSON.parse('{"toString":0}');
		target = document.body.appendChild(document.createElement('div'));
		expect(() => {
			instance = mount(ChildChart, {
				target: target!,
				props: { children: [kid('a'), kid('b')], startDate: hostile, endDate: hostile, wsSlug: 'ws' },
			});
			flushSync();
		}).not.toThrow();
	});
});
