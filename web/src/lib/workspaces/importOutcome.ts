import type { ImportOutcome } from '$lib/types';
import { ImportTransportError } from '$lib/api/importUpload';

// BUG-3475: when an import's response never arrives, ask the server what
// became of it (GET /workspaces/import-status) and say exactly that.
//
// The poll outlasts the server's 60s per-Read idle window: a stalled upload
// the client gave up on is still "running" there until that window closes,
// and only then rolled back. A key the server does not hold is UNKNOWN, never
// "nothing was created" (lead ruling): it may have restarted, or the import
// may have been started somewhere this registry cannot see.

export const IMPORT_RESOLVE_WINDOW_MS = 75_000;
export const IMPORT_RESOLVE_INTERVAL_MS = 3_000;

/** A fresh key per attempt. getRandomValues, not randomUUID: the latter
 *  needs a secure context, and a self-hosted Pad on a LAN IP over http is not
 *  one. 32 hex characters, inside the server's 8-64 [A-Za-z0-9-]. */
export function newImportKey(): string {
	const bytes = new Uint8Array(16);
	crypto.getRandomValues(bytes);
	return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

/**
 * Poll until the attempt is no longer running or the window closes. Returns
 * the last outcome seen, or null when the server never knew the key or could
 * not be reached. `live` lets the caller stop polling for an operation nobody
 * is waiting on any more.
 */
export async function resolveImportOutcome(
	key: string,
	deps: {
		status: (key: string) => Promise<ImportOutcome | null>;
		live: () => boolean;
		sleep?: (ms: number) => Promise<void>;
		now?: () => number;
	}
): Promise<ImportOutcome | null> {
	const sleep = deps.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));
	const now = deps.now ?? (() => Date.now());
	const deadline = now() + IMPORT_RESOLVE_WINDOW_MS;
	let last: ImportOutcome | null = null;
	for (;;) {
		if (!deps.live()) return last;
		try {
			last = await deps.status(key);
			if (last && last.state !== 'running') return last;
		} catch {
			// Unreachable now is not an answer; keep asking until the window closes.
		}
		if (now() >= deadline) return last;
		await sleep(IMPORT_RESOLVE_INTERVAL_MS);
	}
}

export type ImportOutcomeView =
	| { kind: 'complete'; slug: string; name: string; owner: string; text: string }
	| { kind: 'kept'; slug: string; name: string; owner: string; text: string }
	| { kind: 'nothing'; text: string }
	| { kind: 'unknown'; text: string };

/** What the dialog says, from why the client stopped and what the server answered. */
export function describeImportOutcome(err: ImportTransportError, outcome: ImportOutcome | null): ImportOutcomeView {
	const why = err.message;
	if (outcome && outcome.workspace_slug && outcome.owner_username) {
		const name = outcome.workspace_name || outcome.workspace_slug;
		const ws = { slug: outcome.workspace_slug, name, owner: outcome.owner_username };
		if (outcome.state === 'complete') {
			return { kind: 'complete', ...ws, text: `The import finished: "${name}" is ready, even though the answer did not reach this page.` };
		}
		if (outcome.state === 'kept') {
			return { kind: 'kept', ...ws, text: `${why}, and the partial workspace "${name}" was kept. Open it to see what arrived, or delete it and import again.` };
		}
	}
	if (outcome && (outcome.state === 'removed' || outcome.state === 'not_created')) {
		return { kind: 'nothing', text: `${why}. Nothing was kept, so it is safe to try again.` };
	}
	return {
		kind: 'unknown',
		text: `${why}. The server could not confirm what happened, so check your workspace list before trying again.`
	};
}
