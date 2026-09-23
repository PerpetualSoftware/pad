/**
 * The icon a role shows, with the robot fallback for a role created without
 * one (BUG-3161). The fallback is the CHARACTER, not an HTML entity: the lane
 * header renders it through a Svelte text interpolation, which escapes its
 * string, so '&#129302;' printed as those nine characters. The sibling
 * fallback for collections (roles/+page.svelte, `coll.icon || '📦'`) was
 * already the character.
 */
export const DEFAULT_ROLE_ICON = '🤖';

export function roleIcon(icon: string | null | undefined): string {
	return icon || DEFAULT_ROLE_ICON;
}
