/**
 * Deterministic user → hex colour for collab presence cursors
 * (TASK-1263, PLAN-1248).
 *
 * Same user ID always maps to the same colour across sessions / tabs /
 * devices, so a teammate's cursor doesn't visually rebrand on every
 * reconnect. The hash is intentionally simple — collisions across
 * a small workspace are tolerable and the alternative (colour
 * negotiation via awareness) adds protocol complexity for no payoff
 * at v1 scale.
 *
 * Output is `#rrggbb` rather than `hsl(...)` because y-tiptap's
 * default selectionRender appends a literal `70` (alpha hex) to the
 * user's colour string and only validates `#rrggbb` — see Codex
 * review round 1 [P2]. Computing the hue in HSL space and converting
 * to hex gives us the readability of a clamped saturation/lightness
 * band while staying inside y-tiptap's accepted format.
 *
 * Saturation/lightness are clamped to a band that's readable on both
 * light and dark editor backgrounds. The hue covers the full circle.
 */

const SATURATION = 0.65; // 0..1
const LIGHTNESS = 0.55; // 0..1

/**
 * djb2-ish 32-bit string hash. Stable, fast, and good enough for
 * spreading short user-ID strings across the hue circle. Returns a
 * non-negative 32-bit integer.
 */
function hashString(s: string): number {
	let h = 5381;
	for (let i = 0; i < s.length; i++) {
		// `<< 5` + add ≡ multiply by 33; XOR mixes the new char in.
		h = ((h << 5) + h) ^ s.charCodeAt(i);
	}
	// Force unsigned by masking to 32 bits.
	return h >>> 0;
}

/**
 * Convert HSL (h: 0..360, s/l: 0..1) → `#rrggbb`. Standard formula
 * (CSS Color Module 3, Annex A). Values are clamped at integer-byte
 * resolution which is what hex requires anyway.
 */
function hslToHex(h: number, s: number, l: number): string {
	const c = (1 - Math.abs(2 * l - 1)) * s;
	const hp = h / 60;
	const x = c * (1 - Math.abs((hp % 2) - 1));
	let r1 = 0, g1 = 0, b1 = 0;
	if (hp < 1) { r1 = c; g1 = x; }
	else if (hp < 2) { r1 = x; g1 = c; }
	else if (hp < 3) { g1 = c; b1 = x; }
	else if (hp < 4) { g1 = x; b1 = c; }
	else if (hp < 5) { r1 = x; b1 = c; }
	else { r1 = c; b1 = x; }
	const m = l - c / 2;
	const toByte = (v: number) =>
		Math.max(0, Math.min(255, Math.round((v + m) * 255)))
			.toString(16)
			.padStart(2, '0');
	return `#${toByte(r1)}${toByte(g1)}${toByte(b1)}`;
}

/**
 * Map a user identifier (typically `user.id`) to a `#rrggbb` colour
 * string suitable for the `color` field on the Tiptap
 * CollaborationCaret extension. The output is always exactly 7
 * characters (`#` + 6 hex digits), which is what y-tiptap's default
 * selectionRender expects when it appends an alpha byte.
 *
 * Guaranteed: `userColor(x) === userColor(x)` for any string `x`.
 *
 * Empty / null / undefined inputs fall back to a neutral grey rather
 * than throwing — better UX than a coloured cursor for "anonymous"
 * presence states (e.g. mid-auth).
 */
export function userColor(userID: string | null | undefined): string {
	if (!userID) return '#999999';
	const hue = hashString(userID) % 360;
	return hslToHex(hue, SATURATION, LIGHTNESS);
}
