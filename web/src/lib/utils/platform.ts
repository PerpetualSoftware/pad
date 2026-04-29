/**
 * OS detection helpers. Used by ConnectWorkspaceModal to pick a sensible
 * default install tab based on the user's machine. SSR returns "macos" by
 * default since that's the most common dev environment for Pad's audience.
 */

export type Platform = 'macos' | 'linux' | 'windows' | 'other';
export type InstallTab = 'macos' | 'linux' | 'windows' | 'docker';

/**
 * Best-effort platform detection from `navigator.userAgent` with
 * `navigator.platform` as a fallback signal. Pure function — safe to call
 * during render. Returns "macos" on the server (no `navigator`).
 */
export function detectPlatform(): Platform {
	if (typeof navigator === 'undefined') return 'macos';

	const ua = (navigator.userAgent || '').toLowerCase();
	const plat = ((navigator as Navigator & { platform?: string }).platform || '').toLowerCase();
	const haystack = ua + ' ' + plat;

	if (haystack.includes('mac')) return 'macos';
	if (haystack.includes('win')) return 'windows';
	if (haystack.includes('linux') || haystack.includes('x11')) return 'linux';
	return 'other';
}

/**
 * Maps the detected platform to the install tab we want to show first.
 * "other" maps to "macos" since that's the most common dev case.
 */
export function defaultInstallTab(): InstallTab {
	const p = detectPlatform();
	if (p === 'other') return 'macos';
	return p;
}
