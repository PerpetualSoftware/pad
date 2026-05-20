// BUG-1538 / TASK-1539 — Surface the server's `open_children` 409
// guard (IDEA-1494) in the web UI so users can see WHY a status change
// to a terminal value was rejected and decide whether to force.
//
// Server contract (see internal/server/handlers_items_open_children_guard.go):
//   HTTP 409 { error: { code: "open_children", message, details: {
//     open_children: [{ ref, title, status, collection_slug }],
//     hidden_blocker_count: number,
//     done_field: string,
//     attempted_value: string,
//   }}}
//
// The CLI exposes a `--force` flag for the same override. The web
// equivalent is `ItemUpdate.force = true` on the PATCH body
// (transport-only field consumed by the handler).
//
// This helper is the single place call sites delegate to when an
// item-PATCH catches an error. If the error is `open_children`, it
// prompts the user with a structured confirm and retries with
// force=true on confirmation. Returns the freshly-updated item on
// success (so callers can mirror their normal success path), or
// `null` when the user cancelled or the error was something else.
// On non-open_children errors it re-throws so the caller's existing
// error handling (toast, etc.) still fires.
//
// The prompt itself is rendered by the singleton <OpenChildrenDialog />
// mounted at the app shell. This helper publishes the request to the
// `openChildrenDialog` store and awaits the user's response; call sites
// remain dialog-free.

import { PadApiError } from '$lib/api/client';
import { openChildrenDialog } from '$lib/stores/openChildrenDialog.svelte';

export interface OpenChildEntry {
	ref: string;
	title: string;
	status: string;
	collection_slug: string;
}

export interface OpenChildrenDetails {
	open_children: OpenChildEntry[];
	hidden_blocker_count: number;
	done_field: string;
	attempted_value: string;
}

/**
 * Type-narrowing predicate. Returns true when `err` is a
 * `PadApiError` carrying the `open_children` code with a usable
 * details payload.
 */
export function isOpenChildrenError(
	err: unknown
): err is PadApiError & { details: OpenChildrenDetails } {
	if (!(err instanceof PadApiError)) return false;
	if (err.code !== 'open_children') return false;
	const d = err.details as Partial<OpenChildrenDetails> | undefined;
	return !!d && Array.isArray(d.open_children) && typeof d.hidden_blocker_count === 'number';
}

/**
 * Build the human-readable confirm message from a parsed details
 * payload. Mirrors the CLI's phrasing in `writeOpenChildrenError`
 * (server-side) for consistency.
 *
 * `parentRef` is the parent item's issue ref (e.g. "BUG-1536"). Pass
 * `item.ref || item.slug` from the call site.
 */
export function formatOpenChildrenPrompt(parentRef: string, d: OpenChildrenDetails): string {
	const visible = d.open_children.length;
	const hidden = d.hidden_blocker_count;
	const lines: string[] = [];
	if (visible > 0 && hidden > 0) {
		lines.push(
			`Cannot mark ${parentRef} ${d.attempted_value}: ${visible} open child${
				visible === 1 ? '' : 'ren'
			} still in a non-terminal state, plus ${hidden} additional you don't have access to.`
		);
	} else if (visible > 0) {
		lines.push(
			`Cannot mark ${parentRef} ${d.attempted_value}: ${visible} open child${
				visible === 1 ? '' : 'ren'
			} still in a non-terminal state.`
		);
	} else {
		lines.push(
			`Cannot mark ${parentRef} ${d.attempted_value}: blocked by ${hidden} open child${
				hidden === 1 ? '' : 'ren'
			} you don't have access to.`
		);
	}
	if (visible > 0) {
		lines.push('');
		lines.push('Blocking children:');
		for (const c of d.open_children) {
			lines.push(`  • ${c.ref} — ${c.title} (${c.status})`);
		}
	}
	lines.push('');
	lines.push('Override the guard and mark it anyway?');
	return lines.join('\n');
}

/**
 * Confirm-and-retry helper. Call sites pass:
 *   - `err`: the caught error from the original PATCH
 *   - `parentRef`: the item's issue ref for the prompt
 *   - `retryWithForce`: a callback that re-issues the same PATCH with
 *     `force: true` and returns the updated entity
 *
 * Returns the updated entity on confirm+success, `null` when the
 * error was open_children but the user cancelled, and re-throws for
 * any other error so the caller's existing toast/log path runs.
 *
 * Kept generic over `T` so call sites that get back a richer view
 * (e.g. the detail-page `Item`) preserve their type.
 */
export async function confirmOpenChildrenOrThrow<T>(
	err: unknown,
	parentRef: string,
	retryWithForce: () => Promise<T>
): Promise<T | null> {
	if (!isOpenChildrenError(err)) throw err;
	const proceed = await openChildrenDialog.request(parentRef, err.details);
	if (!proceed) return null;
	return await retryWithForce();
}
