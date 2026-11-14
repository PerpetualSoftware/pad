import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';

/**
 * BUG-2789, the admin audit log's disposition: a forensic surface bounds the
 * ROW presentation, never the content. The collapsed cell is a clipped digest
 * (the generic fallback names only three members); expanding it must show
 * everything the row holds, including what the activity feeds suppress.
 */
const adminFetchMock = vi.hoisted(() => vi.fn<(path: string) => Promise<unknown>>());

vi.mock('$lib/stores/admin.svelte', () => ({
	adminFetch: (path: string) => adminFetchMock(path)
}));

const { default: AuditLogPage } = await import('./audit-log/+page.svelte');

const legacyChanges =
	'status: open → done; implementation_notes: [] → [map[id:note-1 summary:a very long legacy blob]]';
const metadata = JSON.stringify({ a: 1, b: 2, c: 3, changes: legacyChanges });

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

async function mountWith(rows: unknown[]): Promise<void> {
	adminFetchMock.mockResolvedValue(rows);
	app = mount(AuditLogPage, { target: host, props: {} }) as Record<string, unknown>;
	flushSync();
	await Promise.resolve();
	await Promise.resolve();
	await tick();
	flushSync();
}

function detailsCell(): HTMLElement {
	return host.querySelectorAll('tbody tr td')[3] as HTMLElement;
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
	adminFetchMock.mockReset();
});

describe('audit log Details cell (BUG-2789)', () => {
	it('is a digest until expanded, then shows the whole record', async () => {
		await mountWith([
			{ id: 'r1', action: 'updated', actor: 'user', source: 'web', created_at: new Date().toISOString(), metadata }
		]);
		const cell = detailsCell();
		// Collapsed: the digest names three members, so `changes` is not in it.
		expect(cell.querySelector('pre')).toBeNull();
		expect(cell.textContent).not.toContain('implementation_notes');

		const toggle = cell.querySelector('button') as HTMLButtonElement;
		expect(toggle.getAttribute('aria-expanded')).toBe('false');
		toggle.click();
		flushSync();

		const full = cell.querySelector('pre')?.textContent ?? '';
		expect(toggle.getAttribute('aria-expanded')).toBe('true');
		// Everything the row holds, the suppressed-elsewhere legacy blob included.
		expect(full).toContain('"changes"');
		expect(full).toContain('implementation_notes');
		expect(JSON.parse(full)).toEqual(JSON.parse(metadata));

		toggle.click();
		flushSync();
		expect(cell.querySelector('pre')).toBeNull();
	});

	it('shows an unparseable record verbatim', async () => {
		await mountWith([
			{ id: 'r2', action: 'updated', actor: 'user', source: 'web', created_at: new Date().toISOString(), metadata: 'not json {' }
		]);
		(detailsCell().querySelector('button') as HTMLButtonElement).click();
		flushSync();
		expect(detailsCell().querySelector('pre')?.textContent).toBe('not json {');
	});

	it('has nothing to expand without metadata', async () => {
		await mountWith([
			{ id: 'r3', action: 'updated', actor: 'user', source: 'web', created_at: new Date().toISOString() }
		]);
		expect(detailsCell().querySelector('button')).toBeNull();
	});
});
