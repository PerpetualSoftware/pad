import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';

/**
 * TASK-2759, codex round 12 — and a correction to my own exemption.
 *
 * The plan listed this tab as exempt because its local row type omitted
 * `metadata`. That was true and it was the wrong reason: the endpoint
 * serializes whole `models.Activity` rows, so the stamped name was on the
 * wire the whole time and the surface DOES meet the unit's discriminator
 * ("does this surface hold an Activity?"). An admin reading a user's
 * activity saw "via cli" with no way to tell which agent acted — the same
 * gap the audit log had, on the same rows.
 */
const adminFetchMock = vi.hoisted(() => vi.fn<(path: string) => Promise<any>>());

vi.mock('$lib/stores/admin.svelte', () => ({
	adminFetch: (path: string) => adminFetchMock(path)
}));

const { default: UserActivityTab } = await import('./UserActivityTab.svelte');
const { default: UserOverviewTab } = await import('./UserOverviewTab.svelte');

function event(over: Record<string, unknown> = {}) {
	return {
		id: 'e1',
		action: 'updated',
		actor: 'agent',
		source: 'cli',
		created_at: new Date('2026-08-24T12:00:00Z').toISOString(),
		metadata: JSON.stringify({ agent: 'wren' }),
		document_id: 'item-1',
		...over
	};
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

async function mountWith(events: Record<string, unknown>[]): Promise<void> {
	adminFetchMock.mockResolvedValue({ events, next_offset: null });
	app = mount(UserActivityTab, {
		target: host,
		props: { user: { id: 'u1' } as never, active: true }
	}) as Record<string, unknown>;
	flushSync();
	for (let i = 0; i < 6; i++) {
		await Promise.resolve();
		await tick();
	}
	flushSync();
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	adminFetchMock.mockReset();
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
});

describe('admin user activity — agent name', () => {
	it('names the agent behind an agent-sourced row', async () => {
		await mountWith([event()]);

		const el = host.querySelector('.activity-agent')!;
		expect(el).not.toBeNull();
		expect(el.textContent).toBe('wren');
	});

	it('renders nothing extra when the row carries no name', async () => {
		// The counterfactual for the whole surface: this is what every row
		// looked like before, and an unconditional element would show an empty
		// one here rather than falling back cleanly.
		await mountWith([event({ metadata: '{}' })]);

		expect(host.querySelector('.activity-agent')).toBeNull();
		expect(host.textContent).toContain('via cli');
	});

	it('never reads the stamp for a non-agent row', async () => {
		await mountWith([event({ actor: 'user' })]);

		expect(host.querySelector('.activity-agent')).toBeNull();
	});

	it('isolates the name so a bidi control cannot reorder the row', async () => {
		await mountWith([event({ metadata: JSON.stringify({ agent: 'wren‮gnimalb' }) })]);

		const el = host.querySelector('.activity-agent')!;
		expect(el.tagName).toBe('BDI');
		expect(el.textContent).toBe('wren‮gnimalb');
	});

	it('survives unparseable metadata without throwing', async () => {
		await mountWith([event({ metadata: 'not json' })]);

		expect(host.querySelector('.activity-agent')).toBeNull();
		expect(host.textContent).toContain('via cli');
	});
});

/**
 * The overview tab renders the same rows through its own markup and its own
 * filter (writes only), so it is a second binding, not a second view of the
 * first — CONVE-19. It reads the same endpoint, plus a metrics call.
 */
async function mountOverview(events: Record<string, unknown>[]): Promise<void> {
	adminFetchMock.mockImplementation((path: string) =>
		path.includes('/metrics')
			? Promise.resolve({})
			: Promise.resolve({ events, next_offset: null })
	);
	app = mount(UserOverviewTab, {
		target: host,
		props: { user: { id: 'u1' } as never, active: true }
	}) as Record<string, unknown>;
	flushSync();
	for (let i = 0; i < 6; i++) {
		await Promise.resolve();
		await tick();
	}
	flushSync();
}

describe('admin user overview — agent name', () => {
	it('names the agent behind a write', async () => {
		await mountOverview([event()]);

		const el = host.querySelector('.recent-agent')!;
		expect(el).not.toBeNull();
		expect(el.tagName).toBe('BDI');
		expect(el.textContent).toBe('wren');
	});

	it('renders nothing extra when the row carries no name', async () => {
		await mountOverview([event({ metadata: '{}' })]);

		expect(host.querySelector('.recent-agent')).toBeNull();
		expect(host.textContent).toContain('via cli');
	});

	it('never reads the stamp for a non-agent row', async () => {
		await mountOverview([event({ actor: 'user' })]);

		expect(host.querySelector('.recent-agent')).toBeNull();
	});
});
