import { describe, it, expect } from 'vitest';
import { pickShellWorkspace } from './shellWorkspace';
import type { Workspace, WorkspaceTab } from '$lib/types';

const ws = (slug: string, owner = 'u') => ({ id: slug, slug, name: slug, owner_username: owner }) as unknown as Workspace;
const tab = (slug: string, owner = 'u') => ({ slug, name: slug, owner_username: owner, is_guest: false, position: 0, ephemeral: false }) as WorkspaceTab;
const list = [ws('alpha'), ws('beta'), ws('gamma'), ws('beta', 'other')];

describe('pickShellWorkspace (TASK-4545)', () => {
	it('prefers the last workspace visited', () => {
		expect(pickShellWorkspace(list, { slug: 'gamma', owner_username: 'u' }, [tab('beta')])?.slug).toBe('gamma');
	});
	it('falls back to the first open tab after a reload', () => {
		expect(pickShellWorkspace(list, null, [tab('beta', 'other'), tab('alpha')])).toBe(list[3]);
	});
	it('skips a tab with no matching workspace', () => {
		expect(pickShellWorkspace(list, null, [tab('gone'), tab('gamma')])?.slug).toBe('gamma');
	});
	it('falls back to the first workspace with no tabs', () => {
		expect(pickShellWorkspace(list, null, [])?.slug).toBe('alpha');
		expect(pickShellWorkspace([], null, [])).toBeUndefined();
	});
});
