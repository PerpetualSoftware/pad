import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Workspace } from '$lib/types';

/**
 * TASK-2200 — the shell brick, and specifically the half nothing retried.
 *
 * A cold load while the server is unreachable leaves `workspaces` empty and
 * `current` null. Both are acquired exactly once — `loadAll` from the root
 * layout's per-auth-resolution attempt, `setCurrent` from an effect keyed on a
 * workspace slug that does not change — and every other caller of either is a
 * user action. So when the server came back, the board could recover while the
 * sidebar stayed empty: its links are built from `current`, and only F5 fixed
 * it.
 *
 * `recoverIfMissing` is the retry, and these legs pin the two things that make
 * it one: that it ACTS when identity is missing, and that it does NOTHING when
 * identity is intact. The second matters as much as the first — a recovery that
 * fired unconditionally would re-list the workspaces on every sync result of
 * every healthy session, which is a worse defect than the one being fixed and
 * would pass any test that only checked the first.
 *
 * Fresh module per test: the store holds module-level rune state with no reset.
 */

function ws(slug: string, name = slug): Workspace {
	return { id: `id-${slug}`, slug, name, owner_username: 'dave' } as unknown as Workspace;
}

async function load() {
	vi.resetModules();
	const { api } = await import('$lib/api/client');
	const { workspaceStore } = await import('./workspace.svelte');
	return { api, workspaceStore };
}

afterEach(() => {
	vi.restoreAllMocks();
});

