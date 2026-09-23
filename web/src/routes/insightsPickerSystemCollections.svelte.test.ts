import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
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

// A REACTIVE `$app/state` page for this file (the shared mock is a plain
// object, so a workspace switch would re-run nothing). The mock delegates
// through a hoisted holder to a module-level `$state`, the Lightbox tests'
// pattern.
const pageHolder = vi.hoisted(() => ({ get: (): unknown => null }));
vi.mock('$app/state', () => ({
	get page() {
		return pageHolder.get();
	},
}));
const reactivePage = $state({
	params: { workspace: 'ws', username: 'alice' } as Record<string, string>,
	url: new URL('http://localhost/'),
});
pageHolder.get = () => reactivePage;

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
	reactivePage.params.workspace = 'ws';
	reactivePage.params.username = 'alice';
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

	it('a stale collections failure from a workspace the user left does not unfilter the current one (codex round 3)', async () => {
		let rejectA!: (e: Error) => void;
		listCollections.mockImplementationOnce(() => new Promise((_, rej) => (rejectA = rej)));
		getLayout.mockResolvedValue({ default_window: 'week', default_collections: ['tasks'], hidden_cards: [] });
		app = mount(InsightsPage, { target: host, props: {} }) as Record<string, unknown>;
		await settle();
		// Switch workspace while A's collections request is still in flight.
		reactivePage.params.workspace = 'ws2';
		await settle();
		expect(listCollections.mock.calls.map((c) => c[0])).toEqual(['ws', 'ws2']);
		rejectA(new Error('A failed late'));
		await settle();
		reportGet.mockClear();
		// Any later report request for ws2 must keep its selection.
		const month = [...host.querySelectorAll<HTMLButtonElement>('button.window-btn')].find((b) => b.textContent?.trim() === 'Month');
		month!.click();
		await settle();
		expect(reportGet, 'precondition: a report was requested for ws2').toHaveBeenCalled();
		const last = reportGet.mock.calls[reportGet.mock.calls.length - 1];
		expect(last[0]).toBe('ws2');
		expect((last[1] as { collections?: string[] }).collections, "A's late failure unfiltered ws2's report").toEqual(['tasks']);
	});
});
