// A workspace-scoped 404 reaches the stop-asking-and-purge seam; an item 404
// does NOT (BUG-2983).
//
// The seam existed only for 403, and Pad answers 403 for a case that is rarer
// than the one that matters: `RequireWorkspaceAccess` resolves through
// `GetWorkspacesBySlugForUser`, so a workspace you are not a member of is a
// 404 — the same 404 a DELETED workspace gives, deliberately (a non-member
// must not be able to tell "exists but forbidden" from "does not exist").
// Folding 404 in is what makes the seam fire for the commonest case there is:
// following a link into someone else's workspace.
//
// THE NARROWING IS THE RISK, and it is what most of these legs are about. A
// 404 is usually about the ITEM — a deleted ref, a stale link, a typo — and
// purging a workspace the caller can read perfectly well would be a far worse
// bug than the storm this fixes.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { api, setAccessRevokedHandler, setIdentityProvider, type AccessRevokedScope } from './client';

function mockFetchStatus(status: number, body: unknown = { error: { code: 'not_found', message: 'x' } }) {
	vi.stubGlobal(
		'fetch',
		vi.fn(async () => ({
			status,
			ok: false,
			json: async () => body,
			headers: { get: () => null },
		})),
	);
}

function capture(): AccessRevokedScope[] {
	const seen: AccessRevokedScope[] = [];
	setAccessRevokedHandler((scope) => seen.push(scope));
	return seen;
}

afterEach(() => {
	setAccessRevokedHandler(null);
	setIdentityProvider(null);
	vi.unstubAllGlobals();
	vi.restoreAllMocks();
});

describe('a 404 on a workspace-scoped path with NO sub-resource', () => {
	it('fires the seam for the items-index bootstrap', async () => {
		const seen = capture();
		mockFetchStatus(404);
		await expect(api.items.listIndex('ghost', { includeArchived: true })).rejects.toThrow();
		expect(seen).toEqual([{ kind: 'workspace', workspace: 'ghost', reason: 'gone' }]);
	});

	it('fires the seam for the collections list', async () => {
		const seen = capture();
		mockFetchStatus(404);
		await expect(api.collections.list('ghost')).rejects.toThrow();
		expect(seen.map((s) => s.workspace)).toEqual(['ghost']);
		expect(seen[0].reason).toBe('gone');
	});
});

describe('a 404 that names a sub-resource', () => {
	it('does NOT fire the seam for a missing ITEM', async () => {
		// The leg the narrowing exists for. `parseAccessRevokedScope` — the 403
		// parser — DOES accept this path, correctly, because a 403 there means
		// the read was refused. Reusing it for 404 would purge a readable
		// workspace every time someone opened a dead link.
		const seen = capture();
		mockFetchStatus(404);
		await expect(api.items.get('readable', 'PLAN-625')).rejects.toThrow();
		expect(seen).toEqual([]);
	});

	it('does NOT fire the seam for a missing COLLECTION', async () => {
		const seen = capture();
		mockFetchStatus(404);
		await expect(api.collections.get('readable', 'plans')).rejects.toThrow();
		expect(seen).toEqual([]);
	});
});

describe('the 403 half is unchanged', () => {
	it('still fires, and says which reason it was', async () => {
		// CONTROL for the whole change: if 403 stopped firing, every leg above
		// could pass while the seam was broken for the case it was built for.
		const seen = capture();
		mockFetchStatus(403, { error: { code: 'forbidden', message: 'x' } });
		await expect(api.items.listIndex('secret', { includeArchived: true })).rejects.toThrow();
		expect(seen).toEqual([{ kind: 'workspace', workspace: 'secret', reason: 'forbidden' }]);
	});

	it('a 403 on a single ITEM still fires — it is a refused read, not a missing one', async () => {
		// The asymmetry stated as a leg, so a future edit that "unifies" the two
		// parsers has to delete this on purpose.
		const seen = capture();
		mockFetchStatus(403, { error: { code: 'forbidden', message: 'x' } });
		await expect(api.items.get('secret', 'PLAN-625')).rejects.toThrow();
		expect(seen.map((s) => s.reason)).toEqual(['forbidden']);
	});
});

describe('the allow-list (codex round 1 P2)', () => {
	// Depth alone was too broad. A workspace-level endpoint may have its OWN
	// not_found — a singleton never created, a feature-gated route — and one of
	// those answering 404 would purge a workspace the caller reads fine. The
	// seam is destructive, so endpoints opt IN.
	it('does NOT fire for a workspace-level endpoint outside the list', async () => {
		const seen = capture();
		mockFetchStatus(404);
		await expect(api.dashboard.get('ghost')).rejects.toThrow();
		expect(seen).toEqual([]);
	});

	it('CONTROL: the four listed reads still fire', async () => {
		// Without this, an empty allow-list would satisfy the leg above.
		const seen = capture();
		mockFetchStatus(404);
		await expect(api.items.listIndex('ghost', { includeArchived: true })).rejects.toThrow();
		expect(seen.map((s) => s.workspace)).toEqual(['ghost']);
	});
});

describe('the request is stamped with WHO asked (codex round 1 P1)', () => {
	// A response can outlive the identity that asked — sign out or switch users
	// mid-flight. The scope carries the identity captured BEFORE the request
	// left, so the handler can refuse to record a refusal against someone who
	// never asked (and whose identity-change clear has already run).
	it('carries the identity the provider reported at issue time', async () => {
		const seen = capture();
		let who: string | null = 'user-a';
		setIdentityProvider(() => who);
		mockFetchStatus(404);
		const inflight = api.items.listIndex('ghost', { includeArchived: true });
		// The switch happens while the request is in flight.
		who = 'user-b';
		await expect(inflight).rejects.toThrow();
		expect(seen[0].identity).toBe('user-a');
	});

	it('leaves identity undefined when no provider is registered', async () => {
		// SSR and tests. `undefined` means "unknown", which the handler treats as
		// "do not second-guess" rather than as anonymous.
		const seen = capture();
		mockFetchStatus(404);
		await expect(api.items.listIndex('ghost', { includeArchived: true })).rejects.toThrow();
		expect(seen[0].identity).toBeUndefined();
	});

	it('stamps the 403 path too', async () => {
		const seen = capture();
		setIdentityProvider(() => 'user-a');
		mockFetchStatus(403, { error: { code: 'forbidden', message: 'x' } });
		await expect(api.items.listIndex('secret', { includeArchived: true })).rejects.toThrow();
		expect(seen[0]).toMatchObject({ reason: 'forbidden', identity: 'user-a' });
	});
});
