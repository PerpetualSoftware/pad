import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import { page } from '$app/state';
import type { Activity } from '$lib/types';

/**
 * TASK-2759, the activity page's two views.
 *
 * `getSourceLabel` returned a hardcoded 'agent' for every agent row, so the
 * Audit view could not say which agent wrote. The Live view had the name but
 * filtered a hardcoded set of client ids out of it (the retired
 * GENERIC_AGENT_IDS shim).
 *
 * Both views are asserted through the PAGE (CONVE-19): the fold and the
 * helper have their own tests, and either could be correct while this page
 * passed the wrong arguments — which is the defect the Audit half actually
 * was, since the metadata was parsed three lines above the call that ignored
 * it.
 */
// `api.activity.list` resolves to a bare Activity[] — the page reads
// `result.length` and assigns it straight to state, so an envelope-shaped
// mock makes the page render empty instead of failing loudly.
const listActivity = vi.hoisted(() =>
	vi.fn<(slug: string, params: unknown) => Promise<Activity[]>>()
);

vi.mock('$lib/api/client', () => ({
	api: {
		collections: { list: vi.fn().mockResolvedValue([]) },
		activity: { list: (slug: string, params: unknown) => listActivity(slug, params) },
		comments: { list: vi.fn().mockResolvedValue([]) }
	},
	PadApiError: class extends Error {}
}));

const { default: ActivityPage } = await import('./[username]/[workspace]/activity/+page.svelte');

let seq = 0;
function act(over: Partial<Activity> = {}): Activity {
	seq += 1;
	return {
		id: `a${seq}`,
		workspace_id: 'ws',
		action: 'updated',
		actor: 'agent',
		source: 'cli',
		metadata: JSON.stringify({ agent: 'wren' }),
		created_at: new Date().toISOString(),
		document_id: 'item-1',
		item_ref: 'TASK-1',
		item_title: 'A thing',
		item_slug: 'a-thing',
		collection_slug: 'tasks',
		...over
	} as Activity;
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

/** Mount the page in `view`, with `rows` as the feed. The view is read from
 *  localStorage in onMount (it cannot be a prop — SSR has no localStorage,
 *  so the component always starts on 'live' and restores after hydration). */
async function mountPage(view: 'live' | 'audit', rows: Activity[]): Promise<void> {
	localStorage.setItem('pad-activity-view', view);
	listActivity.mockResolvedValue(rows);
	app = mount(ActivityPage, { target: host, props: {} }) as Record<string, unknown>;
	flushSync();
	for (let i = 0; i < 4; i++) {
		await Promise.resolve();
		await tick();
	}
	flushSync();
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	// The page derives its workspace from the route; the $app/state mock ships
	// empty params, and an empty slug short-circuits the load effect entirely
	// (every assertion here would then pass or fail on an empty page).
	page.params.workspace = 'ws';
	page.params.username = 'alice';
	localStorage.clear();
	listActivity.mockReset();
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
	localStorage.clear();
});

describe('activity page — Audit view', () => {
	it('badges an agent row with the stamped name', async () => {
		await mountPage('audit', [act()]);

		const badge = host.querySelector('.actor-badge.agent');
		expect(badge).not.toBeNull();
		expect(badge!.textContent!.trim()).toBe('wren');
	});

	it('badges a generic-looking client id verbatim', async () => {
		await mountPage('audit', [act({ metadata: JSON.stringify({ agent: 'claude-code' }) })]);

		expect(host.querySelector('.actor-badge.agent')!.textContent!.trim()).toBe('claude-code');
	});

	it.each([
		['no agent key', JSON.stringify({ changes: 'status' })],
		['an empty name', JSON.stringify({ agent: '' })],
		['unparseable metadata', 'not json'],
		// A row can reach the client with no metadata at all — the audit
		// helpers that log workspace-membership and auth events never call
		// agentMeta, so they produce actor=agent with nothing stamped.
		['absent metadata', undefined as unknown as string]
	])('falls back to the generic badge given %s', async (_case, metadata) => {
		await mountPage('audit', [act({ metadata })]);

		expect(host.querySelector('.actor-badge.agent')!.textContent!.trim()).toBe('agent');
	});

	// Codex round 1. The badge's base rule uppercases, so `Wren` and `wren`
	// rendered identically — the verbatim contract broken in CSS rather than
	// in code, and invisible to every textContent assertion above.
	//
	// BOUNDARY: this asserts the class the markup applies, not the pixels.
	// Svelte component styles are not injected under this vitest setup
	// (`document.querySelectorAll('style').length` is 0, so getComputedStyle
	// resolves nothing), which leaves the one adjacent CSS rule —
	// `.actor-badge.named { text-transform: none }` — outside what this suite
	// can observe. The class is the half a refactor would actually drop.
	it('marks a stamped name as a name so the badge stops upper-casing it', async () => {
		await mountPage('audit', [act({ metadata: JSON.stringify({ agent: 'Wren' }) })]);

		const badge = host.querySelector('.actor-badge.agent')!;
		expect(badge.textContent!.trim()).toBe('Wren');
		expect(badge.classList.contains('named')).toBe(true);
	});

	it('leaves the generic badge upper-cased as a category word', async () => {
		await mountPage('audit', [act({ metadata: '{}' })]);

		expect(host.querySelector('.actor-badge.agent')!.classList.contains('named')).toBe(false);
	});

	// Codex round 2 — the same escaping claim at a second surface, because the
	// two views build their labels through different code paths and a future
	// `{@html}` would land in one of them, not both. See the twin case in
	// timelineActivityAgentName for why this passes today.
	it('renders a name containing markup as text, never as elements', async () => {
		const payload = '<img src=x onerror="alert(1)">';
		await mountPage('audit', [act({ metadata: JSON.stringify({ agent: payload }) })]);

		expect(host.querySelector('img')).toBeNull();
		expect(host.querySelector('.actor-badge.agent')!.textContent!.trim()).toBe(payload);
	});

	it('never reads the stamp for a human row', async () => {
		// The badge for a human is keyed on actor_name/source, and an `agent`
		// key can ride along on any row's shared metadata blob.
		await mountPage('audit', [
			act({ actor: 'user', actor_name: 'Dave', source: 'web' })
		]);

		expect(host.querySelector('.actor-badge.agent')).toBeNull();
		expect(host.textContent).toContain('Dave');
		expect(host.textContent).not.toContain('wren');
	});
});

describe('activity page — Live view', () => {
	it('labels an episode with the stamped name', async () => {
		await mountPage('live', [act()]);

		expect(host.querySelector('.ep-actor')!.textContent!.trim()).toBe('wren');
	});

	it('labels a generic-looking client id verbatim', async () => {
		// The counterfactual for the retired shim: it turned exactly this
		// value back into 'agent', collapsing every such client into one card.
		await mountPage('live', [act({ metadata: JSON.stringify({ agent: 'claude-code' }) })]);

		expect(host.querySelector('.ep-actor')!.textContent!.trim()).toBe('claude-code');
	});
});
