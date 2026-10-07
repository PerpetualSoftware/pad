import { describe, it, expect, vi } from 'vitest';
import { ImportTransportError } from '$lib/api/importUpload';
import type { ImportOutcome } from '$lib/types';
import {
	IMPORT_RESOLVE_INTERVAL_MS,
	IMPORT_RESOLVE_WINDOW_MS,
	describeImportOutcome,
	newImportKey,
	resolveImportOutcome
} from './importOutcome';

function clock() {
	let t = 0;
	return { now: () => t, sleep: async (ms: number) => void (t += ms) };
}

describe('resolveImportOutcome', () => {
	it('polls through "running" and returns the first settled answer', async () => {
		const c = clock();
		const answers: ImportOutcome[] = [{ state: 'running' }, { state: 'running' }, { state: 'removed' }];
		const status = vi.fn(async () => answers.shift() ?? null);
		expect(await resolveImportOutcome('k', { status, live: () => true, ...c })).toEqual({ state: 'removed' });
		expect(status).toHaveBeenCalledTimes(3);
	});

	it('keeps asking through errors and an unknown key until the window closes, then returns what it last saw', async () => {
		const c = clock();
		let n = 0;
		const status = vi.fn(async () => {
			if (n++ % 2 === 0) throw new Error('offline');
			return null;
		});
		expect(await resolveImportOutcome('k', { status, live: () => true, ...c })).toBeNull();
		expect(c.now()).toBeGreaterThanOrEqual(IMPORT_RESOLVE_WINDOW_MS);
		expect(status.mock.calls.length).toBe(Math.floor(IMPORT_RESOLVE_WINDOW_MS / IMPORT_RESOLVE_INTERVAL_MS) + 1);
	});

	it('stops when nobody is waiting any more', async () => {
		const c = clock();
		let live = true;
		const status = vi.fn(async () => {
			live = false;
			return { state: 'running' } as ImportOutcome;
		});
		await resolveImportOutcome('k', { status, live: () => live, ...c });
		expect(status).toHaveBeenCalledOnce();
	});
});

describe('describeImportOutcome', () => {
	const stalled = new ImportTransportError('stalled');
	const ws = { workspace_slug: 's', workspace_name: 'Name', owner_username: 'alice' };

	it('names a finished or kept workspace with a way in', () => {
		expect(describeImportOutcome(stalled, { state: 'complete', ...ws })).toMatchObject({ kind: 'complete', slug: 's', owner: 'alice' });
		const kept = describeImportOutcome(stalled, { state: 'kept', ...ws });
		expect(kept).toMatchObject({ kind: 'kept', slug: 's' });
		expect(kept.text).toContain('partial workspace "Name" was kept');
	});

	it('removed and not_created are "nothing was kept"', () => {
		for (const state of ['removed', 'not_created'] as const) {
			const v = describeImportOutcome(stalled, { state });
			expect(v.kind).toBe('nothing');
			expect(v.text).toBe('The upload stopped making progress. Nothing was kept, so it is safe to try again.');
		}
	});

	it('an unknown key, an unknown state, or one still running is UNKNOWN, never "nothing was kept"', () => {
		const cases: (ImportOutcome | null)[] = [null, { state: 'unknown' }, { state: 'running' }, { state: 'complete' }];
		for (const o of cases) {
			const v = describeImportOutcome(stalled, o);
			expect(v.kind).toBe('unknown');
			expect(v.text).toContain('check your workspace list');
			expect(v.text).not.toContain('Nothing was kept');
		}
	});
});

describe('newImportKey', () => {
	it('is 32 hex characters, inside the server pattern, and fresh each time', () => {
		const a = newImportKey();
		expect(a).toMatch(/^[A-Za-z0-9-]{8,64}$/);
		expect(a).toMatch(/^[0-9a-f]{32}$/);
		expect(newImportKey()).not.toBe(a);
	});
});
