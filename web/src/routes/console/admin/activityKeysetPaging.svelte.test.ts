import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';

/**
 * BUG-2781, the wiring half (CONVE-19). `activityPaging.ts` is unit tested on
 * its own, and a correct helper these surfaces never called would pass all of
 * that. These drive the rendered "Load more" and assert what the SECOND
 * request asked for: the keyset cursor of the last row held, and no offset.
 *
 * Page 2 deliberately repeats the last row of page 1, which is what offset
 * paging produced when a debounce merge moved an older row to the head. Both
 * surfaces render with a keyed each, so the repeat must render once.
 */
const adminFetchMock = vi.hoisted(() => vi.fn<(path: string) => Promise<unknown>>());

vi.mock('$lib/stores/admin.svelte', () => ({
	adminFetch: (path: string) => adminFetchMock(path)
}));

const { default: AuditLogPage } = await import('./audit-log/+page.svelte');
const { default: UserActivityTab } = await import('$lib/components/admin/UserActivityTab.svelte');

function row(i: number) {
	return {
		id: `r${String(i).padStart(3, '0')}`,
		action: 'updated',
		actor: 'user',
		source: 'web',
		created_at: new Date(Date.UTC(2026, 7, 24, 12, 0, 0) - i * 1000).toISOString(),
		metadata: '{}',
		user_id: 'user-1'
	};
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

async function settle(): Promise<void> {
	flushSync();
	for (let i = 0; i < 8; i++) {
		await Promise.resolve();
		await tick();
	}
	flushSync();
}

function clickLoadMore(): void {
	const btn = [...host.querySelectorAll('button')].find((b) => /load more/i.test(b.textContent ?? ''));
	expect(btn, 'a Load more button is rendered').toBeTruthy();
	btn!.click();
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

describe('audit log paging', () => {
	it('asks for the page after the last row held, and renders a repeated row once', async () => {
		const page1 = Array.from({ length: 50 }, (_, i) => row(i));
		const page2 = [row(49), row(50)];
		const auditCalls: string[] = [];
		adminFetchMock.mockImplementation(async (path: string) => {
			if (!path.startsWith('/audit-log')) return [];
			auditCalls.push(path);
			return auditCalls.length === 1 ? page1 : page2;
		});

		app = mount(AuditLogPage, { target: host, props: {} }) as Record<string, unknown>;
		await settle();
		clickLoadMore();
		await settle();

		expect(auditCalls).toHaveLength(2);
		const q = new URL(auditCalls[1], 'http://x').searchParams;
		expect(q.get('before')).toBe(row(49).created_at);
		expect(q.get('before_id')).toBe(row(49).id);
		expect(q.has('offset')).toBe(false);
		expect(host.querySelectorAll('tbody tr')).toHaveLength(51);
	});
});

describe('admin user activity paging', () => {
	it('sends back the cursor the server returned, and renders a repeated row once', async () => {
		const calls: string[] = [];
		adminFetchMock.mockImplementation(async (path: string) => {
			calls.push(path);
			return calls.length === 1
				? { events: [row(0), row(1)], next_before: row(1).created_at, next_before_id: row(1).id }
				: { events: [row(1), row(2)], next_before: null, next_before_id: null };
		});

		app = mount(UserActivityTab, {
			target: host,
			props: { user: { id: 'u1' } as never, active: true }
		}) as Record<string, unknown>;
		await settle();
		clickLoadMore();
		await settle();

		expect(calls).toHaveLength(2);
		const q = new URL(calls[1], 'http://x').searchParams;
		expect(q.get('before')).toBe(row(1).created_at);
		expect(q.get('before_id')).toBe(row(1).id);
		expect(q.has('offset')).toBe(false);
		expect(host.querySelectorAll('li')).toHaveLength(3);
		// The last page carried no cursor, so there is nothing more to load.
		expect([...host.querySelectorAll('button')].some((b) => /load more/i.test(b.textContent ?? ''))).toBe(false);
	});
});
