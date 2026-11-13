// TASK-4515: an avatar's tint, a stable pick from the accent tokens (both themes define them),
// and its initial. Pure.
export const AVATAR_TONES = [
	'--accent-cyan',
	'--accent-orange',
	'--accent-green',
	'--accent-purple',
	'--accent-amber',
	'--accent-brown',
	'--accent-gray'
] as const;

export type AvatarTone = (typeof AVATAR_TONES)[number];

/** FNV-1a over the key's UTF-16 units: the same key is always the same tone. */
export function avatarTone(key: string): AvatarTone {
	let h = 0x811c9dc5;
	for (let i = 0; i < key.length; i++) {
		h ^= key.charCodeAt(i);
		h = Math.imul(h, 0x01000193);
	}
	return AVATAR_TONES[(h >>> 0) % AVATAR_TONES.length];
}

export function avatarInitial(key: string): string {
	const k = key.trim();
	return k ? Array.from(k)[0].toUpperCase() : '?';
}
