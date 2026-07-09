// contentSaver — the debounced raw-markdown content saver extracted from the
// item detail page ([collection]/[slug]/+page.svelte) as part of TASK-2029.
//
// It owns exactly three things that used to be inlined (and tangled) in the
// monolith's raw-mode save path:
//   1. the debounce timer that coalesces keystrokes into one PATCH,
//   2. the "pending markdown" dirty flag (was `rawPendingMarkdown`), and
//   3. flush-now — fire the pending save IMMEDIATELY, cancelling the debounce.
//      This is what the BUG-2024 keepalive-on-unload path and the
//      rich↔raw toggle drain (flushRawIfPending) both need.
//
// The actual PATCH (api.items.update + all the reactive item/saveStatus/
// editorStore bookkeeping + stale-response guards) stays in the page and is
// injected as the `save` callback so this module has no Svelte / API
// dependencies and is unit-testable in plain node.
//
// NOTE (CONVE-1688): `pending` is a plain closure variable, NOT `$state`.
// It is a handler-only dirty tracker — the page's *reactive* dirty flag lives
// in `editorStore`. A `$state` written inside the page's save $effect/handlers
// that an effect also read would silently wedge the effect scheduler in PROD.
// Keeping it a plain `let` is deliberate; this file is `.svelte.ts` for
// colocation with the item-page module conventions, but uses no runes.

export interface ContentSaverConfig {
	/** Debounce window in ms for queued keystroke saves (default 1200). */
	debounceMs?: number;
	/**
	 * Perform the actual content save. `keepalive` is true when flushing on
	 * page unload (fetch keepalive) or any other immediate flush that must
	 * outlive teardown. The return value is ignored (fire-and-forget on the
	 * unload path). The page owns all reactive bookkeeping inside this
	 * callback and clears the pending flag via `clearPending()` when the
	 * PATCH lands and no newer edit superseded it.
	 */
	save: (markdown: string, ctx: { keepalive: boolean }) => void | Promise<unknown>;
}

export interface ContentSaver {
	/**
	 * Queue a save for the given markdown: records it as the pending (dirty)
	 * content and (re)arms the debounce. Rapid successive calls coalesce —
	 * only the last markdown is saved when the debounce fires.
	 */
	queue(markdown: string): void;
	/**
	 * Fire the pending save immediately, cancelling any queued debounce so it
	 * can't fire a second, older-content PATCH. No-op (returns false) when
	 * there's nothing pending. Returns true when a save was dispatched.
	 */
	flushNow(opts?: { keepalive?: boolean }): boolean;
	/** Cancel the pending debounce without saving. Leaves the dirty flag set. */
	cancel(): void;
	/** Clear the pending (dirty) markdown — call once a save has landed. */
	clearPending(): void;
	/** The markdown awaiting a save, or null when clean. */
	readonly pending: string | null;
	/** True when there is unsaved pending markdown. */
	readonly dirty: boolean;
}

const DEFAULT_DEBOUNCE_MS = 1200;

export function createContentSaver(config: ContentSaverConfig): ContentSaver {
	const debounceMs = config.debounceMs ?? DEFAULT_DEBOUNCE_MS;
	let timer: ReturnType<typeof setTimeout> | undefined;
	let pending: string | null = null;

	function cancel(): void {
		if (timer !== undefined) {
			clearTimeout(timer);
			timer = undefined;
		}
	}

	function queue(markdown: string): void {
		cancel();
		pending = markdown;
		timer = setTimeout(() => {
			timer = undefined;
			void config.save(markdown, { keepalive: false });
		}, debounceMs);
	}

	function flushNow(opts: { keepalive?: boolean } = {}): boolean {
		cancel();
		if (pending === null) return false;
		void config.save(pending, { keepalive: opts.keepalive ?? false });
		return true;
	}

	function clearPending(): void {
		pending = null;
	}

	return {
		queue,
		flushNow,
		cancel,
		clearPending,
		get pending() {
			return pending;
		},
		get dirty() {
			return pending !== null;
		},
	};
}
