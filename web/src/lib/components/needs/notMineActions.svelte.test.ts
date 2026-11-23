import { beforeEach, describe, expect, it, vi } from 'vitest';

// TASK-4686: the not-mine / give-to / undo flows shared by /needs and Home.
const h = vi.hoisted(() => ({
	calls: [] as string[],
	notMine: vi.fn(),
	undoNotMine: vi.fn(),
	itemGet: vi.fn(),
	itemUpdate: vi.fn(),
	rolesList: vi.fn(),
	membersList: vi.fn()
}));

vi.mock('$lib/api/client', () => ({
	api: {
		needs: {
			notMine: (ws: string, ref: string, body: unknown) => (h.calls.push('PUT not-mine'), h.notMine(ws, ref, body)),
			undoNotMine: (ws: string, ref: string) => (h.calls.push('DELETE not-mine'), h.undoNotMine(ws, ref))
		},
		items: {
			get: (ws: string, ref: string) => (h.calls.push('GET item'), h.itemGet(ws, ref)),
			update: (ws: string, ref: string, body: unknown) => (h.calls.push('PATCH item'), h.itemUpdate(ws, ref, body))
		},
		agentRoles: { list: (ws: string) => h.rolesList(ws) },
		members: { list: (ws: string) => h.membersList(ws) }
	},
	isUpdateConflictError: (e: unknown) => (e as { code?: string } | null)?.code === 'update_conflict'
}));

import { createGiveTargetCache, giveTo, markNotMine, undoNotMine, type Gave } from './notMineActions';
import type { NeedsRow } from '$lib/types';

const marker = { seen_comment_at: '2026-10-01T10:00:00Z', seen_comment_ids: ['c1'], seen_transition_seq: 3 };
const row = (extra: Partial<NeedsRow['item']> = {}) =>
	({
		workspace: { slug: 'alpha', name: 'Alpha', owner_username: 'dave' },
		item: { ref: 'TASK-1', title: 'T', collection: 'Tasks', seq: 41, ...extra },
		why: { rule: 'assigned', text: 'It is assigned to you.' },
		because: [],
		waiting_since: '2026-10-01T00:00:00Z',
		marker,
		can_edit: true
	}) as unknown as NeedsRow;
const conflict = () => Object.assign(new Error('conflict'), { code: 'update_conflict' });
const PREV = { assigned_user_id: 'u-me', agent_role_id: 'role-old', seq: 44 };
const role = { kind: 'role' as const, id: 'role-rev', label: 'Reviewer' };
const person = { kind: 'user' as const, id: 'u-bob', label: 'Bob Person' };

beforeEach(() => {
	for (const f of [h.notMine, h.undoNotMine, h.itemGet, h.itemUpdate, h.rolesList, h.membersList]) f.mockReset();
	h.calls.length = 0;
});

describe('markNotMine', () => {
	it('sends the row marker verbatim', async () => {
		h.notMine.mockResolvedValue({});
		await markNotMine(row());
		expect(h.notMine).toHaveBeenCalledWith('alpha', 'TASK-1', { marker });
	});
});

describe('giveTo', () => {
	it('sends the PATCH with the row seq, not the seq the GET read', async () => {
		h.itemGet.mockResolvedValue(PREV);
		h.itemUpdate.mockResolvedValue({ ...PREV, assigned_user_id: 'u-bob', seq: 45 });
		h.notMine.mockResolvedValue({});
		const res = await giveTo(row(), person);
		expect(h.calls).toEqual(['GET item', 'PATCH item', 'PUT not-mine']);
		expect(h.itemUpdate.mock.calls[0]).toEqual(['alpha', 'TASK-1', { assigned_user_id: 'u-bob', expected_seq: 41 }]);
		expect(h.notMine.mock.calls[0][2]).toStrictEqual({ marker });
		expect(res).toEqual({ ok: true, gave: { kind: 'user', prev: PREV, patched: { ...PREV, assigned_user_id: 'u-bob', seq: 45 } } });
	});

	it('a role target clears the assigned user', async () => {
		h.itemGet.mockResolvedValue(PREV);
		h.itemUpdate.mockResolvedValue({ ...PREV, seq: 45 });
		h.notMine.mockResolvedValue({});
		await giveTo(row(), role);
		expect(h.itemUpdate.mock.calls[0][2]).toStrictEqual({ agent_role_id: 'role-rev', clear_assigned_user: true, expected_seq: 41 });
	});

	it('a mark failure after the PATCH returns stage mark with the reassignment named', async () => {
		h.itemGet.mockResolvedValue(PREV);
		h.itemUpdate.mockResolvedValue({ ...PREV, seq: 45 });
		h.notMine.mockRejectedValue(new Error('boom'));
		const res = await giveTo(row(), person);
		expect(res).toMatchObject({ ok: false, stage: 'mark', message: 'Gave TASK-1 to Bob Person, but marking it not mine failed: boom' });
	});

	it('a 409 on the PATCH returns conflict and never calls notMine', async () => {
		h.itemGet.mockResolvedValue(PREV);
		h.itemUpdate.mockRejectedValue(conflict());
		const res = await giveTo(row(), person);
		expect(h.notMine).not.toHaveBeenCalled();
		expect(res).toMatchObject({
			ok: false,
			stage: 'reassign',
			conflict: true,
			message: 'The item changed since you loaded the list. Reload and try again.'
		});
	});

	it('a failed GET writes nothing', async () => {
		h.itemGet.mockRejectedValue(new Error('offline'));
		const res = await giveTo(row(), person);
		expect(h.calls).toEqual(['GET item']);
		expect(res).toMatchObject({ ok: false, stage: 'reassign', conflict: false, message: 'offline' });
	});
});

