// The workspace the desktop shell shows outside a workspace route (TASK-4545):
// the last one visited this session, else the first open tab, else the first
// workspace. lastWorkspace is in memory only, so after a reload the open tabs
// are what says which workspace the user was working in.
import type { Workspace, WorkspaceTab } from '$lib/types';
import type { LastWorkspace } from './lastWorkspace.svelte';

type Ref = { slug: string; owner_username: string } | null | undefined;

const same = (w: Workspace, ref: Ref) => !!ref && w.slug === ref.slug && w.owner_username === ref.owner_username;

export function pickShellWorkspace(
	workspaces: Workspace[] | null | undefined,
	last: LastWorkspace | null | undefined,
	tabs: WorkspaceTab[] | null | undefined
): Workspace | undefined {
	const list = workspaces ?? [];
	return (
		list.find((w) => same(w, last)) ??
		(tabs ?? []).map((t) => list.find((w) => same(w, t))).find((w) => !!w) ??
		list[0]
	);
}
