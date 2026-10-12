// BUG-3614: the settings page offered the workspace bundle download to
// editors, and the server answered 403. canExportWorkspace mirrors the
// server's two checks: the owner role, and unrestricted collection access.
import { describe, it, expect } from 'vitest';
import { canExportWorkspace } from './permissions';
import type { WorkspaceMembership } from '$lib/types';

function member(
	role: WorkspaceMembership['role'],
	collection_access: WorkspaceMembership['collection_access']
): WorkspaceMembership {
	return {
		role,
		collection_access,
		visible_collection_ids: [],
		full_access_collection_ids: [],
		collection_grants: []
	} as unknown as WorkspaceMembership;
}

describe('canExportWorkspace', () => {
	it('allows an owner with access to every collection (control)', () => {
		expect(canExportWorkspace(member('owner', 'all'))).toBe(true);
	});

	it('allows an owner whose membership carries no collection_access (absent means all)', () => {
		const m = member('owner', 'all') as unknown as Record<string, unknown>;
		delete m.collection_access;
		expect(canExportWorkspace(m as unknown as WorkspaceMembership)).toBe(true);
	});

	it('refuses an editor, whom the server answers 403', () => {
		expect(canExportWorkspace(member('editor', 'all'))).toBe(false);
	});

	it('refuses an owner scoped to specific collections (BUG-1922)', () => {
		expect(canExportWorkspace(member('owner', 'specific'))).toBe(false);
	});

	it('refuses viewers, guests and an unknown membership', () => {
		expect(canExportWorkspace(member('viewer', 'all'))).toBe(false);
		expect(canExportWorkspace(member('guest', 'specific'))).toBe(false);
		expect(canExportWorkspace(null)).toBe(false);
	});
});
