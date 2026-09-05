/**
 * Canonical off-property links (IDEA-2711 / GitHub #1168).
 *
 * ONE source of truth for the project's own URLs. The repo URL in particular
 * was already written twice in `UserMenuResources.svelte` (the Cloud and
 * self-hosted lists) before the sidebar wanted a third; a URL copied into every
 * surface that shows it is a rename waiting to go half-applied.
 *
 * The visual contract these serve — link-list canonical order and the
 * external-link convention — lives in `docs/brand.md` §6 / §7. This module
 * holds the addresses only; ordering stays with the surface that renders a
 * list, because the two lists are deliberately different lengths.
 */

/** The public repository. Shown in the app so a user never has to search for it. */
export const GITHUB_REPO_URL = 'https://github.com/PerpetualSoftware/pad';

/**
 * Community channel: the repo's GitHub Discussions tab (TASK-2888 — Dave's
 * day-57 call: no Discord; Discussions, or r/getpad, is the channel for now).
 * Project-wide, so shown on Cloud and self-hosted alike.
 */
export const COMMUNITY_URL = `${GITHUB_REPO_URL}/discussions`;

/** Canonical project documentation — the same for Cloud and self-hosted. */
export const DOCS_URL = 'https://getpad.dev/docs';

/** Cloud-only surfaces. Self-hosted operators have their own, if any. */
export const CHANGELOG_URL = 'https://getpad.dev/changelog';
export const STATUS_URL = 'https://status.getpad.dev';
export const SUPPORT_MAILTO = 'mailto:support@getpad.dev';
