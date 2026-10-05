import type { AppInstallState } from '$lib/types';

/** An install state as the owner reads it (SPEC-6 U9a, TASK-3413). */
export function appStateLabel(state: AppInstallState | string): string {
	switch (state) {
		case 'active':
			return 'Active';
		case 'inactive':
			return 'Disabled';
		case 'disabling':
			return 'Disabling (unfinished)';
		case 'uninstalling':
			return 'Uninstalling (unfinished)';
		case 'uninstalled':
			return 'Uninstalled';
		default:
			return state;
	}
}

export function appStateColor(state: AppInstallState | string): string {
	switch (state) {
		case 'active':
			return 'var(--accent-green)';
		case 'disabling':
		case 'uninstalling':
			return 'var(--accent-amber)';
		case 'inactive':
		case 'uninstalled':
		default:
			return 'var(--accent-gray)';
	}
}
