import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import { page } from '$app/state';
import type { Collection } from '$lib/types';

/**
 * BUG-2410: the Insights collection picker offers what the default report
 * covers, which leaves out SYSTEM collections (Conventions, Playbooks) —
 * keyed on `is_system`, so an ordinary collection that happens to be named
 * "Conventions" is still offered.
 */
function coll(slug: string, name: string, is_system: boolean): Collection {
	return { id: `id-${slug}`, slug, name, icon: '', is_system } as unknown as Collection;
}

vi.mock('$lib/api/client', () => ({
	api: {
		collections: {
			list: vi.fn().mockResolvedValue([
				coll('tasks', 'Tasks', false),
				coll('playbooks', 'Playbooks', true),
				coll('house-rules', 'House Rules', true),
				coll('conventions', 'Conventions', false),
			]),
		},
		report: {
			getLayout: vi.fn().mockResolvedValue({}),
			get: vi.fn().mockResolvedValue(null),
			saveLayout: vi.fn().mockResolvedValue(undefined),
		},
	},
	PadApiError: class extends Error {},
}));

const { default: InsightsPage } = await import('./[username]/[workspace]/insights/+page.svelte');

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

async function settle() {
	flushSync();
	for (let i = 0; i < 8; i++) {
		await Promise.resolve();
		await tick();
	}
	flushSync();
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	page.params.workspace = 'ws';
	page.params.username = 'alice';
	localStorage.clear();
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
});

describe('Insights collection picker (BUG-2410)', () => {
	it('offers ordinary collections and leaves out system ones, by the flag not the name', async () => {
		app = mount(InsightsPage, { target: host, props: {} }) as Record<string, unknown>;
		await settle();
		const chips = [...host.querySelectorAll('button.chip')].map((b) => b.textContent?.trim());
		expect(chips, 'precondition: the picker rendered').toContain('Tasks');
		expect(chips).not.toContain('Playbooks');
		expect(chips).not.toContain('House Rules');
		expect(chips, 'an ordinary collection NAMED Conventions is still offered').toContain('Conventions');
	});
});