describe('undoNotMine', () => {
	const gave = (kind: Gave['kind'], prev: Record<string, unknown>): Gave =>
		({ kind, prev, patched: { seq: 42 } }) as unknown as Gave;

	it('a plain not mine is undone by the DELETE alone', async () => {
		h.undoNotMine.mockResolvedValue({});
		expect(await undoNotMine(row())).toEqual({ ok: true });
		expect(h.calls).toEqual(['DELETE not-mine']);
	});

	it('a failed DELETE reports undo failed and patches nothing', async () => {
		h.undoNotMine.mockRejectedValue(new Error('down'));
		expect(await undoNotMine(row(), gave('user', PREV))).toEqual({ ok: false, message: 'Could not undo TASK-1: down' });
		expect(h.itemUpdate).not.toHaveBeenCalled();
	});

	it('a role give reverts role and person, guarded by the give PATCH seq', async () => {
		h.undoNotMine.mockResolvedValue({});
		h.itemUpdate.mockResolvedValue({});
		await undoNotMine(row(), gave('role', PREV));
		expect(h.calls).toEqual(['DELETE not-mine', 'PATCH item']);
		expect(h.itemUpdate.mock.calls[0][2]).toStrictEqual({ agent_role_id: 'role-old', assigned_user_id: 'u-me', expected_seq: 42 });
	});

	it('a person give reverts only the person column', async () => {
		h.undoNotMine.mockResolvedValue({});
		h.itemUpdate.mockResolvedValue({});
		await undoNotMine(row(), gave('user', { assigned_user_id: null, agent_role_id: 'role-old' }));
		expect(h.itemUpdate.mock.calls[0][2]).toStrictEqual({ clear_assigned_user: true, expected_seq: 42 });
	});

	it('a role give onto an item with no role clears the role', async () => {
		h.undoNotMine.mockResolvedValue({});
		h.itemUpdate.mockResolvedValue({});
		await undoNotMine(row(), gave('role', { assigned_user_id: 'u-me', agent_role_id: null }));
		expect(h.itemUpdate.mock.calls[0][2]).toStrictEqual({ clear_agent_role: true, assigned_user_id: 'u-me', expected_seq: 42 });
	});

	it('a 409 on the revert keeps the reassignment and says so', async () => {
		h.undoNotMine.mockResolvedValue({});
		h.itemUpdate.mockRejectedValue(conflict());
		expect(await undoNotMine(row(), gave('user', PREV))).toEqual({
			ok: true,
			notice: { message: 'The item changed since; the reassignment was kept.', type: 'info' }
		});
	});

	it('another revert error keeps the reassignment as an error', async () => {
		h.undoNotMine.mockResolvedValue({});
		h.itemUpdate.mockRejectedValue(new Error('nope'));
		expect(await undoNotMine(row(), gave('user', PREV))).toEqual({
			ok: true,
			notice: { message: 'TASK-1 is no longer marked not mine, but the reassignment was kept: nope', type: 'error' }
		});
	});
});

describe('createGiveTargetCache', () => {
	const flush = async () => {
		for (let i = 0; i < 5; i++) await Promise.resolve();
	};

	it('loads a workspace once and keeps a complete load', async () => {
		h.rolesList.mockResolvedValue([]);
		h.membersList.mockResolvedValue({ members: [] });
		const load = createGiveTargetCache();
		await load('alpha');
		await flush();
		await load('alpha');
		expect(h.rolesList).toHaveBeenCalledTimes(1);
	});

	it.each([
		['roles', () => (h.rolesList.mockRejectedValue(new Error('x')), h.membersList.mockResolvedValue({ members: [] }))],
		['people', () => (h.rolesList.mockResolvedValue([]), h.membersList.mockRejectedValue(new Error('x')))]
	])('evicts a load whose %s half failed', async (_half, arrange) => {
		arrange();
		const load = createGiveTargetCache();
		await load('alpha');
		await flush();
		await load('alpha');
		expect(h.rolesList).toHaveBeenCalledTimes(2);
	});

	it('each cache is its own', async () => {
		h.rolesList.mockResolvedValue([]);
		h.membersList.mockResolvedValue({ members: [] });
		await createGiveTargetCache()('alpha');
		await createGiveTargetCache()('alpha');
		expect(h.rolesList).toHaveBeenCalledTimes(2);
	});
});
