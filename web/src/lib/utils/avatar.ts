/**
 * Deterministic avatar color + initial for workspace/user avatars.
 *
 * Same hash + palette the TopBar used inline (wsColor), lifted to a shared
 * util so the mobile Workspace/You sheets render identical avatars (PLAN-1694).
 */

const PALETTE = [
	'#4a9eff',
	'#4ade80',
	'#a78bfa',
	'#fbbf24',
	'#22d3ee',
	'#fb923c',
	'#f472b6',
	'#34d399'
];

/** Stable color for a name/label, picked from the shared palette. */
export function avatarColor(name: string | undefined | null): string {
	const s = name ?? '';
	let hash = 0;
	for (let i = 0; i < s.length; i++) {
		hash = s.charCodeAt(i) + ((hash << 5) - hash);
	}
	return PALETTE[Math.abs(hash) % PALETTE.length];
}

/** First character, uppercased, for the avatar glyph. */
export function avatarInitial(name: string | undefined | null): string {
	return (name ?? '?').trim().charAt(0).toUpperCase() || '?';
}
