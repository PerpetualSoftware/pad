// In-app tutorials (TASK-3452): the catalog (Pad Cloud only) and the user's
// dismissed suggestions, each loaded once per page load and shared by every
// surface that shows a tutorial.
//
// Self-hosted never asks for the catalog: the server answers 404 there, and
// every surface links to getpad.dev/learn instead (a self-hoster's instance
// makes no third-party calls, and neither does its UI before a click).

import { api } from '$lib/api/client';
import { authStore } from '$lib/stores/auth.svelte';
import { LEARN_URL } from '$lib/brand/links';
import type { TutorialEntry, TutorialsResponse, UIDismissalKey } from '$lib/types';

let catalog = $state<TutorialsResponse | null>(null);
let catalogLoading: Promise<void> | null = null;
let dismissed = $state<Set<UIDismissalKey> | null>(null);
let dismissedLoading: Promise<void> | null = null;

export const tutorialsStore = {
	/** The catalog, or null before it loads, on self-hosted, or on failure. */
	get catalog(): TutorialsResponse | null {
		return catalog?.available ? catalog : null;
	},

	/** null until loaded: callers render nothing dismissible until then. */
	get dismissed(): Set<UIDismissalKey> | null {
		return dismissed;
	},

	loadCatalog(): Promise<void> {
		if (!authStore.cloudMode) return Promise.resolve();
		// Started inside a promise so that nothing, not even a synchronous throw,
		// escapes into the caller's onMount: a tutorial card must never break
		// the page that hosts it.
		catalogLoading ??= Promise.resolve()
			.then(() => api.tutorials.list())
			.then((c) => {
				catalog = c;
			})
			.catch(() => {
				catalog = null;
			});
		return catalogLoading;
	},

	loadDismissed(): Promise<void> {
		dismissedLoading ??= Promise.resolve()
			.then(() => api.uiDismissals.list())
			.then((r) => {
				dismissed = new Set(r.dismissed);
			})
			.catch(() => {
				// Unknown state: treat as nothing dismissed is the nagging
				// failure, so show nothing instead.
				dismissed = null;
			});
		return dismissedLoading;
	},

	async dismiss(key: UIDismissalKey): Promise<void> {
		dismissed = new Set([...(dismissed ?? []), key]);
		try {
			const r = await api.uiDismissals.dismiss(key);
			// Merge, never replace: dismissals only grow, and an older answer
			// arriving after a newer one must not bring a card back (codex r1).
			dismissed = new Set([...(dismissed ?? []), ...r.dismissed]);
		} catch {
			// Keep it hidden for this page load; the next load asks again.
		}
	},

	bySlug(slug: string): TutorialEntry | undefined {
		return catalog?.tutorials.find((t) => t.slug === slug) ?? catalog?.demos.find((t) => t.slug === slug);
	}
};

/** The tutorial's page on getpad.dev: the self-hosted target, and Cloud's fallback. */
export function learnUrl(slug?: string): string {
	return slug ? `${LEARN_URL}/${encodeURIComponent(slug)}` : LEARN_URL;
}

/** "2:29" for 148.6 seconds. */
export function formatDuration(seconds: number): string {
	const s = Math.round(seconds);
	return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
}

/** A chapter start, floored like YouTube's chapter list: "0:12" for 12.9. */
export function formatTimestamp(seconds: number): string {
	const s = Math.floor(seconds);
	return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
}
