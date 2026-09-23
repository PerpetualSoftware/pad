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

const getLayout = vi.hoisted(() => vi.fn());
const listCollections = vi.hoisted(() => vi.fn());
const saveLayout = vi.hoisted(() => vi.fn());
const reportGet = vi.hoisted(() => vi.fn());

vi.mock('$lib/api/client', () => ({
	api: {
		collections: {
			list: (...a: unknown[]) => listCollections(...a),
		},
		report: {
			getLayout: (...a: unknown[]) => getLayout(...a),
			get: (...a: unknown[]) => reportGet(...a),
			saveLayout: (...a: unknown[]) => saveLayout(...a),
		},
	},
	PadApiError: class extends Error {},
}));

// The page debounces a layout save by 500ms (scheduleSave); wait past it.
const SAVE_WAIT_MS = 700;

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
	getLayout.mockReset().mockResolvedValue({ hidden_cards: [] });
	listCollections.mockReset().mockResolvedValue([
		coll('tasks', 'Tasks', false),
		coll('playbooks', 'Playbooks', true),
		coll('house-rules', 'House Rules', true),
		coll('conventions', 'Conventions', false),
	]);
	saveLayout.mockReset().mockResolvedValue(undefined);
	reportGet.mockReset().mockResolvedValue(null);
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

	it('a layout saved with a system collection does not filter the report by it (codex round 1)', async () => {
		getLayout.mockResolvedValue({ default_window: 'week', default_collections: ['playbooks', 'tasks'], hidden_cards: [] });
		app = mount(InsightsPage, { target: host, props: {} }) as Record<string, unknown>;
		await settle();
		const calls = reportGet.mock.calls;
		expect(calls.length, 'precondition: the report loaded').toBeGreaterThan(0);
		const last = calls[calls.length - 1][1] as { collections?: string[] };
		expect(last.collections, 'the report was filtered by a system collection the picker cannot show').toEqual(['tasks']);
		const tasks = [...host.querySelectorAll('button.chip')].find((b) => b.textContent?.trim() === 'Tasks');
		expect(tasks?.getAttribute('aria-pressed'), 'the counted selection is the one shown').toBe('true');
	});

	it('if the collection list fails, the report goes unfiltered and the saved layout is not erased (codex round 2)', async () => {
		listCollections.mockRejectedValue(new Error('boom'));
		getLayout.mockResolvedValue({ default_window: 'week', default_collections: ['playbooks', 'tasks'], hidden_cards: [] });
		app = mount(InsightsPage, { target: host, props: {} }) as Record<string, unknown>;
		await settle();
		const calls = reportGet.mock.calls;
		expect(calls.length, 'precondition: the report loaded').toBeGreaterThan(0);
		const last = calls[calls.length - 1][1] as { collections?: string[] };
		expect(last.collections, 'a selection the page cannot show still filtered the report').toBeUndefined();
		// A user change schedules a layout save; it must keep the saved choice,
		// not the unfiltered fallback the report is using.
		const month = [...host.querySelectorAll<HTMLButtonElement>('button.window-btn')].find((b) => b.textContent?.trim() === 'Month');
		expect(month, 'precondition: the window control rendered').toBeTruthy();
		month!.click();
		await new Promise((r) => setTimeout(r, SAVE_WAIT_MS));
		await settle();
		expect(saveLayout, 'precondition: the change was saved').toHaveBeenCalled();
		const saved = saveLayout.mock.calls[saveLayout.mock.calls.length - 1][1] as { default_collections: string[] };
		expect(saved.default_collections, 'a failed collections load erased the saved layout').toEqual(['playbooks', 'tasks']);
	});
});