describe('TASK-2200 — workspace identity recovers after a failed cold load', () => {
	it('re-acquires the list AND the current workspace when both are missing', async () => {
		const { api, workspaceStore } = await load();

		// The outage: the cold load's list attempt rejected, so the store is
		// empty and `current` was never resolved.
		const list = vi.spyOn(api.workspaces, 'list').mockRejectedValueOnce(new Error('down'));
		await workspaceStore.loadAll().catch(() => undefined);
		expect(workspaceStore.workspaces).toEqual([]);
		expect(workspaceStore.current).toBeNull();

		// The server returns.
		list.mockResolvedValue([ws('alpha')]);
		vi.spyOn(api.workspaces, 'me').mockResolvedValue({} as never);

		await workspaceStore.recoverIfMissing('alpha');

		expect(workspaceStore.workspaces.map((w) => w.slug)).toEqual(['alpha']);
		expect(workspaceStore.current?.slug).toBe('alpha');
	});

	it('CONTROL — does nothing, and issues no request, when identity is intact', async () => {
		const { api, workspaceStore } = await load();

		vi.spyOn(api.workspaces, 'list').mockResolvedValue([ws('alpha')]);
		vi.spyOn(api.workspaces, 'me').mockResolvedValue({} as never);
		await workspaceStore.loadAll();
		await workspaceStore.setCurrent('alpha');

		const list = vi.spyOn(api.workspaces, 'list');
		const get = vi.spyOn(api.workspaces, 'get');
		list.mockClear();
		get.mockClear();

		await workspaceStore.recoverIfMissing('alpha');

		expect(list).not.toHaveBeenCalled();
		expect(get).not.toHaveBeenCalled();
	});

	it('leaves the condition TRUE when the server is still down, so the next result retries', async () => {
		const { api, workspaceStore } = await load();

		const list = vi.spyOn(api.workspaces, 'list').mockRejectedValue(new Error('still down'));
		vi.spyOn(api.workspaces, 'get').mockRejectedValue(new Error('still down'));

		// Must not throw — a rejection here would take the sync subscriber's
		// other consumers down with it.
		await expect(workspaceStore.recoverIfMissing('alpha')).resolves.toBeUndefined();
		expect(workspaceStore.workspaces).toEqual([]);
		expect(workspaceStore.current).toBeNull();

		// The retry is the whole mechanism: a second call must try again rather
		// than latch on the first failure the way the code it replaces did.
		list.mockResolvedValue([ws('alpha')]);
		vi.spyOn(api.workspaces, 'me').mockResolvedValue({} as never);
		await workspaceStore.recoverIfMissing('alpha');

		expect(workspaceStore.current?.slug).toBe('alpha');
	});

	it('re-points `current` when it names a DIFFERENT workspace than the one asked for', async () => {
		const { api, workspaceStore } = await load();

		vi.spyOn(api.workspaces, 'list').mockResolvedValue([ws('alpha'), ws('beta')]);
		vi.spyOn(api.workspaces, 'me').mockResolvedValue({} as never);
		await workspaceStore.loadAll();
		await workspaceStore.setCurrent('alpha');
		expect(workspaceStore.current?.slug).toBe('alpha');

		// Presence is not the property — being the RIGHT workspace is. A
		// `current`-is-null check alone would pass this and leave the layout
		// showing beta's route against alpha's identity.
		await workspaceStore.recoverIfMissing('beta');

		expect(workspaceStore.current?.slug).toBe('beta');
	});

	it('an OLDER overlapping load settling first does not clear the newer one’s join slot', async () => {
		const { api, workspaceStore } = await load();

		const settle: { resolve: (v: Workspace[]) => void; reject: (e: Error) => void }[] = [];
		const list = vi
			.spyOn(api.workspaces, 'list')
			.mockImplementation(
				() => new Promise<Workspace[]>((resolve, reject) => settle.push({ resolve, reject })),
			);
		vi.spyOn(api.workspaces, 'me').mockResolvedValue({} as never);

		const older = workspaceStore.loadAll();
		const newer = workspaceStore.loadAll();
		expect(list).toHaveBeenCalledTimes(2);

		// The OLDER request settles first, and it FAILS — which is what keeps
		// `workspaces` empty so the recovery below has something to recover.
		// The first draft of this leg resolved it successfully, which populated
		// the array, sent the recovery straight past its list branch, and made
		// the assertion pass against unconditional cleanup too: a fixture that
		// could not fail, caught by running it against the mutant.
		settle[0].reject(new Error('older attempt failed'));
		await older.catch(() => undefined);

		const recovering = workspaceStore.recoverIfMissing('alpha');

		// Still two. Unconditional cleanup would have let the older failure
		// clear the slot the NEWER load still owns (codex round 5), and this
		// would be a third request instead of a join — the race round 4 closed,
		// reintroduced by its own cleanup.
		expect(list).toHaveBeenCalledTimes(2);

		settle[1].resolve([ws('alpha')]);
		await Promise.all([newer, recovering]);
		expect(workspaceStore.current?.slug).toBe('alpha');
	});

	it('JOINS a list request already in flight, and still resolves `current` from it', async () => {
		const { api, workspaceStore } = await load();

		let release: (v: Workspace[]) => void = () => {};
		const list = vi
			.spyOn(api.workspaces, 'list')
			.mockImplementation(() => new Promise<Workspace[]>((r) => (release = r)));
		// The single-workspace fallback is DOWN. That is the discriminating
		// part: the first draft of `recoverIfMissing` skipped an in-flight list
		// and went straight to `setCurrent`, which — with `workspaces` still
		// empty — takes this fallback, and when it failed `current` stayed null
		// even though the list request succeeded moments later (codex round 4).
		const get = vi.spyOn(api.workspaces, 'get').mockRejectedValue(new Error('down'));
		vi.spyOn(api.workspaces, 'me').mockResolvedValue({} as never);

		const inFlight = workspaceStore.loadAll();
		const recovering = workspaceStore.recoverIfMissing('alpha');

		// One request, not two.
		expect(list).toHaveBeenCalledTimes(1);

		release([ws('alpha')]);
		await Promise.all([inFlight, recovering]);

		// And the outcome, which the call-count assertion alone cannot see:
		// `current` is resolved OUT OF THE JOINED LIST, so the dead fallback is
		// never needed.
		expect(workspaceStore.current?.slug).toBe('alpha');
		expect(get).not.toHaveBeenCalled();
	});
});
