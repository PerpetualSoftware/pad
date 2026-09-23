import { describe, it, expect, vi, beforeEach } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// BUG-3181 — `enterWorkspaceIndex` is THE way a page brings a workspace's local
// index up: bootstrap → identity check → reconcile, the order the collection
// page's BUG-3084 fix established, now shared with the workspace activity page.
// Two halves, both required so the guard is not weaker than the per-page one it
// replaced: (1) the ORDER, behaviourally; (2) nobody else calls
// `localIndex.bootstrap` directly, by a source scan.

const { localIndexMock, authMock } = vi.hoisted(() => ({
	localIndexMock: { bootstrap: vi.fn(), reconcile: vi.fn() },
	authMock: { identityEpoch: 1 },
}));
vi.mock('./localIndex.svelte', () => ({ localIndex: localIndexMock }));
vi.mock('./auth.svelte', () => ({ authStore: authMock }));

import { enterWorkspaceIndex } from './workspaceIndexEntry';

function deferred() {
	let resolve!: () => void;
	let reject!: (e: unknown) => void;
	const promise = new Promise<void>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}

describe('enterWorkspaceIndex: the fence order (BUG-3181 / BUG-3084)', () => {
	let calls: string[];
	beforeEach(() => {
		vi.clearAllMocks();
		authMock.identityEpoch = 1;
		calls = [];
		localIndexMock.bootstrap.mockImplementation(async () => void calls.push('bootstrap'));
		localIndexMock.reconcile.mockImplementation(async () => (calls.push('reconcile'), true));
	});

	it('bootstraps, then reconciles, for the same workspace and user', async () => {
		expect(await enterWorkspaceIndex('ws', 'u1', 1)).toBe(true);
		expect(calls).toEqual(['bootstrap', 'reconcile']);
		expect(localIndexMock.bootstrap).toHaveBeenCalledWith('ws', { userId: 'u1' });
		expect(localIndexMock.reconcile).toHaveBeenCalledWith('ws');
	});

	it('does NOT reconcile when the identity changed while bootstrap was in flight', async () => {
		const d = deferred();
		localIndexMock.bootstrap.mockImplementation(() => d.promise);
		const run = enterWorkspaceIndex('ws', 'u1', 1);
		authMock.identityEpoch = 2; // sign-out / switch mid-bootstrap
		d.resolve();
		expect(await run).toBe(false);
		expect(localIndexMock.reconcile).not.toHaveBeenCalled();
	});

	it('a bootstrap REJECTION still meets the identity check (not skipped by the catch)', async () => {
		localIndexMock.bootstrap.mockImplementation(async () => {
			authMock.identityEpoch = 2;
			throw new Error('boom');
		});
		expect(await enterWorkspaceIndex('ws', 'u1', 1)).toBe(false);
		expect(localIndexMock.reconcile).not.toHaveBeenCalled();
	});

	it('a bootstrap rejection with the identity held still catches up', async () => {
		localIndexMock.bootstrap.mockRejectedValue(new Error('boom'));
		expect(await enterWorkspaceIndex('ws', 'u1', 1)).toBe(true);
		expect(localIndexMock.reconcile).toHaveBeenCalledTimes(1);
	});

	it('an identity change during reconcile is not a clean catch-up', async () => {
		localIndexMock.reconcile.mockImplementation(async () => {
			authMock.identityEpoch = 2;
			return true;
		});
		expect(await enterWorkspaceIndex('ws', 'u1', 1)).toBe(false);
	});

	it('a failing or capped reconcile reports false', async () => {
		localIndexMock.reconcile.mockRejectedValueOnce(new Error('403'));
		expect(await enterWorkspaceIndex('ws', 'u1', 1)).toBe(false);
		localIndexMock.reconcile.mockResolvedValueOnce(false);
		expect(await enterWorkspaceIndex('ws', 'u1', 1)).toBe(false);
	});
});

// ── Sole caller ─────────────────────────────────────────────────────────────
const SRC = resolve(fileURLToPath(new URL('.', import.meta.url)), '..', '..');
const HELPER = 'lib/stores/workspaceIndexEntry.ts';
/**
 * Direct callers that are NOT this helper, each with its reason and an EXACT
 * count, so a second call in the same file fails too.
 */
const ALLOWED: Record<string, { count: number; why: string }> = {
	'lib/components/items/ItemDetail.svelte': {
		count: 1,
		why:
			'BUG-1461: the item page awaits the index INSIDE its item load (a Promise.all with the ' +
			'item GET), before committing the item, so wiki-links resolve at Y.Doc seed time. A ' +
			'different contract — no reconcile, its own load-generation fence — not a copy of this one.',
	},
};
/** Any spelling of a direct call: `localIndex.bootstrap(`, `?.`, or `['bootstrap']`. */
const DIRECT_CALL = /localIndex\s*(?:\?\.|\.)\s*bootstrap\s*\(|localIndex\s*\[\s*['"`]bootstrap['"`]\s*\]/g;

function sourceFiles(dir: string): string[] {
	const out: string[] = [];
	for (const name of readdirSync(dir)) {
		const p = join(dir, name);
		if (statSync(p).isDirectory()) out.push(...sourceFiles(p));
		else if (/\.(ts|js|svelte)$/.test(name) && !/\.test\.ts$/.test(name)) out.push(p);
	}
	return out;
}

export function directBootstrapCallers(root: string): Record<string, number> {
	const found: Record<string, number> = {};
	for (const file of sourceFiles(root)) {
		const n = (readFileSync(file, 'utf8').match(DIRECT_CALL) ?? []).length;
		if (n > 0) found[relative(root, file).split('\\').join('/')] = n;
	}
	return found;
}

describe('nothing but the helper calls localIndex.bootstrap directly (BUG-3181)', () => {
	it('the scan sees the helper itself (not blind)', () => {
		expect(directBootstrapCallers(SRC)[HELPER], 'the helper must be found by the scan').toBe(1);
	});

	it('every other direct call is a named, counted exception', () => {
		const found = directBootstrapCallers(SRC);
		delete found[HELPER];
		const expected = Object.fromEntries(Object.entries(ALLOWED).map(([f, v]) => [f, v.count]));
		expect(
			found,
			'a page is calling localIndex.bootstrap directly: use enterWorkspaceIndex (bootstrap → ' +
				'identity check → reconcile, the BUG-3084 order), or, for a genuinely different ' +
				'contract, add a named entry to ALLOWED with its reason and exact count',
		).toEqual(expected);
	});
});
