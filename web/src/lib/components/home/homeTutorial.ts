import type { Workspace } from '$lib/types';

// TASK-4571: Home shows the old console's one-time tutorial suggestion to a
// user who owns no workspace (a guest-only user included), and only once the
// workspace list has loaded: never while it loads or after it failed.
export function showHomeTutorial(
	loaded: boolean,
	workspaces: Pick<Workspace, 'owner_id'>[],
	userId: string | null | undefined
): boolean {
	if (!loaded || !userId) return false;
	return !workspaces.some((ws) => ws.owner_id === userId);
}
